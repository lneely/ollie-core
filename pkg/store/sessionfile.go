package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"ollie/pkg/agent"
	"ollie/pkg/backend"
	olog "ollie/pkg/log"
)

// SessionFileList defines the fixed set of files in a session directory.
var SessionFileList = []struct {
	Name string
	Mode os.FileMode
}{
	{"ctl", 0200},
	{"prompt", 0200},
	{"fifo.in", 0200},
	{"fifo.out", 0444},
	{"chat", 0666},
	{"offset", 0444},
	{"state", 0444},
	{"statewait", 0444},
	{"backend", 0666},
	{"agent", 0666},
	{"model", 0666},
	{"cwd", 0666},
	{"cwdwait", 0444},
	{"usage", 0444},
	{"usagewait", 0444},
	{"ctxsz", 0444},
	{"ctxszwait", 0444},
	{"models", 0444},
	{"systemprompt", 0444},
	{"params", 0666},
	{"tail", 0555},
}

// SessionFileStore is a RunFileStore for a session directory.
type SessionFileStore = RunFileStore

func NewSessionFileStore(sess *Session, log *olog.Logger, kill func(), rename func(newID string) error, saveTranscript func([]byte) error) *SessionFileStore {
	h := &sessionHelper{sess: sess, log: log, kill: kill, rename: rename, saveTranscript: saveTranscript}
	specs := make([]FileSpec, len(SessionFileList))
	for i, f := range SessionFileList {
		specs[i] = h.fileSpec(f.Name, f.Mode)
	}
	return NewRunFileStore(sess, specs)
}

// sessionHelper holds the dependencies needed to build session FileSpecs.
type sessionHelper struct {
	sess           *Session
	log            *olog.Logger
	kill           func()
	rename         func(newID string) error
	saveTranscript func([]byte) error
}

func (h *sessionHelper) fileSpec(name string, mode os.FileMode) FileSpec {
	fs := FileSpec{Name: name, Mode: mode}

	// Read
	switch name {
	case "chat":
		fs.Read = func() ([]byte, error) {
			h.sess.mu.RLock()
			data := make([]byte, len(h.sess.log))
			copy(data, h.sess.log)
			h.sess.mu.RUnlock()
			return data, nil
		}
		fs.Size = func() int64 {
			h.sess.mu.RLock()
			n := len(h.sess.log)
			h.sess.mu.RUnlock()
			return int64(n)
		}
	case "fifo.out":
		fs.Read = func() ([]byte, error) {
			item, ok := h.sess.Core.PopQueue()
			if !ok {
				return nil, nil
			}
			return []byte(item), nil
		}
	default:
		fs.Read = func() ([]byte, error) { return []byte(h.content(name)), nil }
	}

	// Write
	switch name {
	case "chat":
		fs.Write = func(data []byte) error {
			input := strings.TrimSpace(string(data))
			if input == "" {
				return nil
			}
			return h.saveTranscript([]byte(input))
		}
	case "prompt":
		fs.Write = func(data []byte) error {
			input := strings.TrimSpace(string(data))
			if input == "" {
				return nil
			}
			pub := h.makePublish()
			h.sess.Core.Submit(h.sess.Ctx, input, pub)
			h.sess.EnsureTrailingNewline()
			return nil
		}
	case "fifo.in":
		fs.Write = func(data []byte) error {
			input := strings.TrimSpace(string(data))
			if input == "" {
				return nil
			}
			h.sess.Core.Queue(input)
			return nil
		}
	case "ctl":
		fs.Write = func(data []byte) error {
			input := strings.TrimSpace(string(data))
			if input == "" {
				return nil
			}
			return h.handleCtl(input)
		}
	case "backend", "model", "agent":
		fs.Write = func(data []byte) error {
			input := strings.TrimSpace(string(data))
			if input == "" {
				return nil
			}
			if h.sess.Core.IsRunning() {
				return fmt.Errorf("cannot switch %s while agent is running", name)
			}
			h.sess.Core.Submit(h.sess.Ctx, "/"+name+" "+input, h.makePublish())
			return nil
		}
	case "cwd":
		fs.Write = func(data []byte) error {
			input := strings.TrimSpace(string(data))
			if input == "" {
				return nil
			}
			return h.sess.Core.SetCWD(input)
		}
	case "params":
		fs.Write = func(data []byte) error {
			input := strings.TrimSpace(string(data))
			if input == "" {
				return nil
			}
			if h.sess.Core.IsRunning() {
				return fmt.Errorf("cannot change params while agent is running")
			}
			params, err := ParseParams(input, h.sess.Core.GenerationParams())
			if err != nil {
				return err
			}
			return h.sess.Core.SetGenerationParams(params)
		}
	}

	// Wait
	switch name {
	case "statewait", "usagewait", "ctxszwait", "cwdwait":
		var field string
		switch name {
		case "statewait":
			field = agent.WatchState
		case "usagewait":
			field = agent.WatchUsage
		case "ctxszwait":
			field = agent.WatchCtxSz
		case "cwdwait":
			field = agent.WatchCWD
		}
		fs.Wait = func(connCtx context.Context, base string) ([]byte, error) {
			ctx, cancel := context.WithCancel(connCtx)
			defer cancel()
			context.AfterFunc(h.sess.Ctx, cancel)
			if base == "" {
				base = h.currentWaitValue(name)
			}
			v, ok := h.sess.Core.WaitChange(ctx, field, base)
			if !ok {
				return nil, nil
			}
			return []byte(v + "\n"), nil
		}
	}

	return fs
}

