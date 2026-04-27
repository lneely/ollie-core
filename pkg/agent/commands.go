package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"ollie/pkg/config"
)

func (s *agent) handleCommand(ctx context.Context, input string, handler EventHandler) bool {
	if strings.HasPrefix(input, "!") {
		handler(infoEvent(""))
		cmdStr := strings.TrimSpace(input[1:])
		if cmdStr == "" {
			return true
		}
		shellCmd := exec.Command("sh", "-c", cmdStr)
		shellCmd.Dir = s.CWD()
		shellCmd.Env = append(os.Environ(), "OLLIE_SESSION_ID="+s.sessionID)
		s.envMu.RLock()
		for k, v := range s.env {
			shellCmd.Env = append(shellCmd.Env, k+"="+v)
		}
		s.envMu.RUnlock()
		o, err := shellCmd.CombinedOutput()
		if err != nil {
			handler(infoEvent("error: " + err.Error()))
		}
		if len(o) > 0 {
			handler(infoEvent(strings.TrimRight(string(o), "\n")))
		}
		return true
	}

	if !strings.HasPrefix(input, "/") {
		return false
	}

	parts := strings.Fields(input)
	if len(parts) == 0 {
		return false
	}
	cmd := parts[0]
	args := parts[1:]

	listMountDir := func(subdir string) {
		mount := os.Getenv("OLLIE")
		if mount == "" {
			home, _ := os.UserHomeDir()
			mount = home + "/mnt/ollie"
		}
		entries, err := os.ReadDir(mount + "/" + subdir)
		if err != nil {
			handler(infoEvent(fmt.Sprintf("%s: %v", cmd, err)))
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				handler(infoEvent("  " + e.Name()))
			}
		}
	}

	type cmdFn func([]string)
	cmds := map[string]cmdFn{
		"/i": func(args []string) {
			prompt := strings.Join(args, " ")
			if prompt == "" {
				handler(infoEvent("error: /i requires a prompt"))
				return
			}
			s.Inject(prompt)
		},

		"/irw": func(args []string) {
			prompt := strings.Join(args, " ")
			if prompt == "" {
				handler(infoEvent("error: /irw requires a prompt"))
				return
			}
			s.injectRewrite(prompt)
		},

		"/backend": func(args []string) {
			if len(args) == 0 {
				handler(infoEvent(s.cfg.Backend.Name()))
				return
			}
			if s.IsRunning() {
				handler(infoEvent("error: cannot switch backend while agent is running"))
				return
			}
			be, err := s.newBackend(args[0])
			if err != nil {
				handler(infoEvent(fmt.Sprintf("error: failed to switch backend: %v", err)))
				return
			}
			s.cfg.Backend = be
			handler(infoEvent(fmt.Sprintf("switched backend to: %s (model: %s)", be.Name(), be.Model())))
		},

		"/models": func(args []string) {
			models := s.cfg.Backend.Models(ctx)
			if len(models) == 0 {
				handler(infoEvent("no models available"))
				return
			}
			current := s.cfg.Backend.Model()
			for _, m := range models {
				marker := "  "
				if m == current {
					marker = "* "
				}
				handler(infoEvent(marker + m))
			}
		},

		"/model": func(args []string) {
			if len(args) == 0 {
				handler(infoEvent(s.cfg.Backend.Model()))
				return
			}
			if s.IsRunning() {
				handler(infoEvent("error: cannot switch model while agent is running"))
				return
			}
			s.cfg.Backend.SetModel(args[0])
			handler(infoEvent("switched model to: " + args[0]))
		},

		"/agents": func(args []string) {
			entries, err := os.ReadDir(s.agentsDir)
			if err != nil {
				handler(infoEvent(fmt.Sprintf("agents: %v", err)))
				return
			}
			found := false
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				name := strings.TrimSuffix(e.Name(), ".json")
				marker := "  "
				if name == s.agentName {
					marker = "* "
				}
				handler(infoEvent(marker + name))
				found = true
			}
			if !found {
				handler(infoEvent("no agents found in " + s.agentsDir))
			}
		},

		"/agent": func(args []string) {
			if len(args) == 0 {
				handler(infoEvent("active agent: " + s.agentName))
				return
			}
			if s.IsRunning() {
				handler(infoEvent("error: cannot switch agent while agent is running"))
				return
			}
			name := args[0]
			cfgPath := AgentConfigPath(s.agentsDir, name)
			f, err := os.Open(cfgPath)
			if err != nil {
				handler(infoEvent(fmt.Sprintf("error: agent %q: %v", name, err)))
				return
			}
			cfg, err := config.Load(f)
			f.Close()
			if err != nil {
				handler(infoEvent(fmt.Sprintf("error: agent %q: %v", name, err)))
				return
			}
			d := s.newDispatcher()
			env := BuildAgentEnv(cfg, d, s.cwd)
			s.dispatcher = env.dispatcher
			s.hooks = env.Hooks
			s.cfg.preamble = env.preamble
			s.cfg.Tools = env.tools
			s.cfg.Exec = env.exec
			s.cfg.GenerationParams = env.genParams
			s.agentName = name
			s.session = nil
			s.sessionID = NewSessionID()
			s.pushSessionEnv()
			for _, msg := range env.Messages {
				handler(infoEvent(msg))
			}
			handler(infoEvent("agent: " + name))
		},

		"/compact": func(args []string) {
			if s.IsRunning() {
				handler(infoEvent("error: cannot compact while agent is running"))
				return
			}
			if s.session == nil {
				handler(infoEvent("nothing to compact"))
				return
			}
			snapshot := s.session.PreCompactionSnapshot()
			s.setState("compacting")
			n, err := s.runCompact(ctx, "manual", handler)
			s.setState("idle")
			if err != nil {
				handler(infoEvent("compact error: " + err.Error()))
				return
			}
			if n == 0 {
				handler(infoEvent("nothing to compact"))
				return
			}
			if s.sessionsDir != "" && s.sessionID != "" {
				histPath := s.sessionsDir + "/" + s.sessionID + ".compaction.jsonl"
				if data, err := json.Marshal(snapshot); err == nil {
					f, err := os.OpenFile(histPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
					if err == nil {
						f.Write(append(data, '\n')) //nolint:errcheck
						f.Close()                   //nolint:errcheck
					}
				}
			}
			handler(infoEvent(fmt.Sprintf("compacted %d messages", n)))
			s.saveSession()
		},

		"/context": func(args []string) {
			if s.session == nil {
				handler(infoEvent("no active session"))
				return
			}
			ctxLen := s.cfg.Backend.ContextLength(ctx)
			if ctxLen <= 0 {
				ctxLen = defaultContextLength
			}
			estimated := s.session.estimateTokens()
			pct := estimated * 100 / ctxLen
			handler(infoEvent(fmt.Sprintf("~%d / %d tokens (%d%%)", estimated, ctxLen, pct)))
			handler(infoEvent(strings.TrimRight(s.session.contextDebug(), "\n")))
		},

		"/cost": func(args []string) {
			if s.session == nil {
				handler(infoEvent("no active session"))
				return
			}
			handler(infoEvent(fmt.Sprintf("last=$%.4f  session=$%.4f",
				s.session.LastTurnCostUSD, s.session.SessionCostUSD)))
		},

		"/usage": func(args []string) {
			if s.session == nil {
				handler(infoEvent("no active session"))
				return
			}
			ctxLen := s.cfg.Backend.ContextLength(ctx)
			if ctxLen <= 0 {
				ctxLen = defaultContextLength
			}
			estimated := s.session.estimateTokens()
			pct := estimated * 100 / ctxLen
			usageStr := fmt.Sprintf("~%d / %d tokens (%d%%) | %d in, %d out, %d requests",
				estimated, ctxLen, pct,
				s.session.TotalInputTokens, s.session.TotalOutputTokens,
				s.session.TotalRequests)
			if s.session.Estimated {
				usageStr += " [estimated]"
			}
			handler(infoEvent(usageStr))
		},

		"/history": func(args []string) {
			if s.session == nil {
				handler(infoEvent("no active session"))
				return
			}
			for _, msg := range s.session.history() {
				preview := msg.Content
				if len(preview) > 200 {
					preview = preview[:200] + "..."
				}
				handler(infoEvent(fmt.Sprintf("[%s] %s", msg.Role, preview)))
			}
		},

		"/clear": func(args []string) {
			if s.IsRunning() {
				handler(infoEvent("error: cannot clear while agent is running"))
				return
			}
			s.session = nil
			handler(infoEvent("cleared"))
		},

		"/sessions": func(args []string) {
			entries, err := os.ReadDir(s.sessionsDir)
			if err != nil {
				handler(infoEvent(fmt.Sprintf("sessions: %v", err)))
				return
			}
			found := false
			for i := len(entries) - 1; i >= 0; i-- {
				e := entries[i]
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				id := strings.TrimSuffix(e.Name(), ".json")
				marker := "  "
				if id == s.sessionID {
					marker = "* "
				}
				label := id
				if data, readErr := os.ReadFile(s.sessionsDir + "/" + e.Name()); readErr == nil {
					var ps PersistedSession
					if json.Unmarshal(data, &ps) == nil {
						goal := ""
						for _, msg := range ps.Messages {
							if msg.Role == "user" {
								goal = msg.Content
								break
							}
						}
						if len(goal) > 60 {
							goal = goal[:60] + "..."
						}
						label = fmt.Sprintf("%-24s  [%s] %q", id, ps.Agent, goal)
					}
				}
				handler(infoEvent(marker + label))
				found = true
			}
			if !found {
				handler(infoEvent("no sessions found in " + s.sessionsDir))
			}
		},

		"/cwd": func(args []string) {
			if len(args) == 0 {
				handler(infoEvent("cwd: " + s.CWD()))
				return
			}
			dir := strings.Join(args, " ")
			if err := s.SetCWD(dir); err != nil {
				handler(infoEvent("error: " + err.Error()))
				return
			}
			handler(infoEvent("cwd: " + dir))
		},

		"/skills": func(args []string) { listMountDir("sk") },
		"/tools":  func(args []string) { listMountDir("t") },

		"/sp": func(args []string) {
			handler(infoEvent(s.cfg.preamble))
		},

		"/help": func(args []string) {
			lines := []string{
				"Available commands:",
				"  /agents          - list available agent configs",
				"  /sessions        - list saved sessions",
				"  /agent [name]    - show or switch active agent",
				"  /backend [type]  - show current backend, or switch to <type>",
				"  /model [name]    - show current model, or switch to <name>",
				"  /models          - list available models",
				"  /skills          - list available skills",
				"  /tools           - list available tools",
				"  /cwd [path]      - show or change working directory",
				"  /i <prompt>       - inject prompt into the running turn",
				"  /irw <prompt>     - rewrite the pending inject",
				"  /queued [pop|clear] - manage queued prompts",
				"  /compact         - summarize conversation and compact context",
				"  /context         - show context size and message breakdown",
				"  /cost            - show last turn and session cost",
				"  /usage           - show token usage and context percentage",
				"  /history         - dump bounded message history",
				"  /clear           - clear session",
				"  /kill            - kill session",
				"  /rn <name>       - rename session",
				"  /sp              - show rendered system prompt",
				"  /help            - show this help",
				"  !<cmd>           - run shell command",
			}
			for _, l := range lines {
				handler(infoEvent(l))
			}
		},
	}

	fn, ok := cmds[cmd]
	if !ok {
		return false
	}
	handler(infoEvent(""))
	fn(args)
	return true
}
