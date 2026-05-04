package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ollie/pkg/backend"
	"ollie/pkg/config"
	olog "ollie/pkg/log"
	"ollie/pkg/paths"
	"ollie/pkg/tools"
)

// toolClassifier reports whether a named tool is safe to run concurrently
// with other read-class tools. nil means treat all tools as serial.
type toolClassifier func(name string) bool

// AgentEnv holds the runtime state derived from an agent config file.
type AgentEnv struct {
	dispatcher   tools.Dispatcher
	tools        []backend.Tool
	exec         toolExecutor
	classifyTool toolClassifier
	Hooks        Hooks
	preamble     string
	genParams    backend.GenerationParams
	Messages     []string
}

// BuildAgentEnv constructs an AgentEnv from a pre-configured Dispatcher and
// optional agent config. cwd sets the working directory reported in the
// system prompt; if empty, the process working directory is used.
// The caller is responsible for registering all servers on d before calling this.
func BuildAgentEnv(cfg *config.Config, d tools.Dispatcher, cwd string) AgentEnv {
	var messages []string

	var allToolInfos []tools.ToolInfo
	var allTools []backend.Tool
	serverOf := make(map[string]string)

	if cfg == nil || cfg.ToolsEnabled() {
		var listErr error
		allToolInfos, listErr = d.ListTools()
		if listErr != nil {
			messages = append(messages, fmt.Sprintf("list tools: %v", listErr))
		}
		for _, t := range allToolInfos {
			serverOf[t.Name] = t.Server
		}
		allTools = toolInfosToBackend(allToolInfos)
	}

	hooks := Hooks{}
	var preamble string
	var genParams backend.GenerationParams
	if cfg != nil {
		for k, v := range cfg.Hooks {
			hooks[k] = []string(v)
		}
		if resolved, err := resolvePrompt(cfg.Prompt, cwd); err == nil {
			preamble = resolved
		}
		genParams = backend.GenerationParams{
			MaxTokens:        cfg.MaxTokens,
			Temperature:      cfg.Temperature,
			FrequencyPenalty: cfg.FrequencyPenalty,
			PresencePenalty:  cfg.PresencePenalty,
		}
		if len(cfg.TrustedTools) > 0 {
			if srv, ok := d.GetServer("execute"); ok {
				if ts, ok := srv.(tools.TrustedToolsSetter); ok {
					ts.SetTrustedTools(cfg.TrustedTools)
				}
			}
		}
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
		text, isErr := extractToolResult(raw)
		if isErr {
			return "", fmt.Errorf("%s", text)
		}
		return text, nil
	}

	var classify toolClassifier
	if srv, ok := d.GetServer("execute"); ok {
		if pc, ok := srv.(tools.ParallelClassifier); ok {
			classify = pc.IsParallelRead
		}
	}

	return AgentEnv{
		dispatcher:   d,
		tools:        allTools,
		exec:         exec,
		classifyTool: classify,
		Hooks:        hooks,
		preamble:     preamble,
		genParams:    genParams,
		Messages:     messages,
	}
}


// DefaultPromptsDir returns the default directory for prompt templates.
func DefaultPromptsDir() string {
	return paths.CfgDir() + "/prompts"
}


