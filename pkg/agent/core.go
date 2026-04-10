package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"crypto/rand"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"text/template"
	"time"

	"ollie/pkg/backend"
	"ollie/pkg/config"
	"ollie/pkg/mcp"
	"ollie/pkg/tools"
)

// systemPrompt builds the full system prompt for a given tool set.
func systemPrompt(workdir string) string {
	if workdir == "" {
		workdir, _ = os.Getwd()
	}
	raw, err := os.ReadFile(DefaultPromptsDir() + "/SYSTEM_PROMPT.md")
	if err != nil {
		return "You are ollie, an agentic assistant."
	}
	tmpl, err := template.New("system").Parse(string(raw))
	if err != nil {
		return string(raw)
	}
	var buf bytes.Buffer
	tmpl.Execute(&buf, map[string]string{
		"WorkDir":   workdir,
		"Platform":  runtime.GOOS,
		"Date":      time.Now().Format("2006-01-02"),
		"IsGitRepo": "unknown",
	})
	return buf.String()
}

// AgentEnv holds the runtime state derived from an agent config file.
type AgentEnv struct {
	dispatcher   tools.Dispatcher
	tools        []backend.Tool
	exec         toolExecutor
	Hooks        Hooks
	systemPrompt string
	genParams    backend.GenerationParams
	Messages     []string
}

// BuildAgentEnv constructs an AgentEnv from a pre-configured Dispatcher and
// optional agent config. workdir sets the working directory reported in the
// system prompt; if empty, the process working directory is used.
// The caller is responsible for registering all servers on d before calling this.
func BuildAgentEnv(cfg *config.Config, d tools.Dispatcher, workdir string) AgentEnv {
	var messages []string

	if cfg != nil {
		for name, serverCfg := range cfg.MCPServers {
			if serverCfg.Disabled || serverCfg.Command == "" {
				continue
			}
			transport := mcp.NewSTDIOTransport(serverCfg.Command, serverCfg.Args, serverCfg.Env)
			client, err := transport.Connect()
			if err != nil {
				messages = append(messages, fmt.Sprintf("MCP %s: failed to connect: %v", name, err))
				continue
			}
			d.AddServer(name, tools.NewServer(client))
			messages = append(messages, fmt.Sprintf("MCP %s: connected", name))
		}
	}

	allToolInfos, listErr := d.ListTools()
	if listErr != nil {
		messages = append(messages, fmt.Sprintf("list tools: %v", listErr))
	}

	serverOf := make(map[string]string, len(allToolInfos))
	for _, t := range allToolInfos {
		serverOf[t.Name] = t.Server
	}

	allTools := mcpToolsToBackend(allToolInfos)

	hooks := Hooks{}
	agentPrompt := ""
	var genParams backend.GenerationParams
	if cfg != nil {
		if cfg.Hooks != nil {
			hooks = cfg.Hooks
		}
		agentPrompt = cfg.Prompt
		genParams = backend.GenerationParams{
			MaxTokens:        cfg.MaxTokens,
			Temperature:      cfg.Temperature,
			FrequencyPenalty: cfg.FrequencyPenalty,
			PresencePenalty:  cfg.PresencePenalty,
		}
	}

	sp := systemPrompt(workdir)
	if agentPrompt != "" {
		sp += "\n\n" + agentPrompt
	}

	exec := func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		server, ok := serverOf[name]
		if !ok {
			return "", fmt.Errorf("unknown tool: %s", name)
		}
		raw, err := d.Dispatch(ctx, server, name, args)
		if err != nil {
			return "", err
		}
		return extractMCPText(raw), nil
	}

	return AgentEnv{
		dispatcher:   d,
		tools:        allTools,
		exec:         exec,
		Hooks:        hooks,
		systemPrompt: sp,
		genParams:    genParams,
		Messages:     messages,
	}
}


// DefaultAgentsDir returns the default directory for agent config files.
func DefaultAgentsDir() string {
	home, _ := os.UserHomeDir()
	return home + "/.config/ollie/agents"
}

