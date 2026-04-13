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
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"ollie/pkg/backend"
	"ollie/pkg/config"
	"ollie/pkg/mcp"
	"ollie/pkg/tools"
	"ollie/pkg/tools/reasoning"
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
	agentPrompt  string // agent-specific suffix appended after the base system prompt
	genParams    backend.GenerationParams
	Messages     []string
}

// EnvOption is a functional option for BuildAgentEnv.
type EnvOption func(*envOptions)

type envOptions struct {
	fallbackPlan tools.PlanBackend
}

// WithFallbackPlanBackend sets a PlanBackend used when no task_create MCP tool
// is available. When task_create IS available, it takes priority and this
// fallback is ignored.
func WithFallbackPlanBackend(b tools.PlanBackend) EnvOption {
	return func(o *envOptions) { o.fallbackPlan = b }
}

// BuildAgentEnv constructs an AgentEnv from a pre-configured Dispatcher and
// optional agent config. workdir sets the working directory reported in the
// system prompt; if empty, the process working directory is used.
// The caller is responsible for registering all servers on d before calling this.
func BuildAgentEnv(cfg *config.Config, d tools.Dispatcher, workdir string, opts ...EnvOption) AgentEnv {
	var eo envOptions
	for _, o := range opts {
		o(&eo)
	}
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

	// Wire plan backend to the reasoning server.
	// If task_create is available, use the dispatch backend (primary).
	// Otherwise fall back to the caller-supplied backend, if any.
	if rs, ok := d.GetServer("reasoning"); ok {
		if setter, ok := rs.(tools.PlanBackendSetter); ok {
			if taskServer, ok := serverOf["task_create"]; ok {
				setter.SetPlanBackend(&dispatchPlanBackend{d: d, server: taskServer})
			} else if eo.fallbackPlan != nil {
				setter.SetPlanBackend(eo.fallbackPlan)
			}
		}

		// Wire memory backend to the reasoning server.
		// If memory_remember is available via MCP, use the dispatch backend.
		// Otherwise fall back to the flat-dir backend (always available).
		if setter, ok := rs.(tools.MemoryBackendSetter); ok {
			if memServer, ok := serverOf["memory_remember"]; ok {
				setter.SetMemoryBackend(&dispatchMemoryBackend{d: d, server: memServer})
			} else {
				setter.SetMemoryBackend(reasoning.NewFlatDirBackend(""))
			}
		}
	}

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
		agentPrompt:  agentPrompt,
		genParams:    genParams,
		Messages:     messages,
	}
}

// Dispatcher returns the underlying tool dispatcher.
func (e *AgentEnv) Dispatcher() tools.Dispatcher { return e.dispatcher }


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
	agentPrompt   string // agent-specific prompt suffix; kept for system prompt rebuilds
	currentAction atomic.Pointer[actionHandle]
	fifo          PromptFIFO
	pendingInject atomic.Pointer[string]
	mu            sync.RWMutex
	state         string // "idle", "thinking", "calling: <tool>"
	reply         string // assistant text from the most recently completed turn
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
		agentPrompt:   cfg.Env.agentPrompt,
		dispatcher:    cfg.Env.dispatcher,
		newDispatcher: cfg.NewDispatcher,
		state:         "idle",
	}
}

func (s *agentCore) prompt() string {
	return fmt.Sprintf("[%s :: %s] ", s.loopcfg.Backend.Name(), s.agentName)
}

// Prompt returns the display prompt string for the current session state.
func (s *agentCore) Prompt() string { return s.prompt() }

func (s *agentCore) AgentName() string   { return s.agentName }
func (s *agentCore) BackendName() string { return s.loopcfg.Backend.Name() }
func (s *agentCore) ModelName() string   { return s.loopcfg.Backend.Model() }

func (s *agentCore) State() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *agentCore) Reply() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reply
}

// WorkDir returns the current working directory for tool execution.
func (s *agentCore) WorkDir() string {
	if s.workdir != "" {
		return s.workdir
	}
	wd, _ := os.Getwd()
	return wd
}

