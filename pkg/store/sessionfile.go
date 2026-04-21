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
}

// SessionFileStore implements Store for the files within a single
// session directory. The file set is fixed.
type SessionFileStore struct {
	*storeConfig
	Runnable
	sess           *Session
	log            *olog.Logger
	kill           func()
	rename         func(newID string) error
	saveTranscript func([]byte) error
}

func NewSessionFileStore(sess *Session, log *olog.Logger, kill func(), rename func(newID string) error, saveTranscript func([]byte) error) *SessionFileStore {
	sf := &SessionFileStore{Runnable: sess, sess: sess, log: log, kill: kill, rename: rename, saveTranscript: saveTranscript}
	notSupported := func(string) error { return fmt.Errorf("not supported") }
	sf.storeConfig = &storeConfig{
		StatFn:   sf.stat,
		ListFn:   sf.list,
		OpenFn:   sf.open,
		DeleteFn: notSupported,
		CreateFn: notSupported,
		RenameFn: func(string, string) error { return fmt.Errorf("not supported") },
	}
	return sf
}

func (s *SessionFileStore) list() ([]os.DirEntry, error) {
	entries := make([]os.DirEntry, len(SessionFileList))
	for i, f := range SessionFileList {
		entries[i] = FileEntry(f.Name, f.Mode)
	}
	return entries, nil
}

func (s *SessionFileStore) stat(name string) (os.FileInfo, error) {
	for _, f := range SessionFileList {
		if f.Name == name {
			var size int64
			switch name {
			case "chat":
				s.sess.mu.RLock()
				size = int64(len(s.sess.log))
				s.sess.mu.RUnlock()
			case "statewait", "usagewait", "ctxszwait", "cwdwait":
			default:
				size = int64(len(s.content(name)))
			}
			return &SyntheticFileInfo{Name_: name, Mode_: f.Mode, Size_: size}, nil
		}
	}
	return nil, fmt.Errorf("%s: not found", name)
}

func (s *SessionFileStore) open(name string) (StoreEntry, error) {
	for _, f := range SessionFileList {
		if f.Name == name {
			return s.entryFor(name, f.Mode), nil
		}
	}
	return nil, fmt.Errorf("%s: not found", name)
}

func (s *SessionFileStore) entryFor(name string, mode os.FileMode) StoreEntry {
	return &EntryConfig{
		StatFn: func() (os.FileInfo, error) {
			var size int64
			switch name {
			case "chat":
				s.sess.mu.RLock()
				size = int64(len(s.sess.log))
				s.sess.mu.RUnlock()
			case "statewait", "usagewait", "ctxszwait", "cwdwait":
				// Blocking reads; size is unknown until resolved.
			default:
				size = int64(len(s.content(name)))
			}
			return &SyntheticFileInfo{Name_: name, Mode_: mode, Size_: size}, nil
		},
		ReadFn:         func() ([]byte, error) { return s.readFile(name) },
		WriteFn:        func(data []byte) error { return s.writeFile(name, data) },
		BlockingReadFn: func(ctx context.Context, base string) ([]byte, error) { return s.blockingRead(ctx, name, base) },
	}
}

func (s *SessionFileStore) readFile(name string) ([]byte, error) {
	switch name {
	case "chat":
		s.sess.mu.RLock()
		data := make([]byte, len(s.sess.log))
		copy(data, s.sess.log)
		s.sess.mu.RUnlock()
		return data, nil
	case "offset":
		return []byte(s.content("offset")), nil
	case "fifo.out":
		item, ok := s.sess.Core.PopQueue()
		if !ok {
			return nil, nil
		}
		return []byte(item), nil
	default:
		for _, f := range SessionFileList {
			if f.Name == name {
				return []byte(s.content(name)), nil
			}
		}
		return nil, fmt.Errorf("%s: not found", name)
	}
}