func (h *sessionHelper) content(name string) string {
	h.sess.mu.RLock()
	defer h.sess.mu.RUnlock()
	switch name {
	case "backend":
		return h.sess.Core.BackendName() + "\n"
	case "agent":
		return h.sess.Core.AgentName() + "\n"
	case "model":
		return h.sess.Core.ModelName() + "\n"
	case "state":
		return h.sess.Core.State() + "\n"
	case "cwd":
		return h.sess.Core.CWD() + "\n"
	case "usage":
		return h.sess.Core.Usage() + "\n"
	case "ctxsz":
		return h.sess.Core.CtxSz() + "\n"
	case "models":
		return h.sess.Core.ListModels() + "\n"
	case "systemprompt":
		return h.sess.Core.SystemPrompt()
	case "offset":
		return fmt.Sprintf("%d\n", h.sess.ChatOffset)
	case "params":
		return FormatParams(h.sess.Core.GenerationParams())
	case "tail":
		return "#!/bin/sh\nexec tail -f \"$(dirname \"$0\")/chat\"\n"
	}
	return ""
}

func (h *sessionHelper) currentWaitValue(name string) string {
	switch name {
	case "statewait":
		return h.sess.Core.State()
	case "usagewait":
		return h.sess.Core.Usage()
	case "ctxszwait":
		return h.sess.Core.CtxSz()
	case "cwdwait":
		return h.sess.Core.CWD()
	}
	return ""
}

func (h *sessionHelper) makePublish() func(agent.Event) {
	assistantStarted := false
	return func(ev agent.Event) {
		if ev.Role == "user" {
			if assistantStarted {
				h.sess.AppendLog([]byte("\n"))
				assistantStarted = false
			}
			h.sess.mu.Lock()
			h.sess.ChatOffset = len(h.sess.log)
			h.sess.mu.Unlock()
		} else {
			switch ev.Role {
			case "call", "tool":
				if assistantStarted {
					h.sess.AppendLog([]byte("\n"))
					assistantStarted = false
				}
			case "assistant":
				if !assistantStarted {
					h.sess.AppendLog([]byte("assistant: "))
					assistantStarted = true
				}
			}
		}
		h.sess.AppendLog(FormatEvent(ev))
	}
}

func (h *sessionHelper) handleCtl(input string) error {
	cmd := strings.Fields(input)
	if len(cmd) == 0 {
		return fmt.Errorf("empty ctl command")
	}
	switch cmd[0] {
	case "stop":
		h.sess.Core.Interrupt(agent.ErrInterrupted)
	case "kill":
		h.kill()
	case "rn":
		if name := strings.TrimSpace(input[3:]); name != "" {
			if err := h.rename(name); err != nil {
				h.log.Error("rename: %v", err)
			}
		}
	case "save":
		h.sess.mu.RLock()
		data := make([]byte, len(h.sess.log))
		copy(data, h.sess.log)
		h.sess.mu.RUnlock()
		return h.saveTranscript(data)
	case "compact", "clear", "backend", "model", "models",
		"agents", "agent", "sessions", "cwd", "skills",
		"tools", "context", "usage", "history",
		"irw", "help":
		h.sess.Core.Submit(h.sess.Ctx, "/"+input, h.makePublish())
	default:
		return fmt.Errorf("unknown ctl command: %s", cmd[0])
	}
	return nil
}

// FormatParams formats generation parameters as key=value lines.
func FormatParams(p backend.GenerationParams) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "maxTokens=%d\n", p.MaxTokens)
	if p.Temperature != nil {
		fmt.Fprintf(&sb, "temperature=%g\n", *p.Temperature)
	} else {
		fmt.Fprintf(&sb, "temperature=\n")
	}
	if p.FrequencyPenalty != nil {
		fmt.Fprintf(&sb, "frequencyPenalty=%g\n", *p.FrequencyPenalty)
	} else {
		fmt.Fprintf(&sb, "frequencyPenalty=\n")
	}
	if p.PresencePenalty != nil {
		fmt.Fprintf(&sb, "presencePenalty=%g\n", *p.PresencePenalty)
	} else {
		fmt.Fprintf(&sb, "presencePenalty=\n")
	}
	return sb.String()
}

// ParseParams parses key=value lines into generation parameters.
func ParseParams(input string, current backend.GenerationParams) (backend.GenerationParams, error) {
	p := current
	for _, line := range strings.Split(input, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "maxTokens":
			if v == "" {
				p.MaxTokens = 0
			} else {
				n, err := strconv.Atoi(v)
				if err != nil {
					return p, fmt.Errorf("invalid maxTokens: %s", v)
				}
				p.MaxTokens = n
			}
		case "temperature":
			if v == "" {
				p.Temperature = nil
			} else {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return p, fmt.Errorf("invalid temperature: %s", v)
				}
				p.Temperature = &f
			}
		case "frequencyPenalty":
			if v == "" {
				p.FrequencyPenalty = nil
			} else {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return p, fmt.Errorf("invalid frequencyPenalty: %s", v)
				}
				p.FrequencyPenalty = &f
			}
		case "presencePenalty":
			if v == "" {
				p.PresencePenalty = nil
			} else {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return p, fmt.Errorf("invalid presencePenalty: %s", v)
				}
				p.PresencePenalty = &f
			}
		}
	}
	return p, nil
}