// DefaultPromptsDir returns the default directory for prompt templates.
func DefaultPromptsDir() string {
	home, _ := os.UserHomeDir()
	return home + "/.config/ollie/prompts"
}

// DefaultSessionsDir returns the default directory for saved sessions.
func DefaultSessionsDir() string {
	home, _ := os.UserHomeDir()
	return home + "/.config/ollie/sessions"
}

// AgentConfigPath resolves the config file path for a named agent.
func AgentConfigPath(agentsDir, name string) string {
	p := agentsDir + "/" + name + ".json"
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if name == "default" {
		home, _ := os.UserHomeDir()
		return home + "/.config/ollie/config.json"
	}
	return p
}

// NewSessionID generates a unique, lexicographically sortable session identifier.
// Format: <unix-nanoseconds>-<random-hex> — sortable by creation time, unique
// even if two sessions are created within the same nanosecond.
func NewSessionID() string {
	b := make([]byte, 3)
	rand.Read(b) //nolint:errcheck
	return strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + fmt.Sprintf("%06x", b)
}


// infoEvent wraps a plain-text message as an info Event.
func infoEvent(text string) Event {
	return Event{Role: "info", Content: text + "\n"}
}

// actionHandle holds the cancel function for the current agent turn.
type actionHandle struct {
	cancel context.CancelCauseFunc
}

// AgentCoreConfig is the configuration for creating an agentCore.
type AgentCoreConfig struct {
	Backend     backend.Backend
	ModelName   string // if non-empty, overrides backend's default model
	AgentName   string
	AgentsDir   string
	SessionsDir string
	SessionID   string
	WorkDir     string // working directory for tool execution and system prompt
	Session     *Session
	Env           AgentEnv
	NewDispatcher func() tools.Dispatcher
}

// agentCore is the Core implementation. It owns all agent and session state
// but has no knowledge of how output is rendered.
type agentCore struct {
	session       *Session
	loopcfg       loopConfig
	hooks         Hooks
	agentName     string
	agentsDir     string
	sessionsDir   string
	sessionID     string
	dispatcher    tools.Dispatcher
	newDispatcher func() tools.Dispatcher
	workdir       string
	currentAction atomic.Pointer[actionHandle]
}

var _ Core = (*agentCore)(nil) // compile-time interface check

// NewAgentCore creates an agentCore from the given configuration.
func NewAgentCore(cfg AgentCoreConfig) Core {
	if cfg.ModelName != "" {
		cfg.Backend.SetModel(cfg.ModelName)
	}
	loopcfg := loopConfig{
		Backend:          cfg.Backend,
		systemPrompt:     cfg.Env.systemPrompt,
		Tools:            cfg.Env.tools,
		Exec:             cfg.Env.exec,
		MaxSteps:         20,
		GenerationParams: cfg.Env.genParams,
	}
	return &agentCore{
		session:       cfg.Session,
		loopcfg:       loopcfg,
		hooks:         cfg.Env.Hooks,
		agentName:     cfg.AgentName,
		agentsDir:     cfg.AgentsDir,
		sessionsDir:   cfg.SessionsDir,
		sessionID:     cfg.SessionID,
		workdir:       cfg.WorkDir,
		dispatcher:    cfg.Env.dispatcher,
		newDispatcher: cfg.NewDispatcher,
	}
}

func (s *agentCore) prompt() string {
	return fmt.Sprintf("[%s :: %s] ", s.loopcfg.Backend.Name(), s.agentName)
}

// Prompt returns the display prompt string for the current session state.
func (s *agentCore) Prompt() string { return s.prompt() }

// defaultContextLength is used when the backend cannot report the model's
// actual context window (e.g. CodeWhisperer). 128k tokens is a safe default
// for modern models.
const defaultContextLength = 128000