func (s *SessionFileStore) writeFile(name string, data []byte) error {
	input := strings.TrimSpace(string(data))
	if input == "" {
		return nil
	}
	switch name {
	case "chat":
		return s.saveTranscript([]byte(input))
	case "prompt":
		s.sess.Core.Submit(s.sess.Ctx, input, s.makePublish())
	case "fifo.in":
		s.sess.Core.Queue(input)
	case "ctl":
		return s.handleCtl(input)
	case "backend":
		if s.sess.Core.IsRunning() {
			return fmt.Errorf("cannot switch backend while agent is running")
		}
		s.sess.Core.Submit(s.sess.Ctx, "/backend "+input, s.makePublish())
	case "agent":
		if s.sess.Core.IsRunning() {
			return fmt.Errorf("cannot switch agent while agent is running")
		}
		s.sess.Core.Submit(s.sess.Ctx, "/agent "+input, s.makePublish())
	case "model":
		if s.sess.Core.IsRunning() {
			return fmt.Errorf("cannot switch model while agent is running")
		}
		s.sess.Core.Submit(s.sess.Ctx, "/model "+input, s.makePublish())
	case "cwd":
		if err := s.sess.Core.SetCWD(input); err != nil {
			return err
		}
	case "params":
		if s.sess.Core.IsRunning() {
			return fmt.Errorf("cannot change params while agent is running")
		}
		params, err := ParseParams(input, s.sess.Core.GenerationParams())
		if err != nil {
			return err
		}
		return s.sess.Core.SetGenerationParams(params)
	}
	return nil
}

func (s *SessionFileStore) blockingRead(connCtx context.Context, name, base string) ([]byte, error) {
	ctx, cancel := context.WithCancel(connCtx)
	defer cancel()
	context.AfterFunc(s.sess.Ctx, cancel)

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
	default:
		return nil, fmt.Errorf("%s: not a wait file", name)
	}

	if base == "" {
		base = s.currentWaitValue(name)
	}

	v, ok := s.sess.Core.WaitChange(ctx, field, base)
	if !ok {
		return nil, nil
	}
	return []byte(v + "\n"), nil
}

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

func (s *SessionFileStore) content(name string) string {
	s.sess.mu.RLock()
	defer s.sess.mu.RUnlock()
	switch name {
	case "backend":
		return s.sess.Core.BackendName() + "\n"
	case "agent":
		return s.sess.Core.AgentName() + "\n"
	case "model":
		return s.sess.Core.ModelName() + "\n"
	case "state":
		return s.sess.Core.State() + "\n"
	case "cwd":
		return s.sess.Core.CWD() + "\n"
	case "usage":
		return s.sess.Core.Usage() + "\n"
	case "ctxsz":
		return s.sess.Core.CtxSz() + "\n"
	case "models":
		return s.sess.Core.ListModels() + "\n"
	case "systemprompt":
		return s.sess.Core.SystemPrompt()
	case "offset":
		return fmt.Sprintf("%d\n", s.sess.ChatOffset)
	case "params":
		return FormatParams(s.sess.Core.GenerationParams())
	}
	return ""
}

// currentWaitValue returns the current value for the named *wait file.
func (s *SessionFileStore) currentWaitValue(name string) string {
	switch name {
	case "statewait":
		return s.sess.Core.State()
	case "usagewait":
		return s.sess.Core.Usage()
	case "ctxszwait":
		return s.sess.Core.CtxSz()
	case "cwdwait":
		return s.sess.Core.CWD()
	}
	return ""
}

func (s *SessionFileStore) makePublish() func(agent.Event) {
	assistantStarted := false
	return func(ev agent.Event) {
		switch ev.Role {
		case "user", "call", "tool":
			assistantStarted = false
		case "assistant":
			if !assistantStarted {
				s.sess.AppendLog([]byte("assistant: "))
				assistantStarted = true
			}
		}
		s.sess.AppendLog(FormatEvent(ev))
		if ev.Role == "user" {
			s.sess.mu.Lock()
			s.sess.ChatOffset = len(s.sess.log)
			s.sess.mu.Unlock()
		}
	}
}

func (s *SessionFileStore) handleCtl(input string) error {
	cmd := strings.Fields(input)
	if len(cmd) == 0 {
		return fmt.Errorf("empty ctl command")
	}
	switch cmd[0] {
	case "stop":
		s.sess.Core.Interrupt(agent.ErrInterrupted)
	case "kill":
		s.kill()
	case "rn":
		if name := strings.TrimSpace(input[3:]); name != "" {
			if err := s.rename(name); err != nil {
				s.log.Error("rename: %v", err)
			}
		}
	case "save":
		s.sess.mu.RLock()
		data := make([]byte, len(s.sess.log))
		copy(data, s.sess.log)
		s.sess.mu.RUnlock()
		return s.saveTranscript(data)
	case "compact", "clear", "backend", "model", "models",
		"agents", "agent", "sessions", "cwd", "skills",
		"tools", "context", "usage", "history",
		"irw", "help":
		s.sess.Core.Submit(s.sess.Ctx, "/"+input, s.makePublish())
	default:
		return fmt.Errorf("unknown ctl command: %s", cmd[0])
	}
	return nil
}