// AgentConfigPath resolves the config file path for a named agent.
func AgentConfigPath(agentsDir, name string) string {
	return agentsDir + "/" + name + ".json"
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

// AgentCoreConfig is the configuration for creating an agent.
type AgentCoreConfig struct {
	Backend       backend.Backend
	ModelName     string // if non-empty, overrides backend's default model
	AgentName     string
	AgentsDir     string
	SessionsDir   string
	SessionID     string
	CWD           string // working directory for tool execution and system prompt
	Session       *Session
	Env           AgentEnv
	NewDispatcher func() tools.Dispatcher
	NewBackend    func(string) (backend.Backend, error) // if nil, defaults to backend.NewWithName
	Log           *olog.Logger                     // if nil, logging is disabled
}

// agent is the Core implementation. It owns all agent and session state
// but has no knowledge of how output is rendered.
type agent struct {
	session         *Session
	cfg agentConfig
	hooks           Hooks
	log             *olog.Logger
	agentName       string
	agentsDir       string
	sessionsDir     string
	sessionID       string
	dispatcher      tools.Dispatcher
	newDispatcher   func() tools.Dispatcher
	newBackend      func(string) (backend.Backend, error)
	cwd             string
	startupMessages  []string
	currentAction   atomic.Pointer[actionHandle]
	fifo            PromptFIFO
	pendingInject   atomic.Pointer[string]
	mu              sync.RWMutex
	state           string // "idle", "thinking", "calling: <tool>"
	reply           string // assistant text from the most recently completed turn
	envMu           sync.RWMutex
	env             map[string]string // session-scoped env vars
	changeMu        sync.Mutex
	changeCond      *sync.Cond
	submitMu        sync.Mutex // serializes Submit calls (commands + turns)
	warnedContext   bool       // true after a context-usage warning; cleared on compaction
	auditLog        *olog.Logger
}

// SetEnv stores a session-scoped variable and propagates it to the execute server.
func (s *agent) SetEnv(key, value string) {
	s.envMu.Lock()
	if s.env == nil {
		s.env = make(map[string]string)
	}
	s.env[key] = value
	s.envMu.Unlock()
	if s.dispatcher == nil {
		return
	}
	if srv, ok := s.dispatcher.GetServer("execute"); ok {
		if es, ok := srv.(tools.EnvSetter); ok {
			es.SetEnv(key, value)
		}
	}
}

// pushSessionEnv injects OLLIE_SESSION_ID into the execute server subprocess env.
func (s *agent) pushSessionEnv() {
	if s.dispatcher == nil || s.sessionID == "" {
		return
	}
	if srv, ok := s.dispatcher.GetServer("execute"); ok {
		if es, ok := srv.(tools.EnvSetter); ok {
			es.SetEnv("OLLIE_SESSION_ID", s.sessionID)
		}
	}
}

// pushLockDir sets the flock directory on the execute server to the session tmpdir.
func (s *agent) pushLockDir() {
	if s.dispatcher == nil || s.sessionID == "" {
		return
	}
	if srv, ok := s.dispatcher.GetServer("execute"); ok {
		if ls, ok := srv.(tools.LockDirSetter); ok {
			ls.SetLockDir(filepath.Join(ollieTmpDir(), s.sessionID))
		}
	}
}

var _ Core = (*agent)(nil) // compile-time interface check

var sweepTmpOnce sync.Once

// ollieTmpDir returns the base temp directory for session tmpdirs.
func ollieTmpDir() string {
	if p := os.Getenv("OLLIE_TMP_PATH"); p != "" {
		return p
	}
	return filepath.Join(os.TempDir(), "ollie")
}

// sweepStaleTmpDirs removes session tmpdirs left by a previous crash.
// All session tmpdirs are owned by a single process, so anything
// present at startup is stale.
func sweepStaleTmpDirs() {
	sweepTmpOnce.Do(func() {
		base := ollieTmpDir()
		os.RemoveAll(base)      //nolint:errcheck
		os.MkdirAll(base, 0700) //nolint:errcheck
	})
}

// NewAgentCore creates an agent from the given configuration.
func NewAgentCore(cfg AgentCoreConfig) Core {
	sweepStaleTmpDirs()
	if cfg.ModelName != "" {
		cfg.Backend.SetModel(cfg.ModelName)
	}
	if cfg.NewBackend == nil {
		cfg.NewBackend = backend.NewWithName
	}
	run := agentConfig{
		Backend:            cfg.Backend,
		preamble:           cfg.Env.preamble,
		Tools:              cfg.Env.tools,
		Exec:               cfg.Env.exec,
		ClassifyTool:       cfg.Env.classifyTool,
		ToolResultMaxBytes: defaultToolResultMaxBytes,
		GenerationParams:   cfg.Env.genParams,
	}
	if cfg.SessionID != "" {
		os.MkdirAll(filepath.Join(ollieTmpDir(), cfg.SessionID), 0700) //nolint:errcheck
	}

	log := cfg.Log
	if log == nil {
		log = olog.NewWriter("core", olog.LevelError+1, io.Discard, io.Discard)
	}

	a := &agent{
		session:         cfg.Session,
		cfg:         run,
		hooks:           cfg.Env.Hooks,
		log:             log,
		auditLog:        log.Sub("audit"),
		agentName:       cfg.AgentName,
		agentsDir:       cfg.AgentsDir,
		sessionsDir:     cfg.SessionsDir,
		sessionID:       cfg.SessionID,
		cwd:             paths.ExpandHome(cfg.CWD),
		startupMessages: cfg.Env.Messages,
		dispatcher:      cfg.Env.dispatcher,
		newDispatcher:   cfg.NewDispatcher,
		newBackend:      cfg.NewBackend,
		state:           "idle",
	}
	a.changeCond = sync.NewCond(&a.changeMu)
	a.pushSessionEnv()
	a.pushLockDir()
	a.cfg.TurnError = func(_ context.Context, errType, errMsg string) HookResult {
		// Use a detached context so the hook can safely write "stop" to ctl
		// without cancelling its own execution via the action context.
		hookCtx, cancel := context.WithTimeout(context.Background(), time.Duration(hookTimeout)*time.Second)
		defer cancel()
		return a.hooks.Run(hookCtx, HookTurnError, map[string]string{
			"session_id": a.sessionID,
			"cwd":        a.CWD(),
			"model":      a.cfg.Backend.Model(),
			"error_type": errType,
			"error":      errMsg,
		}, a.log)
	}
	return a
}

// Close releases resources for this session, including its tmpdir.
func (s *agent) Close() {
	s.log.Debug("Close() session=%q", s.sessionID)
	if s.sessionID != "" {
		os.RemoveAll(filepath.Join(ollieTmpDir(), s.sessionID)) //nolint:errcheck
	}
}

func (s *agent) AgentName() string {
	v := s.agentName
	s.log.Debug("AgentName() = %q", v)
	return v
}
func (s *agent) BackendName() string {
	v := s.cfg.Backend.Name()
	s.log.Debug("BackendName() = %q", v)
	return v
}
func (s *agent) ModelName() string {
	v := s.cfg.Backend.Model()
	s.log.Debug("ModelName() = %q", v)
	return v
}

func (s *agent) State() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *agent) notifyChange() {
	s.changeMu.Lock()
	s.changeCond.Broadcast()
	s.changeMu.Unlock()
}