// SetWorkDir changes the working directory for tool execution and updates the
// system prompt. Returns an error if the path does not exist.
func (s *agentCore) SetWorkDir(dir string) error {
	if dir != "" {
		if _, err := os.Stat(dir); err != nil {
			return fmt.Errorf("workdir: %w", err)
		}
	}
	s.workdir = dir
	// Propagate to any tool server that knows how to handle it (e.g. execute).
	if s.dispatcher != nil {
		if srv, ok := s.dispatcher.GetServer("execute"); ok {
			if ws, ok := srv.(tools.WorkDirSetter); ok {
				ws.SetWorkDir(dir)
			}
		}
	}
	// Rebuild system prompt so it reflects the new workdir.
	sp := systemPrompt(dir)
	if s.agentPrompt != "" {
		sp += "\n\n" + s.agentPrompt
	}
	s.loopcfg.systemPrompt = sp
	return nil
}

// SetSessionID renames the session. It updates the in-memory ID, renames
// persisted files on disk, and propagates to the execute server env.
func (s *agentCore) SetSessionID(newID string) error {
	oldID := s.sessionID
	if oldID == newID {
		return nil
	}
	// Rename persisted files on disk.
	if s.sessionsDir != "" && oldID != "" {
		for _, suffix := range []string{".json", ".compaction.jsonl"} {
			oldPath := s.sessionsDir + "/" + oldID + suffix
			if _, err := os.Stat(oldPath); err == nil {
				if err := os.Rename(oldPath, s.sessionsDir+"/"+newID+suffix); err != nil {
					return fmt.Errorf("rename %s: %w", suffix, err)
				}
			}
		}
	}
	s.sessionID = newID
	// Propagate to the execute server env.
	if s.dispatcher != nil {
		if srv, ok := s.dispatcher.GetServer("execute"); ok {
			if es, ok := srv.(tools.EnvSetter); ok {
				es.SetEnv("OLLIE_SESSION_ID", newID)
			}
		}
	}
	return nil
}

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

func (s *agentCore) Inject(prompt string) {
	// If an inject is already pending, fall back to the normal FIFO so nothing
	// is lost. Use CompareAndSwap to avoid a race between the nil check and store.
	if !s.pendingInject.CompareAndSwap(nil, &prompt) {
		s.fifo.Push(prompt)
		return
	}
	emit(s.loopcfg, Event{Role: "info", Content: "\n"})
	emit(s.loopcfg, Event{Role: "user", Content: prompt})
}

func (s *agentCore) injectRewrite(prompt string) {
	s.pendingInject.Store(&prompt)
	emit(s.loopcfg, Event{Role: "info", Content: "\n"})
	emit(s.loopcfg, Event{Role: "user", Content: prompt})
}

func (s *agentCore) Queue(prompt string) {
	s.fifo.Push(prompt)
}

func (s *agentCore) PopQueue() (string, bool) {
	return s.fifo.Pop()
}

func (s *agentCore) IsRunning() bool {
	return s.currentAction.Load() != nil
}

func (s *agentCore) CtxSz() string {
	if s.session == nil {
		return "no active session"
	}
	ctxLen := s.loopcfg.Backend.ContextLength(context.Background())
	if ctxLen <= 0 {
		ctxLen = defaultContextLength
	}
	estimated := s.session.estimateTokens()
	pct := estimated * 100 / ctxLen
	return fmt.Sprintf("%d / %d (%d%%)", estimated, ctxLen, pct)
}

func (s *agentCore) Usage() string {
	if s.session == nil {
		return "no active session"
	}
	str := fmt.Sprintf("%d in, %d out, %d requests",
		s.session.TotalInputTokens, s.session.TotalOutputTokens,
		s.session.TotalRequests)
	if s.session.Estimated {
		str += " [estimated]"
	}
	return str
}

func (s *agentCore) ListModels() string {
	models := s.loopcfg.Backend.Models(context.Background())
	return strings.Join(models, "\n")
}

