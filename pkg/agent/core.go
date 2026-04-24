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
	"runtime"
	"runtime/debug"
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

// BuildAgentEnv constructs an AgentEnv from a pre-configured Dispatcher and
// optional agent config. cwd sets the working directory reported in the
// system prompt; if empty, the process working directory is used.
// The caller is responsible for registering all servers on d before calling this.
func BuildAgentEnv(cfg *config.Config, d tools.Dispatcher, cwd string) AgentEnv {
	var messages []string

	allToolInfos, listErr := d.ListTools()
	if listErr != nil {
		messages = append(messages, fmt.Sprintf("list tools: %v", listErr))
	}

	serverOf := make(map[string]string, len(allToolInfos))
	for _, t := range allToolInfos {
		serverOf[t.Name] = t.Server
	}

	allTools := toolInfosToBackend(allToolInfos)

	hooks := Hooks{}
	agentPrompt := ""
	var genParams backend.GenerationParams
	if cfg != nil {
		for k, v := range cfg.Hooks {
			hooks[k] = []string(v)
		}
		agentPrompt = cfg.Prompt
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

	path := DefaultPromptsDir() + "/SYSTEM_PROMPT.md"
	data, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("loadSystemPrompt: %s: %v", path, err))
	}
	base := expandSystemPrompt(string(data), cwd)
	sp := base
	if agentPrompt != "" {
		if sp != "" {
			sp = sp + "\n\n" + agentPrompt
		} else {
			sp = agentPrompt
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


// DefaultPromptsDir returns the default directory for prompt templates.
func DefaultPromptsDir() string {
	return paths.CfgDir() + "/prompts"
}

func expandSystemPrompt(content, cwd string) string {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	isGit := "false"
	if _, err := os.Stat(filepath.Join(cwd, ".git")); err == nil {
		isGit = "true"
	}
	mapper := func(key string) string {
		switch key {
		case "PWD":
			return cwd
		case "PRIME_DATE":
			return time.Now().Format("2006-01-02")
		case "PRIME_PLATFORM":
			return strings.ToLower(runtime.GOOS)
		case "PRIME_IS_GIT_REPO":
			return isGit
		default:
			return os.Getenv(key)
		}
	}
	return os.Expand(content, mapper)
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

// AgentCoreConfig is the configuration for creating an agentCore.
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

// agentCore is the Core implementation. It owns all agent and session state
// but has no knowledge of how output is rendered.
type agentCore struct {
	session         *Session
	loopcfg         loopConfig
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
	agentPrompt      string // agent-specific prompt suffix; kept for system prompt rebuilds
	startupMessages  []string
	agentSpawnFired  bool
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
}

// SetEnv stores a session-scoped variable and propagates it to the execute server.
func (s *agentCore) SetEnv(key, value string) {
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
func (s *agentCore) pushSessionEnv() {
	if s.dispatcher == nil || s.sessionID == "" {
		return
	}
	if srv, ok := s.dispatcher.GetServer("execute"); ok {
		if es, ok := srv.(tools.EnvSetter); ok {
			es.SetEnv("OLLIE_SESSION_ID", s.sessionID)
		}
	}
}

var _ Core = (*agentCore)(nil) // compile-time interface check

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

// NewAgentCore creates an agentCore from the given configuration.
func NewAgentCore(cfg AgentCoreConfig) Core {
	sweepStaleTmpDirs()
	if cfg.ModelName != "" {
		cfg.Backend.SetModel(cfg.ModelName)
	}
	if cfg.NewBackend == nil {
		cfg.NewBackend = backend.NewWithName
	}
	loopcfg := loopConfig{
		Backend:          cfg.Backend,
		systemPrompt:     cfg.Env.systemPrompt,
		Tools:            cfg.Env.tools,
		Exec:             cfg.Env.exec,
		GenerationParams: cfg.Env.genParams,
	}
	if cfg.SessionID != "" {
		os.MkdirAll(filepath.Join(ollieTmpDir(), cfg.SessionID), 0700) //nolint:errcheck
	}

	log := cfg.Log
	if log == nil {
		log = olog.NewWriter("core", olog.LevelError+1, io.Discard, io.Discard)
	}

	a := &agentCore{
		session:         cfg.Session,
		loopcfg:         loopcfg,
		hooks:           cfg.Env.Hooks,
		log:             log,
		agentName:       cfg.AgentName,
		agentsDir:       cfg.AgentsDir,
		sessionsDir:     cfg.SessionsDir,
		sessionID:       cfg.SessionID,
		cwd:             paths.ExpandHome(cfg.CWD),
		agentPrompt:     cfg.Env.agentPrompt,
		startupMessages: cfg.Env.Messages,
		dispatcher:      cfg.Env.dispatcher,
		newDispatcher:   cfg.NewDispatcher,
		newBackend:      cfg.NewBackend,
		state:           "idle",
	}
	a.changeCond = sync.NewCond(&a.changeMu)
	a.pushSessionEnv()
	return a
}

// Close releases resources for this session, including its tmpdir.
func (s *agentCore) Close() {
	s.log.Debug("Close() session=%q", s.sessionID)
	if s.sessionID != "" {
		os.RemoveAll(filepath.Join(ollieTmpDir(), s.sessionID)) //nolint:errcheck
	}
}

func (s *agentCore) AgentName() string {
	v := s.agentName
	s.log.Debug("AgentName() = %q", v)
	return v
}
func (s *agentCore) BackendName() string {
	v := s.loopcfg.Backend.Name()
	s.log.Debug("BackendName() = %q", v)
	return v
}
func (s *agentCore) ModelName() string {
	v := s.loopcfg.Backend.Model()
	s.log.Debug("ModelName() = %q", v)
	return v
}

func (s *agentCore) State() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *agentCore) notifyChange() {
	s.changeMu.Lock()
	s.changeCond.Broadcast()
	s.changeMu.Unlock()
}

// WaitChange blocks until the named field changes from current, then returns
// the new value. Returns ("", false) if ctx is cancelled.
func (s *agentCore) WaitChange(ctx context.Context, field, current string) (string, bool) {
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

func (s *agentCore) setState(state string) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
	s.log.Debug("state -> %q", state)
	s.notifyChange()
}

func (s *agentCore) Reply() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.log.Debug("Reply() len=%d", len(s.reply))
	return s.reply
}

// CWD returns the current working directory for tool execution.
func (s *agentCore) CWD() string {
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
func (s *agentCore) SetCWD(dir string) error {
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
func (s *agentCore) SetSessionID(newID string) error {
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

// fireAgentSpawn runs the agentSpawn hook and appends any returned context to
// the system prompt. It is idempotent: it fires at most once per agent lifetime
// (reset when the active agent changes).
func (s *agentCore) fireAgentSpawn(ctx context.Context, handler EventHandler) {
	if s.agentSpawnFired {
		return
	}
	s.agentSpawnFired = true
	result := s.hooks.Run(ctx, HookAgentSpawn, map[string]string{
		"session_id": s.sessionID,
		"agent":      s.agentName,
		"cwd":        s.CWD(),
	}, s.log)
	// TODO: route hook info to debug/err file instead of chat
	// if result.Ran {
	// 	handler(infoEvent(hooksRan(1)))
	// }
	if result.Warning != "" {
		handler(infoEvent(result.Warning))
	}
	if result.Context != "" {
		s.loopcfg.systemPrompt += "\n\n---\n\n" + result.Context
	}
}

func (s *agentCore) saveSession() {
	if s.session == nil || s.sessionID == "" || s.sessionsDir == "" {
		return
	}
	path := s.sessionsDir + "/" + s.sessionID + ".json"
	if err := s.session.saveTo(path, s.sessionID, s.agentName); err != nil {
		s.log.Error("session save: %v", err)
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
	s.log.Debug("Interrupt() cause=%v", cause)
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
	s.log.Debug("Queue() len=%d", len(prompt))
	s.fifo.Push(prompt)
}

func (s *agentCore) PopQueue() (string, bool) {
	v, ok := s.fifo.Pop()
	s.log.Debug("PopQueue() ok=%v len=%d", ok, len(v))
	return v, ok
}

func (s *agentCore) IsRunning() bool {
	v := s.currentAction.Load() != nil
	s.log.Debug("IsRunning() = %v", v)
	return v
}

func (s *agentCore) CtxSz() string {
	if s.session == nil {
		s.log.Debug("CtxSz() no session")
		return "no active session"
	}
	ctxLen := s.loopcfg.Backend.ContextLength(context.Background())
	if ctxLen <= 0 {
		ctxLen = defaultContextLength
	}
	estimated := s.session.estimateTokens()
	pct := estimated * 100 / ctxLen
	v := fmt.Sprintf("%d / %d (%d%%)", estimated, ctxLen, pct)
	s.log.Debug("CtxSz() = %q", v)
	return v
}

func (s *agentCore) Usage() string {
	if s.session == nil {
		s.log.Debug("Usage() no session")
		return "no active session"
	}
	str := fmt.Sprintf("%d in, %d out, %d requests",
		s.session.TotalInputTokens, s.session.TotalOutputTokens,
		s.session.TotalRequests)
	if s.session.Estimated {
		str += " [estimated]"
	}
	s.log.Debug("Usage() = %q", str)
	return str
}

func (s *agentCore) SystemPrompt() string {
	s.log.Debug("SystemPrompt() len=%d", len(s.loopcfg.systemPrompt))
	return s.loopcfg.systemPrompt
}

func (s *agentCore) GenerationParams() backend.GenerationParams {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loopcfg.GenerationParams
}

func (s *agentCore) SetGenerationParams(params backend.GenerationParams) error {
	if s.IsRunning() {
		return fmt.Errorf("cannot change params while agent is running")
	}
	s.mu.Lock()
	s.loopcfg.GenerationParams = params
	s.mu.Unlock()
	return nil
}

func (s *agentCore) ListModels() string {
	s.log.Debug("ListModels()")
	models := s.loopcfg.Backend.Models(context.Background())
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
func (s *agentCore) Submit(ctx context.Context, input string, handler EventHandler) {
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
func (s *agentCore) executeTurn(ctx context.Context, input string, handler EventHandler) string {
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

	s.setState("thinking")

	if s.session == nil {
		for _, msg := range s.startupMessages {
			s.log.Debug("startup: %s", msg)
			handler(infoEvent(msg))
		}
		s.startupMessages = nil
		s.fireAgentSpawn(ctx, handler)
		s.session = newSession(input)
	} else {
		s.session.appendUserMessage(input)
	}

	actCtx, actCancel := context.WithCancelCause(ctx)
	handle := &actionHandle{cancel: actCancel}
	s.currentAction.Store(handle)

	var replyBuf strings.Builder
	s.loopcfg.Output = func(ev Event) {
		switch ev.Role {
		case "assistant":
			replyBuf.WriteString(ev.Content)
		case "call":
			s.setState("calling: " + ev.Name)
		case "tool":
			s.setState("thinking")
		}
		if ev.Role == "usage" && s.session != nil {
			var in, out, est int
			fmt.Sscanf(ev.Content, "%d %d %d", &in, &out, &est)
			s.session.addUsage(backend.Usage{InputTokens: in, OutputTokens: out}, est != 0)
			s.notifyChange()
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
			payload := map[string]string{"session_id": s.sessionID, "trigger": "auto", "cwd": s.CWD()}
			pre := s.hooks.Run(ctx, HookPreCompact, payload, s.log)
			// TODO: route hook info to debug/err file instead of chat
			// if pre.Ran {
			// 	handler(infoEvent(hooksRan(1)))
			// }
			if pre.Warning != "" {
				handler(infoEvent(pre.Warning))
			}
			if !pre.Blocked {
				if pre.Context != "" {
					s.session.appendUserMessage(pre.Context)
				}
				emit(s.loopcfg, Event{Role: "info", Content: "auto-compacting context...\n"})
				s.setState("compacting")
				if _, _, err := s.session.compact(ctx, s.loopcfg.Backend); err != nil {
					panic(fmt.Sprintf("auto-compact: %v", err))
				}
				s.setState("thinking")
				post := s.hooks.Run(ctx, HookPostCompact, payload, s.log)
				// TODO: route hook info to debug/err file instead of chat
				// if post.Ran {
				// 	handler(infoEvent(hooksRan(1)))
				// }
				if post.Warning != "" {
					handler(infoEvent(post.Warning))
				}
				if post.Context != "" {
					s.session.appendUserMessage(post.Context)
				}
			}
		}
	}

	err := run(actCtx, s.loopcfg, s.session)
	actCancel(nil)
	s.currentAction.CompareAndSwap(handle, nil)

	s.mu.Lock()
	s.reply = replyBuf.String()
	s.mu.Unlock()
	replyBuf.Reset()
	s.setState("idle")

	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrInterrupted) {
		handler(Event{Role: "error", Content: err.Error()})
	}

	stopResult := s.hooks.Run(ctx, HookPostTurn, map[string]string{
		"session_id": s.sessionID,
		"cwd":        s.CWD(),
	}, s.log)
	if stopResult.Warning != "" {
		handler(infoEvent(stopResult.Warning))
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