// WaitChange blocks until the named field changes from current, then returns
// the new value. Returns ("", false) if ctx is cancelled.
func (s *agent) WaitChange(ctx context.Context, field, current string) (string, bool) {
	read := func() string {
		switch field {
		case WatchState:
			return s.State()
		case WatchUsage:
			return s.Usage()
		case WatchCtxSz:
			return s.CtxSz()
		case WatchCWD:
			return s.CWD()
		}
		return ""
	}

	// context.AfterFunc fires in a separate goroutine when ctx is done,
	// broadcasting to unblock any waiters.
	stop := context.AfterFunc(ctx, func() {
		s.changeMu.Lock()
		s.changeCond.Broadcast()
		s.changeMu.Unlock()
	})
	defer stop()

	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	for ctx.Err() == nil {
		if v := read(); v != current {
			return v, true
		}
		s.changeCond.Wait()
	}
	return "", false
}

func (s *agent) setState(state string) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
	s.log.Debug("state -> %q", state)
	s.notifyChange()
}

func (s *agent) Reply() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.log.Debug("Reply() len=%d", len(s.reply))
	return s.reply
}

// CWD returns the current working directory for tool execution.
func (s *agent) CWD() string {
	if s.cwd != "" {
		s.log.Debug("CWD() = %q", s.cwd)
		return s.cwd
	}
	wd, _ := os.Getwd()
	s.log.Debug("CWD() = %q (from getwd)", wd)
	return wd
}