func (s *agentCore) ListServers() string {
	if s.dispatcher == nil {
		return "no dispatcher"
	}
	allTools, err := s.dispatcher.ListTools()
	if err != nil {
		return "error: " + err.Error()
	}
	if len(allTools) == 0 {
		return "no servers registered"
	}

	// Group tools by server, preserving first-seen order.
	type serverEntry struct {
		name  string
		tools []tools.ToolInfo
	}
	index := map[string]int{}
	var servers []serverEntry
	for _, t := range allTools {
		i, ok := index[t.Server]
		if !ok {
			i = len(servers)
			index[t.Server] = i
			servers = append(servers, serverEntry{name: t.Server})
		}
		servers[i].tools = append(servers[i].tools, t)
	}

	var sb strings.Builder
	for si, srv := range servers {
		if si > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "%s\n", srv.name)
		for _, t := range srv.tools {
			desc := firstSentence(t.Description)
			fmt.Fprintf(&sb, "  %-22s %s\n", t.Name, desc)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// firstSentence returns the first sentence of s (up to the first period or
// newline), trimmed. Falls back to s truncated at 80 chars if no sentence end
// is found.
func firstSentence(s string) string {
	for i, r := range s {
		if r == '.' || r == '\n' {
			return strings.TrimSpace(s[:i+1])
		}
	}
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

// Submit implements Core. It processes one line of user input: slash commands
// and shell shortcuts are dispatched immediately via handler; any other input
// starts an agent turn that streams events to handler. If a turn is already
// in progress the prompt is queued as an in-stream interruption instead.
func (s *agentCore) Submit(ctx context.Context, input string, handler EventHandler) {
	if s.handleCommand(ctx, input, handler) {
		return
	}

	if s.IsRunning() {
		s.Inject(input)
		return
	}

	handler(Event{Role: "user", Content: input})

	hookResult := s.hooks.Run(ctx, HookUserPromptSubmit, map[string]string{
		"session_id": s.sessionID,
		"prompt":     input,
	})
	if hookResult.Blocked {
		handler(infoEvent("hook blocked prompt"))
		return
	}
	if hookResult.Context != "" {
		input += "\n" + hookResult.Context
	}

	if s.session == nil {
		s.session = newSession(input)
	} else {
		s.session.appendUserMessage(input)
	}

	s.mu.Lock()
	s.state = "thinking"
	s.mu.Unlock()

	actCtx, actCancel := context.WithCancelCause(ctx)
	handle := &actionHandle{cancel: actCancel}
	s.currentAction.Store(handle)

	var replyBuf strings.Builder
	s.loopcfg.Output = func(ev Event) {
		switch ev.Role {
		case "assistant":
			replyBuf.WriteString(ev.Content)
		case "call":
			s.mu.Lock()
			s.state = "calling: " + ev.Name
			s.mu.Unlock()
		case "tool":
			s.mu.Lock()
			s.state = "thinking"
			s.mu.Unlock()
		}
		if ev.Role == "usage" && s.session != nil {
			var in, out, est int
			fmt.Sscanf(ev.Content, "%d %d %d", &in, &out, &est)
			s.session.addUsage(backend.Usage{InputTokens: in, OutputTokens: out}, est != 0)
		}
		handler(ev)
	}
	s.loopcfg.PopInject = func() string {
		if p := s.pendingInject.Swap(nil); p != nil {
			return *p
		}
		return ""
	}

	// Auto-compact before the turn if approaching the context limit.
	if s.session != nil {
		if limit := s.autoCompactLimit(ctx); limit > 0 && s.session.estimateTokens() >= limit {
			emit(s.loopcfg, Event{Role: "info", Content: "auto-compacting context...\n"})
			s.session.compact(ctx, s.loopcfg.Backend) //nolint:errcheck
		}
	}

	s.hooks.Run(ctx, HookAgentSpawn, map[string]string{
		"session_id": s.sessionID,
	})

	err := run(actCtx, s.loopcfg, s.session)
	actCancel(nil)
	s.currentAction.CompareAndSwap(handle, nil)

	s.mu.Lock()
	s.reply = replyBuf.String()
	s.state = "idle"
	s.mu.Unlock()
	replyBuf.Reset()

	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrInterrupted) {
		handler(Event{Role: "error", Content: err.Error()})
	}

	handler(Event{Role: "newline"})

	stopResult := s.hooks.Run(ctx, HookStop, map[string]string{
		"session_id": s.sessionID,
	})

	s.saveSession()

	// Stop hook said "continue" — re-enter with the hook's context as the prompt.
	if stopResult.Blocked && stopResult.Context != "" && ctx.Err() == nil {
		s.Submit(ctx, stopResult.Context, handler)
		return
	}

	// If an inject was pending but never consumed (e.g. text-only response
	// with no tool calls), treat it as the next user message.
	if p := s.pendingInject.Swap(nil); p != nil && ctx.Err() == nil {
		s.Submit(ctx, *p, handler)
		return
	}

	// Drain the FIFO: run queued prompts sequentially.
	for ctx.Err() == nil {
		next, ok := s.fifo.Pop()
		if !ok {
			break
		}
		s.Submit(ctx, next, handler)
	}
}

func (s *agentCore) handleCommand(ctx context.Context, input string, handler EventHandler) bool {
	if strings.HasPrefix(input, "!") {
		handler(infoEvent(""))
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

	handler(infoEvent(""))

	switch cmd {
	case "/irw":
		prompt := strings.Join(args, " ")
		if prompt == "" {
			handler(infoEvent("error: /irw requires a prompt"))
			return true
		}
		s.injectRewrite(prompt)
		return true

	case "/backend":
		if len(args) == 0 {
			handler(infoEvent(s.loopcfg.Backend.Name()))
			return true
		}
		if s.IsRunning() {
			handler(infoEvent("error: cannot switch backend while agent is running"))
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
			handler(infoEvent(s.loopcfg.Backend.Model()))
			return true
		}
		if s.IsRunning() {
			handler(infoEvent("error: cannot switch model while agent is running"))
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
		if s.IsRunning() {
			handler(infoEvent("error: cannot switch agent while agent is running"))
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
		s.agentPrompt = env.agentPrompt
		s.agentName = name
		s.session = nil
		s.sessionID = NewSessionID()
		for _, msg := range env.Messages {
			handler(infoEvent(msg))
		}
		handler(infoEvent("agent: " + name))
		return true

	case "/compact":
		if s.IsRunning() {
			handler(infoEvent("error: cannot compact while agent is running"))
			return true
		}
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
		ctxLen := s.loopcfg.Backend.ContextLength(ctx)
		if ctxLen <= 0 {
			ctxLen = defaultContextLength
		}
		estimated := s.session.estimateTokens()
		pct := estimated * 100 / ctxLen
		handler(infoEvent(fmt.Sprintf("~%d / %d tokens (%d%%)", estimated, ctxLen, pct)))
		handler(infoEvent(strings.TrimRight(s.session.contextDebug(), "\n")))
		return true

	case "/usage":
		if s.session == nil {
			handler(infoEvent("no active session"))
			return true
		}
		ctxLen := s.loopcfg.Backend.ContextLength(ctx)
		if ctxLen <= 0 {
			ctxLen = defaultContextLength
		}
		ctxEstimated := s.session.estimateTokens()
		pct := ctxEstimated * 100 / ctxLen
		usageStr := fmt.Sprintf("~%d / %d tokens (%d%%) | %d in, %d out, %d requests",
			ctxEstimated, ctxLen, pct,
			s.session.TotalInputTokens, s.session.TotalOutputTokens,
			s.session.TotalRequests)
		if s.session.Estimated {
			usageStr += " [estimated]"
		}
		handler(infoEvent(usageStr))
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
		if s.IsRunning() {
			handler(infoEvent("error: cannot clear while agent is running"))
			return true
		}
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

	case "/cwd":
		if len(args) == 0 {
			handler(infoEvent("workdir: " + s.WorkDir()))
			return true
		}
		dir := strings.Join(args, " ")
		if err := s.SetWorkDir(dir); err != nil {
			handler(infoEvent("error: " + err.Error()))
			return true
		}
		handler(infoEvent("workdir: " + dir))
		return true

	case "/skills", "/tools":
		subdir := "sk"
		if cmd == "/tools" {
			subdir = "t"
		}
		mount := os.Getenv("OLLIE_9MOUNT")
		if mount == "" {
			home, _ := os.UserHomeDir()
			mount = home + "/mnt/ollie"
		}
		entries, err := os.ReadDir(mount + "/" + subdir)
		if err != nil {
			handler(infoEvent(fmt.Sprintf("%s: %v", cmd, err)))
			return true
		}
		for _, e := range entries {
			if !e.IsDir() {
				handler(infoEvent("  " + e.Name()))
			}
		}
		return true

	case "/mcp":
		handler(infoEvent(s.ListServers()))
		return true

	case "/help":
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
			"  /mcp             - list registered tool servers and their tools",
			"  /cwd [path]      - show or change working directory",
			"  /queued [pop|clear] - manage queued prompts",
			"  /compact         - summarize conversation and compact context",
			"  /context         - show context size and message breakdown",
			"  /usage           - show token usage and context percentage",
			"  /history         - dump bounded message history",
			"  /clear           - clear session",
			"  /kill            - kill session",
			"  /rn <name>       - rename session",
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