// autoCompactLimit returns the token threshold for auto-compaction.
// Uses 75% of the model's context length, reserving room for output and the
// compaction prompt.
func (s *agentCore) autoCompactLimit(ctx context.Context) int {
	ctxLen := s.loopcfg.Backend.ContextLength(ctx)
	if ctxLen <= 0 {
		ctxLen = defaultContextLength
	}
	return ctxLen * 3 / 4
}

func (s *agentCore) saveSession() {
	if s.session == nil || s.sessionID == "" || s.sessionsDir == "" {
		return
	}
	path := s.sessionsDir + "/" + s.sessionID + ".json"
	if err := s.session.saveTo(path, s.sessionID, s.agentName); err != nil {
		fmt.Fprintln(os.Stderr, "session save:", err)
	}
}

func (s *agentCore) getActionCancel() context.CancelCauseFunc {
	if a := s.currentAction.Load(); a != nil {
		return a.cancel
	}
	return nil
}

// Interrupt cancels the current in-progress agent turn.
// Returns true if an action was running and was cancelled.
func (s *agentCore) Interrupt(cause error) bool {
	if cancel := s.getActionCancel(); cancel != nil {
		cancel(cause)
		return true
	}
	return false
}

// Submit implements Core. It processes one line of user input: slash commands
// and shell shortcuts are dispatched immediately via handler; any other input
// starts an agent turn that streams events to handler.
func (s *agentCore) Submit(ctx context.Context, input string, handler EventHandler) {
	if s.handleCommand(ctx, input, handler) {
		return
	}

	s.hooks.Run(HookUserPromptSubmit)

	if s.session == nil {
		s.session = newSession(input)
	} else {
		s.session.appendUserMessage(input)
	}

	actCtx, actCancel := context.WithCancelCause(ctx)
	handle := &actionHandle{cancel: actCancel}
	s.currentAction.Store(handle)

	s.loopcfg.Output = handler

	// Auto-compact before the turn if approaching the context limit.
	if s.session != nil {
		if limit := s.autoCompactLimit(ctx); limit > 0 && s.session.estimateTokens() >= limit {
			emit(s.loopcfg, Event{Role: "info", Content: "auto-compacting context...\n"})
			s.session.compact(ctx, s.loopcfg.Backend) //nolint:errcheck
		}
	}

	s.hooks.Run(HookAgentSpawn)

	err := run(actCtx, s.loopcfg, s.session)
	actCancel(nil)
	s.currentAction.CompareAndSwap(handle, nil)

	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrInterrupted) {
		handler(Event{Role: "error", Content: err.Error()})
		s.session.rollback()
	}

	handler(Event{Role: "newline"})

	s.hooks.Run(HookStop)

	s.saveSession()
}