// SetCWD changes the working directory for tool execution and updates the
// system prompt. Returns an error if the path does not exist.
func (s *agent) SetCWD(dir string) error {
	s.log.Debug("SetCWD(%q)", dir)
	dir = paths.ExpandHome(dir)
	if dir != "" {
		if _, err := os.Stat(dir); err != nil {
			return fmt.Errorf("cwd: %w", err)
		}
	}
	s.cwd = dir
	// Propagate to any tool server that knows how to handle it (e.g. execute).
	if s.dispatcher != nil {
		if srv, ok := s.dispatcher.GetServer("execute"); ok {
			if ws, ok := srv.(tools.CWDSetter); ok {
				ws.SetCWD(dir)
			}
		}
	}
	s.notifyChange()
	return nil
}

// SetSessionID renames the session. It updates the in-memory ID, renames
// persisted files on disk, and propagates to the execute server env.
func (s *agent) SetSessionID(newID string) error {
	s.log.Debug("SetSessionID(%q) old=%q", newID, s.sessionID)
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
	// Rename tmpdir so isread markers remain valid after rename.
	oldTemp := filepath.Join(ollieTmpDir(), oldID)
	newTemp := filepath.Join(ollieTmpDir(), newID)
	if _, err := os.Stat(oldTemp); err == nil {
		os.Rename(oldTemp, newTemp) //nolint:errcheck
	}
	s.pushSessionEnv()
	return nil
}

// defaultContextLength is used when the backend cannot report the model's
// actual context window (e.g. CodeWhisperer). 128k tokens is a safe default
// for modern models.
const defaultContextLength = 128000
const defaultToolResultMaxBytes = 8192

// autoCompactLimit returns the token threshold for auto-compaction (75%).
func (s *agent) autoCompactLimit(ctx context.Context) int {
	ctxLen := s.cfg.Backend.ContextLength(ctx)
	if ctxLen <= 0 {
		ctxLen = defaultContextLength
	}
	return ctxLen * 3 / 4
}

// autoWarnLimit returns the token threshold for a context-usage warning (60%).
func (s *agent) autoWarnLimit(ctx context.Context) int {
	ctxLen := s.cfg.Backend.ContextLength(ctx)
	if ctxLen <= 0 {
		ctxLen = defaultContextLength
	}
	return ctxLen * 3 / 5
}

// spawnContext assembles the agent context injected at each session refresh
// point (session start, post-clear, post-compaction). It combines the
// agent-specific prompt with any agentSpawn hook output.
func (s *agent) spawnContext(ctx context.Context, handler EventHandler) string {
	result := s.hooks.Run(ctx, HookAgentSpawn, map[string]string{
		"session_id": s.sessionID,
		"agent":      s.agentName,
		"cwd":        s.CWD(),
		"model":      s.cfg.Backend.Model(),
	}, s.log)
	if result.Warning != "" {
		handler(infoEvent(result.Warning))
	}
	var parts []string
	if result.Context != "" {
		parts = append(parts, result.Context)
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// runCompact executes a full compaction cycle: pre-hook, compact, spawn-context
// re-injection, post-hook. Returns (n compacted, error). Returns (0, nil) if
// the pre-hook blocked or there was nothing to compact. Caller manages setState.
func (s *agent) runCompact(ctx context.Context, trigger string, handler EventHandler) (int, error) {
	payload := map[string]string{"session_id": s.sessionID, "trigger": trigger, "cwd": s.CWD()}
	pre := s.hooks.Run(ctx, HookPreCompact, payload, s.log)
	if pre.Warning != "" {
		handler(infoEvent(pre.Warning))
	}
	if pre.Blocked {
		handler(infoEvent("compact cancelled by hook"))
		return 0, nil
	}
	if pre.Context != "" {
		s.session.appendUserMessage(pre.Context)
	}
	n, _, err := s.session.compact(ctx, s.cfg.Backend)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.auditLog.Debug("compact: removed %d messages trigger=%s session=%s", n, trigger, s.sessionID)
		s.warnedContext = false
		if sc := s.spawnContext(ctx, handler); sc != "" {
			s.session.appendUserMessage(sc)
		}
	}
	post := s.hooks.Run(ctx, HookPostCompact, payload, s.log)
	if post.Warning != "" {
		handler(infoEvent(post.Warning))
	}
	if post.Context != "" {
		s.session.appendUserMessage(post.Context)
	}
	return n, nil
}

func (s *agent) saveSession() {
	if s.session == nil || s.sessionID == "" || s.sessionsDir == "" {
		return
	}
	path := s.sessionsDir + "/" + s.sessionID + ".json"
	if err := s.session.saveTo(path, s.sessionID, s.agentName); err != nil {
		s.log.Error("session save: %v", err)
	}
}

func (s *agent) getActionCancel() context.CancelCauseFunc {
	if a := s.currentAction.Load(); a != nil {
		return a.cancel
	}
	return nil
}

// Interrupt cancels the current in-progress agent turn.
// Returns true if an action was running and was cancelled.
func (s *agent) Interrupt(cause error) bool {
	s.log.Debug("Interrupt() cause=%v", cause)
	if cancel := s.getActionCancel(); cancel != nil {
		cancel(cause)
		return true
	}
	return false
}

func (s *agent) Inject(prompt string) {
	// If an inject is already pending, fall back to the normal FIFO so nothing
	// is lost. Use CompareAndSwap to avoid a race between the nil check and store.
	if !s.pendingInject.CompareAndSwap(nil, &prompt) {
		s.fifo.Push(prompt)
		return
	}
	emit(s.cfg, Event{Role: "info", Content: "\n"})
	emit(s.cfg, Event{Role: "user", Content: prompt})
}

func (s *agent) injectRewrite(prompt string) {
	s.pendingInject.Store(&prompt)
	emit(s.cfg, Event{Role: "info", Content: "\n"})
	emit(s.cfg, Event{Role: "user", Content: prompt})
}

func (s *agent) Queue(prompt string) {
	s.log.Debug("Queue() len=%d", len(prompt))
	s.fifo.Push(prompt)
}

func (s *agent) PopQueue() (string, bool) {
	v, ok := s.fifo.Pop()
	s.log.Debug("PopQueue() ok=%v len=%d", ok, len(v))
	return v, ok
}

func (s *agent) IsRunning() bool {
	v := s.currentAction.Load() != nil
	s.log.Debug("IsRunning() = %v", v)
	return v
}

func (s *agent) CtxSz() string {
	if s.session == nil {
		s.log.Debug("CtxSz() no session")
		return "no active session"
	}
	ctxLen := s.cfg.Backend.ContextLength(context.Background())
	if ctxLen <= 0 {
		ctxLen = defaultContextLength
	}
	estimated := s.session.estimateTokens()
	pct := estimated * 100 / ctxLen
	v := fmt.Sprintf("%d / %d (%d%%)", estimated, ctxLen, pct)
	s.log.Debug("CtxSz() = %q", v)
	return v
}

func (s *agent) Cost() string {
	if s.session == nil {
		return "no active session"
	}
	return fmt.Sprintf("costLast=$%.4f\ncostSession=$%.4f\n",
		s.session.LastTurnCostUSD, s.session.SessionCostUSD)
}

func (s *agent) Usage() string {
	if s.session == nil {
		s.log.Debug("Usage() no session")
		return "no active session"
	}
	str := fmt.Sprintf("%d in, %d out, %d requests",
		s.session.TotalInputTokens, s.session.TotalOutputTokens,
		s.session.TotalRequests)
	if s.session.TotalCachedInputTokens > 0 {
		str += fmt.Sprintf(", %d cached", s.session.TotalCachedInputTokens)
	}
	if s.session.Estimated {
		str += " [estimated]"
	}
	s.log.Debug("Usage() = %q", str)
	return str
}

func (s *agent) Context() []backend.Message {
	s.mu.RLock()
	var msgs []backend.Message
	if s.session != nil {
		msgs = slices.Clone(s.session.history())
	}
	s.mu.RUnlock()
	if s.cfg.preamble != "" {
		msgs = append([]backend.Message{{Role: "system", Content: s.cfg.preamble}}, msgs...)
	}
	return msgs
}

func (s *agent) SystemPrompt() string {
	s.log.Debug("SystemPrompt() len=%d", len(s.cfg.preamble))
	return s.cfg.preamble
}

func (s *agent) GenerationParams() backend.GenerationParams {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.GenerationParams
}

func (s *agent) SetGenerationParams(params backend.GenerationParams) error {
	if s.IsRunning() {
		return fmt.Errorf("cannot change params while agent is running")
	}
	s.mu.Lock()
	s.cfg.GenerationParams = params
	s.mu.Unlock()
	return nil
}

func (s *agent) ListModels() string {
	s.log.Debug("ListModels()")
	models := s.cfg.Backend.Models(context.Background())
	slices.Sort(models)
	return strings.Join(models, "\n")
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
//
// Continuations (post-turn hook context, unconsumed inject, FIFO drain) are
// handled via an explicit loop rather than recursion to avoid stack growth.
func (s *agent) Submit(ctx context.Context, input string, handler EventHandler) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("panic: %v\n%s", r, debug.Stack())
			if a := s.currentAction.Swap(nil); a != nil {
				a.cancel(fmt.Errorf("%v", r))
			}
			s.setState("idle")
			handler(Event{Role: "error", Content: fmt.Sprintf("%v", r)})
		}
	}()
	s.log.Debug("Submit() input_len=%d running=%v", len(input), s.IsRunning())
	if input == "" {
		return
	}

	// Fast path: inject and FIFO push use atomics and are safe without
	// the submit lock. Handle them before acquiring submitMu so they
	// don't block behind a long-running turn or command.
	if s.IsRunning() {
		if s.handleCommand(ctx, input, handler) {
			return
		}
		s.fifo.Push(input)
		return
	}

	// Serialize commands and turns so that e.g. a /compact arriving via
	// ctl cannot race with an executeTurn arriving via prompt.
	s.submitMu.Lock()
	defer s.submitMu.Unlock()

	if s.handleCommand(ctx, input, handler) {
		return
	}
	if s.IsRunning() {
		s.fifo.Push(input)
		return
	}

	for input != "" && ctx.Err() == nil {
		input = s.executeTurn(ctx, input, handler)
	}
}