func (s *agentCore) handleCommand(ctx context.Context, input string, handler EventHandler) bool {
	if strings.HasPrefix(input, "!") {
		cmdStr := strings.TrimSpace(input[1:])
		if cmdStr == "" {
			return true
		}
		o, err := exec.Command("sh", "-c", cmdStr).CombinedOutput()
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

	switch cmd {
	case "/backend":
		if len(args) == 0 {
			handler(infoEvent("error: /backend requires an argument (e.g., /backend ollama)"))
			return true
		}
		os.Setenv("OLLIE_BACKEND", args[0])
		be, err := backend.New()
		if err != nil {
			handler(infoEvent(fmt.Sprintf("error: failed to switch backend: %v", err)))
			return true
		}
		s.loopcfg.Backend = be
		handler(infoEvent(fmt.Sprintf("switched backend to: %s (model: %s)", be.Name(), be.Model())))
		return true

	case "/models":
		models := s.loopcfg.Backend.Models(ctx)
		if len(models) == 0 {
			handler(infoEvent("no models available"))
			return true
		}
		current := s.loopcfg.Backend.Model()
		for _, m := range models {
			marker := "  "
			if m == current {
				marker = "* "
			}
			handler(infoEvent(marker + m))
		}
		return true

	case "/model":
		if len(args) == 0 {
			handler(infoEvent("error: /model requires an argument (e.g., /model qwen3:8b)"))
			return true
		}
		s.loopcfg.Backend.SetModel(args[0])
		handler(infoEvent("switched model to: " + args[0]))
		return true

	case "/agents":
		entries, err := os.ReadDir(s.agentsDir)
		if err != nil {
			handler(infoEvent(fmt.Sprintf("agents: %v", err)))
			return true
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
		return true

	case "/agent":
		if len(args) == 0 {
			handler(infoEvent("active agent: " + s.agentName))
			return true
		}
		name := args[0]
		cfgPath := AgentConfigPath(s.agentsDir, name)
		cfg, err := config.Load(cfgPath)
		if err != nil {
			handler(infoEvent(fmt.Sprintf("error: agent %q: %v", name, err)))
			return true
		}
		if s.dispatcher != nil {
			s.dispatcher.Close()
		}
		d := s.newDispatcher()
		env := BuildAgentEnv(cfg, d, s.workdir)
		s.dispatcher = env.dispatcher
		s.hooks = env.Hooks
		s.loopcfg.systemPrompt = env.systemPrompt
		s.loopcfg.Tools = env.tools
		s.loopcfg.Exec = env.exec
		s.loopcfg.GenerationParams = env.genParams
		s.agentName = name
		s.session = nil
		s.sessionID = NewSessionID()
		for _, msg := range env.Messages {
			handler(infoEvent(msg))
		}
		handler(infoEvent("agent: " + name))
		return true

	case "/compact":
		if s.session == nil {
			handler(infoEvent("nothing to compact"))
			return true
		}
		// Snapshot history before compaction for persistence.
		snapshot := s.session.PreCompactionSnapshot()
		n, _, err := s.session.compact(ctx, s.loopcfg.Backend)
		if err != nil {
			handler(infoEvent("compact error: " + err.Error()))
		} else if n == 0 {
			handler(infoEvent("nothing to compact"))
		} else {
			// Persist pre-compaction history as append-only JSONL.
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
		}
		return true

	case "/context":
		if s.session == nil {
			handler(infoEvent("no active session"))
			return true
		}
		handler(infoEvent(strings.TrimRight(s.session.contextDebug(), "\n")))
		return true

	case "/history":
		if s.session == nil {
			handler(infoEvent("no active session"))
			return true
		}
		for _, msg := range s.session.history() {
			preview := msg.Content
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			handler(infoEvent(fmt.Sprintf("[%s] %s", msg.Role, preview)))
		}
		return true

	case "/clear":
		s.session = nil
		s.sessionID = NewSessionID()
		handler(infoEvent("cleared"))
		return true

	case "/sessions":
		entries, err := os.ReadDir(s.sessionsDir)
		if err != nil {
			handler(infoEvent(fmt.Sprintf("sessions: %v", err)))
			return true
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
		return true

	case "/help":
		lines := []string{
			"Available commands:",
			"  /agents          - list available agent configs",
			"  /sessions        - list saved sessions",
			"  /agent [name]    - show or switch active agent",
			"  /backend <type>  - switch backend (ollama, openai)",
			"  /model <name>    - switch model",
			"  /models          - list available models",
			"  /queued [pop|clear] - manage queued prompts",
			"  /compact         - summarize conversation and compact context",
			"  /context         - show context window debug info",
			"  /history         - dump bounded message history",
			"  /clear           - clear session",
			"  /help            - show this help",
			"  !<cmd>           - run shell command",
		}
		for _, l := range lines {
			handler(infoEvent(l))
		}
		return true
	}

	return false
}

func mcpToolsToBackend(mcpTools []tools.ToolInfo) []backend.Tool {
	out := make([]backend.Tool, len(mcpTools))
	for i, t := range mcpTools {
		out[i] = backend.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}
	}
	return out
}

func extractMCPText(raw json.RawMessage) string {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return string(raw)
	}
	var parts []string
	for _, c := range result.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