// executeTurn runs a single agent turn and returns the next prompt to execute,
// or "" if there is nothing more to do.
func (s *agent) executeTurn(ctx context.Context, input string, handler EventHandler) string {
	handler(Event{Role: "user", Content: input})

	hookResult := s.hooks.Run(ctx, HookPreTurn, map[string]string{
		"session_id": s.sessionID,
		"cwd":        s.CWD(),
		"prompt":     input,
	}, s.log)
	if hookResult.Blocked {
		handler(infoEvent("hook blocked prompt"))
		return ""
	}
	if hookResult.Warning != "" {
		handler(infoEvent(hookResult.Warning))
	}
	if hookResult.Context != "" {
		input += "\n" + hookResult.Context
	}

	// Snapshot session state before this turn modifies it. Restored on failure
	// so the session is clean for the next attempt.
	snapSession := s.session
	var snapMessages []backend.Message
	if s.session != nil {
		snapMessages = cloneMessages(s.session.messages)
	}

	if s.session == nil {
		for _, msg := range s.startupMessages {
			s.log.Debug("startup: %s", msg)
			handler(infoEvent(msg))
		}
		s.startupMessages = nil
		s.session = newSession(input)
		if sc := s.spawnContext(ctx, handler); sc != "" {
			s.session.appendUserMessage(sc)
		}
		s.session.appendUserMessage(input)
	} else {
		s.session.appendUserMessage(input)
	}

	actCtx, actCancel := context.WithCancelCause(ctx)
	handle := &actionHandle{cancel: actCancel}
	s.currentAction.Store(handle)
	s.setState("thinking")

	s.auditLog.Debug("turn: start input=%s session=%s", auditTruncate(input), s.sessionID)

	var replyBuf strings.Builder
	s.cfg.Output = func(ev Event) {
		switch ev.Role {
		case "assistant":
			replyBuf.WriteString(ev.Content)
		case "call":
			s.setState("calling: " + ev.Name)
			s.auditLog.Debug("call: %s %s", ev.Name, auditTruncate(string(ev.Content)))
		case "tool":
			s.setState("thinking")
			s.auditLog.Debug("result: %s %s", ev.Name, auditTruncate(ev.Content))
		case "limitretry":
			s.setState("limitretry")
		case "error":
			s.auditLog.Debug("error: %s", ev.Content)
		}
		if ev.Role == "usage" && s.session != nil {
			var in, out, est, cached, creation int
			var costUSD float64
			fmt.Sscanf(ev.Content, "%d %d %d %g %d %d", &in, &out, &est, &costUSD, &cached, &creation)
			s.session.addUsage(backend.Usage{
				InputTokens:         in,
				CachedInputTokens:   cached,
				CacheCreationTokens: creation,
				OutputTokens:        out,
				CostUSD:             costUSD,
			}, est != 0)
			s.notifyChange()
			if maxCostStr := os.Getenv("OLLIE_MAX_SESSION_COST"); maxCostStr != "" {
				if limit, ferr := strconv.ParseFloat(maxCostStr, 64); ferr == nil && limit > 0 && s.session.SessionCostUSD >= limit {
					handler(infoEvent(fmt.Sprintf("spending cap $%.2f reached — stopping", limit)))
					s.Interrupt(ErrInterrupted)
				}
			}
		}
		handler(ev)
	}
	s.cfg.PopInject = func() string {
		if p := s.pendingInject.Swap(nil); p != nil {
			return *p
		}
		return ""
	}
	s.cfg.PreTool = func(ctx context.Context, name string, args json.RawMessage) HookResult {
		return s.hooks.Run(ctx, HookPreTool, map[string]string{
			"session_id": s.sessionID,
			"cwd":        s.CWD(),
			"tool":       name,
			"args":       string(args),
		}, s.log)
	}
	s.cfg.PostTool = func(ctx context.Context, name string, args json.RawMessage, result string) HookResult {
		return s.hooks.Run(ctx, HookPostTool, map[string]string{
			"session_id": s.sessionID,
			"cwd":        s.CWD(),
			"tool":       name,
			"args":       string(args),
			"result":     result,
		}, s.log)
	}
	s.cfg.SaveSession = func() { s.saveSession() }
	s.cfg.AutoCompact = func(ctx context.Context) {
		if ctx.Err() != nil || s.session == nil {
			return
		}
		limit := s.autoCompactLimit(ctx)
		if limit <= 0 || s.session.estimateTokens() < limit {
			return
		}
		emit(s.cfg, Event{Role: "info", Content: "auto-compacting context...\n"})
		s.setState("compacting")
		if _, err := s.runCompact(ctx, "auto", handler); err != nil {
			panic(fmt.Sprintf("mid-turn auto-compact: %v", err))
		}
		s.setState("thinking")
	}

	// Warn once when context usage crosses 60%; compact at 75%.
	if s.session != nil {
		tokens := s.session.estimateTokens()
		if compactLimit := s.autoCompactLimit(ctx); compactLimit > 0 && tokens >= compactLimit {
			emit(s.cfg, Event{Role: "info", Content: "auto-compacting context...\n"})
			s.setState("compacting")
			if _, err := s.runCompact(ctx, "auto", handler); err != nil {
				panic(fmt.Sprintf("auto-compact: %v", err))
			}
			s.setState("thinking")
		} else if warnLimit := s.autoWarnLimit(ctx); warnLimit > 0 && tokens >= warnLimit && !s.warnedContext {
			ctxLen := s.cfg.Backend.ContextLength(ctx)
			if ctxLen <= 0 {
				ctxLen = defaultContextLength
			}
			pct := tokens * 100 / ctxLen
			emit(s.cfg, Event{Role: "info", Content: fmt.Sprintf("context at %d%% — will auto-compact at 75%%\n", pct)})
			s.warnedContext = true
		}
	}

	// Spending cap: reject before spending more tokens.
	if maxCostStr := os.Getenv("OLLIE_MAX_SESSION_COST"); maxCostStr != "" && s.session != nil {
		if limit, err := strconv.ParseFloat(maxCostStr, 64); err == nil && limit > 0 {
			if s.session.SessionCostUSD >= limit {
				handler(Event{Role: "error", Content: fmt.Sprintf("spending cap $%.2f reached (session total $%.4f)", limit, s.session.SessionCostUSD)})
				s.setState("idle")
				actCancel(nil)
				s.currentAction.CompareAndSwap(handle, nil)
				if snapSession == nil {
					s.session = nil
				} else {
					s.session.messages = snapMessages
				}
				return ""
			}
		}
	}

	if s.session != nil {
		s.session.resetTurnAccumulators()
	}

	// Run the turn, retrying once after compaction on context overflow.
	var (
		overflowRetried bool
		err             error
	)
	for {
		err = run(actCtx, s.cfg, s.session)
		actCancel(nil)
		s.currentAction.CompareAndSwap(handle, nil)

		if err == nil {
			break
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrInterrupted) {
			break
		}
		var ctxErr *backend.ContextOverflowError
		if !overflowRetried && errors.As(err, &ctxErr) && s.session != nil {
			overflowRetried = true
			s.session.messages = snapMessages
			emit(s.cfg, Event{Role: "info", Content: "context overflow — compacting and retrying...\n"})
			s.setState("compacting")
			if _, cerr := s.runCompact(ctx, "overflow", handler); cerr != nil {
				break
			}
			s.session.appendUserMessage(input)
			s.setState("thinking")
			s.session.resetTurnAccumulators()
			replyBuf.Reset()
			actCtx, actCancel = context.WithCancelCause(ctx)
			handle = &actionHandle{cancel: actCancel}
			s.currentAction.Store(handle)
			continue
		}
		break
	}

	s.mu.Lock()
	s.reply = replyBuf.String()
	s.mu.Unlock()
	replyBuf.Reset()
	s.setState("idle")

	if err != nil {
		// Keep completed work — only remove cancelled tool results.
		// Error results are valuable feedback for the agent.
		if s.session != nil {
			s.session.removeCancelledToolResults()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrInterrupted) {
			s.auditLog.Debug("turn: interrupted session=%s", s.sessionID)
			s.saveSession()
			return ""
		}
		handler(Event{Role: "error", Content: err.Error()})
		// Drain one FIFO item — the turnError hook may have queued a recovery prompt.
		if next, ok := s.fifo.Pop(); ok {
			return next
		}
		return ""
	}

	stopResult := s.hooks.Run(ctx, HookPostTurn, map[string]string{
		"session_id": s.sessionID,
		"cwd":        s.CWD(),
	}, s.log)
	if stopResult.Warning != "" {
		handler(infoEvent(stopResult.Warning))
	}
	if !stopResult.Blocked && stopResult.Context != "" && s.session != nil {
		s.session.appendUserMessage(stopResult.Context)
	}

	if s.session != nil {
		s.session.recordTurnCost(s.cfg.Backend.Model())
		if s.session.LastTurnCostUSD > 0 {
			handler(Event{Role: "info", Content: fmt.Sprintf("costLast=$%.4f\n", s.session.LastTurnCostUSD)})
		}
		s.auditLog.Debug("turn: end reply=%s cost=$%.4f session_total=$%.4f session=%s",
			auditTruncate(s.reply), s.session.LastTurnCostUSD, s.session.SessionCostUSD, s.sessionID)
		s.notifyChange()
	}
	s.saveSession()

	// Post-turn hook said "continue" — its context becomes the next prompt.
	if stopResult.Blocked && stopResult.Context != "" {
		return stopResult.Context
	}

	// Inject that was pending but never consumed (text-only response with no
	// tool calls) — treat it as the next user message.
	if p := s.pendingInject.Swap(nil); p != nil {
		return *p
	}

	// Drain one item from the FIFO; the outer loop handles the rest.
	if next, ok := s.fifo.Pop(); ok {
		return next
	}

	return ""
}


func toolInfosToBackend(infos []tools.ToolInfo) []backend.Tool {
	out := make([]backend.Tool, len(infos))
	for i, t := range infos {
		out[i] = backend.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}
	}
	return out
}

func extractToolResult(raw json.RawMessage) (text string, isError bool) {
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return string(raw), false
	}
	var parts []string
	for _, c := range result.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n"), result.IsError
}

