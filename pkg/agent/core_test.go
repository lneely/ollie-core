package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ollie/pkg/backend"
	"ollie/pkg/config"
	"ollie/pkg/tools"
)

// --- mock backend ---

type mockBackend struct {
	mu      sync.Mutex
	name    string
	model   string
	ctxLen  int
	models  []string
	respond func(context.Context, []backend.Message, []backend.Tool, backend.GenerationParams) (<-chan backend.StreamEvent, error)
}

func (m *mockBackend) ChatStream(ctx context.Context, msgs []backend.Message, ts []backend.Tool, p backend.GenerationParams) (<-chan backend.StreamEvent, error) {
	m.mu.Lock()
	fn := m.respond
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, msgs, ts, p)
	}
	return textStream("ok"), nil
}
func (m *mockBackend) Name() string                        { return m.name }
func (m *mockBackend) DefaultModel() string                { return m.model }
func (m *mockBackend) Model() string                       { m.mu.Lock(); defer m.mu.Unlock(); return m.model }
func (m *mockBackend) SetModel(s string)                   { m.mu.Lock(); m.model = s; m.mu.Unlock() }
func (m *mockBackend) ContextLength(_ context.Context) int { return m.ctxLen }
func (m *mockBackend) Models(_ context.Context) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.models
}

// textStream returns a single-event stream carrying the given text.
func textStream(text string) <-chan backend.StreamEvent {
	ch := make(chan backend.StreamEvent, 1)
	ch <- backend.StreamEvent{
		Content:    text,
		Done:       true,
		StopReason: "stop",
		Usage:      backend.Usage{InputTokens: 10, OutputTokens: 5},
	}
	close(ch)
	return ch
}

// blockedStream returns a channel that delivers a response when unblock is
// closed, or drains silently if ctx is cancelled first.
func blockedStream(ctx context.Context, unblock <-chan struct{}) <-chan backend.StreamEvent {
	ch := make(chan backend.StreamEvent, 1)
	go func() {
		defer close(ch)
		select {
		case <-ctx.Done():
		case <-unblock:
			ch <- backend.StreamEvent{Content: "ok", Done: true, StopReason: "stop"}
		}
	}()
	return ch
}

// --- test helpers ---

func defaultBE() *mockBackend {
	return &mockBackend{name: "mock", model: "test", ctxLen: 128000}
}

// newCore builds a minimal *agentCore for tests, bypassing loadSystemPrompt
// by directly setting systemPrompt on AgentEnv.
func newCore(t *testing.T, be backend.Backend, hooks Hooks) *agentCore {
	t.Helper()
	if be == nil {
		be = defaultBE()
	}
	if hooks == nil {
		hooks = Hooks{}
	}
	env := AgentEnv{
		Hooks:        hooks,
		systemPrompt: "test system prompt",
	}
	c := NewAgentCore(AgentCoreConfig{
		Backend:       be,
		AgentName:     "test",
		AgentsDir:     t.TempDir(),
		SessionsDir:   t.TempDir(),
		SessionID:     NewSessionID(),
		CWD:           t.TempDir(),
		Env:           env,
		NewDispatcher: tools.NewDispatcher,
	})
	t.Cleanup(c.Close)
	return c.(*agentCore)
}

// collectEvents runs Submit synchronously and returns all emitted events.
func collectEvents(ctx context.Context, c Core, input string) []Event {
	var evs []Event
	c.Submit(ctx, input, func(ev Event) { evs = append(evs, ev) })
	return evs
}

// byRole returns the Content of every event with the given role.
func byRole(evs []Event, role string) []string {
	var out []string
	for _, ev := range evs {
		if ev.Role == role {
			out = append(out, ev.Content)
		}
	}
	return out
}

// waitState blocks until c.State() == want, failing after 2 s.
func waitState(t *testing.T, c Core, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for c.State() != want {
		if _, ok := c.WaitChange(ctx, WatchState, c.State()); !ok {
			t.Fatalf("timed out waiting for state %q (current: %q)", want, c.State())
		}
	}
}

// --- Submit: happy path ---

func TestSubmit_HappyPath(t *testing.T) {
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return textStream("hello back"), nil
	}
	c := newCore(t, be, nil)

	evs := collectEvents(context.Background(), c, "hello")

	if got := byRole(evs, "user"); len(got) == 0 || got[0] != "hello" {
		t.Errorf("user event: %v", got)
	}
	if got := byRole(evs, "assistant"); len(got) == 0 || got[0] != "hello back" {
		t.Errorf("assistant event: %v", got)
	}
	if got := c.State(); got != "idle" {
		t.Errorf("State() = %q; want idle", got)
	}
	if got := c.Reply(); got != "hello back" {
		t.Errorf("Reply() = %q; want %q", got, "hello back")
	}
	if c.IsRunning() {
		t.Error("IsRunning() = true after turn; want false")
	}
}

// --- State transitions ---

func TestSubmit_StateTransitions(t *testing.T) {
	unblock := make(chan struct{})
	be := defaultBE()
	be.respond = func(ctx context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return blockedStream(ctx, unblock), nil
	}
	c := newCore(t, be, nil)

	if got := c.State(); got != "idle" {
		t.Fatalf("initial state = %q; want idle", got)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Submit(context.Background(), "hello", func(Event) {})
	}()

	waitState(t, c, "thinking")
	close(unblock)
	<-done

	if got := c.State(); got != "idle" {
		t.Errorf("final state = %q; want idle", got)
	}
}

func TestSubmit_ToolCallStateTransitions(t *testing.T) {
	const toolName = "my_tool"
	callCount := 0
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		if callCount == 1 {
			ch := make(chan backend.StreamEvent, 1)
			ch <- backend.StreamEvent{
				ToolCalls:  []backend.ToolCall{{Name: toolName, Arguments: json.RawMessage(`{}`)}},
				Done:       true,
				StopReason: "tool_calls",
			}
			close(ch)
			return ch, nil
		}
		return textStream("done"), nil
	}

	c := newCore(t, be, nil)

	var stateAtExec string
	c.loopcfg.Exec = func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		stateAtExec = c.State()
		return `{}`, nil
	}

	collectEvents(context.Background(), c, "run tool")

	wantState := "calling: " + toolName
	if stateAtExec != wantState {
		t.Errorf("state during tool exec = %q; want %q", stateAtExec, wantState)
	}
	if got := c.State(); got != "idle" {
		t.Errorf("final state = %q; want idle", got)
	}
}

// --- preTurn hook ---

func TestSubmit_PreTurnHookBlocks(t *testing.T) {
	callCount := 0
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		return textStream("should not be called"), nil
	}
	c := newCore(t, be, Hooks{HookPreTurn: []string{"exit 2"}})

	evs := collectEvents(context.Background(), c, "hello")

	if callCount > 0 {
		t.Error("backend called despite preTurn hook blocking")
	}
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "hook blocked prompt") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'hook blocked prompt' info event; got: %v", byRole(evs, "info"))
	}
}

func TestSubmit_PreTurnHookContext(t *testing.T) {
	var lastUserMsg string
	be := defaultBE()
	be.respond = func(_ context.Context, msgs []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		for _, m := range msgs {
			if m.Role == "user" {
				lastUserMsg = m.Content
			}
		}
		return textStream("ok"), nil
	}
	c := newCore(t, be, Hooks{HookPreTurn: []string{`echo "extra context"`}})

	collectEvents(context.Background(), c, "base prompt")

	if !strings.Contains(lastUserMsg, "extra context") {
		t.Errorf("user message %q does not contain hook-injected context", lastUserMsg)
	}
}

// --- postTurn hook continuation ---

func TestSubmit_PostTurnHookContinue(t *testing.T) {
	flagFile := filepath.Join(t.TempDir(), "fired")
	hookScript := fmt.Sprintf(
		`if [ ! -f %q ]; then touch %q; printf "auto-continue" >&2; exit 2; fi`,
		flagFile, flagFile,
	)
	callCount := 0
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		return textStream(fmt.Sprintf("response %d", callCount)), nil
	}
	c := newCore(t, be, Hooks{HookPostTurn: []string{hookScript}})

	collectEvents(context.Background(), c, "first prompt")

	if callCount != 2 {
		t.Errorf("backend called %d times; want 2 (original + hook continuation)", callCount)
	}
}

// --- FIFO drain ---

func TestSubmit_FIFODrain(t *testing.T) {
	callCount := 0
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		return textStream("ok"), nil
	}
	c := newCore(t, be, nil)
	c.Queue("second")
	c.Queue("third")

	collectEvents(context.Background(), c, "first")

	if callCount != 3 {
		t.Errorf("backend called %d times; want 3 (first + second + third)", callCount)
	}
}

// --- pendingInject as next turn ---

// TestSubmit_PendingInjectAsNextTurn verifies that a pendingInject left
// unconsumed (no tool calls) becomes the next prompt after the turn.
func TestSubmit_PendingInjectAsNextTurn(t *testing.T) {
	callCount := 0
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		return textStream("ok"), nil
	}
	c := newCore(t, be, nil)

	inject := "injected follow-up"
	c.pendingInject.Store(&inject)

	collectEvents(context.Background(), c, "first")

	if callCount != 2 {
		t.Errorf("backend called %d times; want 2 (first turn + inject turn)", callCount)
	}
}

// --- Interrupt ---

func TestInterrupt_Running(t *testing.T) {
	unblock := make(chan struct{})
	be := defaultBE()
	be.respond = func(ctx context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return blockedStream(ctx, unblock), nil
	}
	c := newCore(t, be, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Submit(context.Background(), "hello", func(Event) {})
	}()

	waitState(t, c, "thinking")

	if !c.Interrupt(ErrInterrupted) {
		t.Error("Interrupt() = false; want true (action was running)")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit did not return after Interrupt")
	}
	if got := c.State(); got != "idle" {
		t.Errorf("State() = %q after interrupt; want idle", got)
	}
	if c.IsRunning() {
		t.Error("IsRunning() = true after interrupt; want false")
	}
}

func TestInterrupt_Idle(t *testing.T) {
	c := newCore(t, nil, nil)
	if c.Interrupt(ErrInterrupted) {
		t.Error("Interrupt() = true when idle; want false")
	}
}

// --- Submit while running ---

func TestSubmit_WhileRunning(t *testing.T) {
	unblock := make(chan struct{})
	be := defaultBE()
	be.respond = func(ctx context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return blockedStream(ctx, unblock), nil
	}
	c := newCore(t, be, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Submit(context.Background(), "first", func(Event) {})
	}()

	waitState(t, c, "thinking")

	// Concurrent Submit must not start a new turn — always goes to FIFO.
	c.Submit(context.Background(), "concurrent", func(Event) {})

	if stored := c.pendingInject.Load(); stored != nil {
		t.Errorf("concurrent Submit set pendingInject %q; want FIFO only", *stored)
	}
	got, inFIFO := c.PopQueue()
	if !inFIFO || got != "concurrent" {
		t.Errorf("concurrent Submit PopQueue = %q, %v; want %q, true", got, inFIFO, "concurrent")
	}

	close(unblock)
	<-done
}

// --- compaction ---

func TestManualCompact(t *testing.T) {
	callCount := 0
	var stateAtCompact string
	be := defaultBE()
	c := newCore(t, be, nil)
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		stateAtCompact = c.State()
		return textStream("summary or answer"), nil
	}

	// Seed a session with more than 4 messages so compact() doesn't short-circuit.
	c.session = newSession("goal")
	for i := range 5 {
		c.session.messages = append(c.session.messages,
			backend.Message{Role: "assistant", Content: fmt.Sprintf("response %d with enough text to count", i)},
			backend.Message{Role: "user", Content: fmt.Sprintf("follow up %d", i)},
		)
	}
	before := len(c.session.messages)

	evs := collectEvents(context.Background(), c, "/compact")

	if c.session == nil {
		t.Fatal("session nil after compact")
	}
	if after := len(c.session.messages); after >= before {
		t.Errorf("messages: before=%d after=%d; want fewer after compact", before, after)
	}
	if callCount == 0 {
		t.Error("backend not called for compaction")
	}
	if stateAtCompact != "compacting" {
		t.Errorf("state during compact = %q; want compacting", stateAtCompact)
	}
	if got := c.State(); got != "idle" {
		t.Errorf("state after compact = %q; want idle", got)
	}
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "compacted") {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'compacted' info event; got: %v", byRole(evs, "info"))
	}
}

func TestAutoCompact(t *testing.T) {
	callCount := 0
	var stateAtCompact string
	// ctxLen=10 → autoCompactLimit = 7 tokens; our seeded session exceeds this.
	be := &mockBackend{name: "mock", model: "test", ctxLen: 10}
	c := newCore(t, be, nil)
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		if callCount == 1 {
			stateAtCompact = c.State()
			return textStream("summary text for compaction"), nil
		}
		return textStream("answer"), nil
	}

	c.session = newSession("goal")
	for range 3 {
		c.session.messages = append(c.session.messages,
			backend.Message{Role: "assistant", Content: "a long response that exceeds seven tokens of content"},
			backend.Message{Role: "user", Content: "follow up question with enough text to push over the limit"},
		)
	}

	collectEvents(context.Background(), c, "next prompt")

	if callCount < 2 {
		t.Errorf("backend called %d times; want ≥2 (compact + turn)", callCount)
	}
	if stateAtCompact != "compacting" {
		t.Errorf("state during auto-compact = %q; want compacting", stateAtCompact)
	}
}

// --- agentSpawn ---

func TestAgentSpawnFiresOnce(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "spawned")
	hookScript := fmt.Sprintf(`printf "spawn\n" >> %q`, logFile)
	c := newCore(t, nil, Hooks{HookAgentSpawn: []string{hookScript}})

	collectEvents(context.Background(), c, "first")
	collectEvents(context.Background(), c, "second")

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("spawn log not written: %v", err)
	}
	lines := strings.Count(string(data), "\n")
	if lines != 1 {
		t.Errorf("agentSpawn hook fired %d time(s); want 1", lines)
	}
}

// --- loadSystemPrompt ---

func TestLoadSystemPrompt_MissingFile(t *testing.T) {
	t.Setenv("OLLIE_CFG_PATH", t.TempDir()) // no SYSTEM_PROMPT.md written

	defer func() {
		if recover() == nil {
			t.Error("expected panic from loadSystemPrompt; got none")
		}
	}()
	loadSystemPrompt("")
}

// --- panic recovery ---

func TestSubmit_PanicRecovery(t *testing.T) {
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		panic("backend exploded")
	}
	c := newCore(t, be, nil)

	var errEvents []Event
	c.Submit(context.Background(), "hello", func(ev Event) {
		if ev.Role == "error" {
			errEvents = append(errEvents, ev)
		}
	})

	if len(errEvents) == 0 {
		t.Error("no error event after panic; expected one")
	}
	if got := c.State(); got != "idle" {
		t.Errorf("State() = %q after panic; want idle", got)
	}
	if c.IsRunning() {
		t.Error("IsRunning() = true after panic; want false")
	}
}

// --- commands ---

func TestCommand_Help(t *testing.T) {
	c := newCore(t, nil, nil)
	evs := collectEvents(context.Background(), c, "/help")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "Available commands") {
			found = true
		}
	}
	if !found {
		t.Errorf("/help: 'Available commands' not found in info events: %v", byRole(evs, "info"))
	}
}

func TestCommand_Clear(t *testing.T) {
	c := newCore(t, nil, nil)
	collectEvents(context.Background(), c, "first turn")
	if c.session == nil {
		t.Fatal("session nil after first turn")
	}
	oldID := c.sessionID
	collectEvents(context.Background(), c, "/clear")
	if c.session != nil {
		t.Error("session not nil after /clear")
	}
	if c.sessionID != oldID {
		t.Errorf("sessionID changed after /clear: %q → %q; want unchanged", oldID, c.sessionID)
	}
}

// --- CWD ---

func TestSetCWD(t *testing.T) {
	c := newCore(t, nil, nil)
	dir := t.TempDir()
	if err := c.SetCWD(dir); err != nil {
		t.Fatalf("SetCWD(%q): %v", dir, err)
	}
	if got := c.CWD(); got != dir {
		t.Errorf("CWD() = %q; want %q", got, dir)
	}
}

func TestSetCWD_NonExistent(t *testing.T) {
	c := newCore(t, nil, nil)
	if err := c.SetCWD("/nonexistent/path/xyz/abc"); err == nil {
		t.Error("SetCWD with nonexistent path returned nil; want error")
	}
}

// --- Queue / PopQueue ---

func TestQueuePopQueue(t *testing.T) {
	c := newCore(t, nil, nil)
	c.Queue("a")
	c.Queue("b")

	if got, ok := c.PopQueue(); !ok || got != "a" {
		t.Errorf("PopQueue() = %q, %v; want %q, true", got, ok, "a")
	}
	if got, ok := c.PopQueue(); !ok || got != "b" {
		t.Errorf("PopQueue() = %q, %v; want %q, true", got, ok, "b")
	}
	if got, ok := c.PopQueue(); ok {
		t.Errorf("PopQueue on empty = %q, true; want empty, false", got)
	}
}

// --- shell ! command ---

func TestShellCommand_Basic(t *testing.T) {
	c := newCore(t, nil, nil)
	evs := collectEvents(context.Background(), c, "!echo shellout")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "shellout") {
			found = true
		}
	}
	if !found {
		t.Errorf("!echo shellout: output not in info events: %v", byRole(evs, "info"))
	}
	if c.IsRunning() {
		t.Error("IsRunning() = true after shell command; want false")
	}
}

func TestShellCommand_Empty(t *testing.T) {
	c := newCore(t, nil, nil)
	// "!" with no command must return without starting a turn.
	evs := collectEvents(context.Background(), c, "!")
	if c.session != nil {
		t.Error("session created by empty '!' command; want nil")
	}
	_ = evs
}

// --- /i and /irw ---

func TestCommand_I_Empty(t *testing.T) {
	c := newCore(t, nil, nil)
	evs := collectEvents(context.Background(), c, "/i")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "error") {
			found = true
		}
	}
	if !found {
		t.Errorf("/i with no args: expected error event, got: %v", byRole(evs, "info"))
	}
}

func TestCommand_I_SetsInject(t *testing.T) {
	c := newCore(t, nil, nil)
	collectEvents(context.Background(), c, "/i my inject")
	p := c.pendingInject.Load()
	if p == nil || *p != "my inject" {
		t.Errorf("pendingInject = %v; want %q", p, "my inject")
	}
}

func TestCommand_I_FallsToFIFOWhenFull(t *testing.T) {
	c := newCore(t, nil, nil)
	existing := "first"
	c.pendingInject.Store(&existing)
	collectEvents(context.Background(), c, "/i second")
	if got, ok := c.PopQueue(); !ok || got != "second" {
		t.Errorf("FIFO after /i with full inject = %q, %v; want %q, true", got, ok, "second")
	}
}

func TestCommand_IRW_Empty(t *testing.T) {
	c := newCore(t, nil, nil)
	evs := collectEvents(context.Background(), c, "/irw")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "error") {
			found = true
		}
	}
	if !found {
		t.Errorf("/irw with no args: expected error event, got: %v", byRole(evs, "info"))
	}
}

func TestCommand_IRW_OverwritesInject(t *testing.T) {
	c := newCore(t, nil, nil)
	existing := "old"
	c.pendingInject.Store(&existing)
	collectEvents(context.Background(), c, "/irw new inject")
	p := c.pendingInject.Load()
	if p == nil || *p != "new inject" {
		t.Errorf("pendingInject after /irw = %v; want %q", p, "new inject")
	}
}

// --- /compact additional paths ---

func TestCommand_Compact_WhileRunning(t *testing.T) {
	unblock := make(chan struct{})
	be := defaultBE()
	be.respond = func(ctx context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return blockedStream(ctx, unblock), nil
	}
	c := newCore(t, be, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Submit(context.Background(), "hello", func(Event) {})
	}()
	waitState(t, c, "thinking")

	evs := collectEvents(context.Background(), c, "/compact")

	close(unblock)
	<-done

	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "error") {
			found = true
		}
	}
	if !found {
		t.Errorf("/compact while running: expected error event, got: %v", byRole(evs, "info"))
	}
}

func TestCommand_Compact_NilSession(t *testing.T) {
	c := newCore(t, nil, nil)
	evs := collectEvents(context.Background(), c, "/compact")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "nothing to compact") {
			found = true
		}
	}
	if !found {
		t.Errorf("/compact with nil session: expected 'nothing to compact', got: %v", byRole(evs, "info"))
	}
}

func TestCommand_Compact_PreHookBlocks(t *testing.T) {
	c := newCore(t, nil, Hooks{HookPreCompact: []string{"exit 2"}})
	c.session = newSession("goal")
	for i := range 5 {
		c.session.messages = append(c.session.messages,
			backend.Message{Role: "assistant", Content: fmt.Sprintf("response %d", i)},
			backend.Message{Role: "user", Content: fmt.Sprintf("follow up %d", i)},
		)
	}
	evs := collectEvents(context.Background(), c, "/compact")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "cancelled") {
			found = true
		}
	}
	if !found {
		t.Errorf("/compact with blocking preHook: expected 'cancelled', got: %v", byRole(evs, "info"))
	}
}

// --- /clear while running ---

func TestCommand_Clear_WhileRunning(t *testing.T) {
	unblock := make(chan struct{})
	be := defaultBE()
	be.respond = func(ctx context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return blockedStream(ctx, unblock), nil
	}
	c := newCore(t, be, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Submit(context.Background(), "hello", func(Event) {})
	}()
	waitState(t, c, "thinking")

	evs := collectEvents(context.Background(), c, "/clear")

	close(unblock)
	<-done

	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "error") {
			found = true
		}
	}
	if !found {
		t.Errorf("/clear while running: expected error event, got: %v", byRole(evs, "info"))
	}
}

// --- executeTurn: backend error ---

func TestSubmit_BackendError(t *testing.T) {
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return nil, fmt.Errorf("backend unavailable")
	}
	c := newCore(t, be, nil)
	evs := collectEvents(context.Background(), c, "hello")
	found := false
	for _, ev := range evs {
		if ev.Role == "error" && strings.Contains(ev.Content, "backend unavailable") {
			found = true
		}
	}
	if !found {
		t.Errorf("backend error: expected error event, got: %v", evs)
	}
	if got := c.State(); got != "idle" {
		t.Errorf("State() = %q after backend error; want idle", got)
	}
}

// --- executeTurn: startup messages ---

func TestSubmit_StartupMessages(t *testing.T) {
	c := newCore(t, nil, nil)
	c.startupMessages = []string{"startup msg 1", "startup msg 2"}
	evs := collectEvents(context.Background(), c, "hello")
	found := 0
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "startup msg") {
			found++
		}
	}
	if found != 2 {
		t.Errorf("startup messages: found %d info events; want 2; got: %v", found, byRole(evs, "info"))
	}
	if c.startupMessages != nil {
		t.Error("startupMessages not cleared after first turn")
	}
}

// --- loop: stream interrupted without Done ---

func TestRun_StreamInterrupted(t *testing.T) {
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		ch := make(chan backend.StreamEvent)
		close(ch) // close without sending Done=true
		return ch, nil
	}
	c := newCore(t, be, nil)
	evs := collectEvents(context.Background(), c, "hello")
	found := false
	for _, ev := range evs {
		if ev.Role == "error" {
			found = true
		}
	}
	if !found {
		t.Errorf("stream interrupted: expected error event, got: %v", evs)
	}
}

// --- loop: unknown stop reason ---

func TestRun_UnknownStopReason(t *testing.T) {
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		ch := make(chan backend.StreamEvent, 1)
		ch <- backend.StreamEvent{Done: true, StopReason: "max_completion_tokens", Content: "partial"}
		close(ch)
		return ch, nil
	}
	c := newCore(t, be, nil)
	evs := collectEvents(context.Background(), c, "hello")
	found := false
	for _, ev := range evs {
		if ev.Role == "error" {
			found = true
		}
	}
	if !found {
		t.Errorf("unknown stop reason: expected error event, got: %v", evs)
	}
}

// --- loop: tool call with empty name ---

func TestRun_ToolEmptyName(t *testing.T) {
	callCount := 0
	be := defaultBE()
	be.respond = func(_ context.Context, msgs []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		if callCount == 1 {
			ch := make(chan backend.StreamEvent, 1)
			ch <- backend.StreamEvent{
				ToolCalls:  []backend.ToolCall{{Name: "", Arguments: json.RawMessage(`{}`)}},
				Done:       true,
				StopReason: "tool_calls",
			}
			close(ch)
			return ch, nil
		}
		return textStream("done"), nil
	}
	c := newCore(t, be, nil)
	collectEvents(context.Background(), c, "run empty tool")
	if callCount != 2 {
		t.Errorf("backend called %d times; want 2 (empty-tool + follow-up)", callCount)
	}
	if got := c.State(); got != "idle" {
		t.Errorf("State() = %q after empty-tool turn; want idle", got)
	}
}

// --- loop: no tool executor configured ---

func TestRun_NoExec(t *testing.T) {
	callCount := 0
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		if callCount == 1 {
			ch := make(chan backend.StreamEvent, 1)
			ch <- backend.StreamEvent{
				ToolCalls:  []backend.ToolCall{{Name: "my_tool", Arguments: json.RawMessage(`{}`)}},
				Done:       true,
				StopReason: "tool_calls",
			}
			close(ch)
			return ch, nil
		}
		return textStream("done"), nil
	}
	c := newCore(t, be, nil)
	c.loopcfg.Exec = nil // no executor
	collectEvents(context.Background(), c, "run tool")
	if callCount != 2 {
		t.Errorf("backend called %d times; want 2", callCount)
	}
}

// --- hooks: non-zero non-two exit code ---

func TestHook_NonZeroExitCode(t *testing.T) {
	callCount := 0
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		return textStream("ok"), nil
	}
	// exit 1 is a non-blocking warning: turn should still proceed.
	c := newCore(t, be, Hooks{HookPreTurn: []string{"exit 1"}})
	collectEvents(context.Background(), c, "hello")
	if callCount == 0 {
		t.Error("backend not called after exit-1 preTurn hook; want call (non-blocking)")
	}
}

// --- hooks: context cancelled mid-hook ---

func TestHook_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// Sleep-10 hook is killed when ctx times out; must not deadlock.
	c := newCore(t, nil, Hooks{HookPreTurn: []string{"sleep 10"}})
	collectEvents(ctx, c, "hello") // returns when ctx expires
}

// --- /backend ---

func TestCommand_Backend_Show(t *testing.T) {
	c := newCore(t, nil, nil)
	evs := collectEvents(context.Background(), c, "/backend")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "mock") {
			found = true
		}
	}
	if !found {
		t.Errorf("/backend: backend name not in info events: %v", byRole(evs, "info"))
	}
}

func TestCommand_Backend_WhileRunning(t *testing.T) {
	unblock := make(chan struct{})
	be := defaultBE()
	be.respond = func(ctx context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return blockedStream(ctx, unblock), nil
	}
	c := newCore(t, be, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Submit(context.Background(), "hello", func(Event) {})
	}()
	waitState(t, c, "thinking")

	evs := collectEvents(context.Background(), c, "/backend newbackend")

	close(unblock)
	<-done

	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "error") {
			found = true
		}
	}
	if !found {
		t.Errorf("/backend while running: expected error event, got: %v", byRole(evs, "info"))
	}
}

// --- /model ---

func TestCommand_Model_Show(t *testing.T) {
	c := newCore(t, nil, nil)
	evs := collectEvents(context.Background(), c, "/model")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "test") {
			found = true
		}
	}
	if !found {
		t.Errorf("/model: model name not in info events: %v", byRole(evs, "info"))
	}
}

func TestCommand_Model_Set(t *testing.T) {
	be := defaultBE()
	c := newCore(t, be, nil)
	collectEvents(context.Background(), c, "/model gpt-4")
	if got := be.Model(); got != "gpt-4" {
		t.Errorf("after /model gpt-4: Model() = %q; want gpt-4", got)
	}
}

func TestCommand_Model_WhileRunning(t *testing.T) {
	unblock := make(chan struct{})
	be := defaultBE()
	be.respond = func(ctx context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return blockedStream(ctx, unblock), nil
	}
	c := newCore(t, be, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Submit(context.Background(), "hello", func(Event) {})
	}()
	waitState(t, c, "thinking")

	evs := collectEvents(context.Background(), c, "/model new-model")

	close(unblock)
	<-done

	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "error") {
			found = true
		}
	}
	if !found {
		t.Errorf("/model while running: expected error event, got: %v", byRole(evs, "info"))
	}
}

// --- /models ---

func TestCommand_Models_Empty(t *testing.T) {
	c := newCore(t, nil, nil) // defaultBE has nil models
	evs := collectEvents(context.Background(), c, "/models")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "no models") {
			found = true
		}
	}
	if !found {
		t.Errorf("/models with no models: expected 'no models available', got: %v", byRole(evs, "info"))
	}
}

func TestCommand_Models_List(t *testing.T) {
	be := &mockBackend{name: "mock", model: "b", ctxLen: 128000, models: []string{"a", "b", "c"}}
	c := newCore(t, be, nil)
	evs := collectEvents(context.Background(), c, "/models")
	infos := byRole(evs, "info")
	if len(infos) < 3 {
		t.Fatalf("/models: expected ≥3 info events; got: %v", infos)
	}
	markedCurrent := false
	for _, s := range infos {
		if strings.Contains(s, "* ") && strings.Contains(s, "b") {
			markedCurrent = true
		}
	}
	if !markedCurrent {
		t.Errorf("/models: current model 'b' not marked with '* '; got: %v", infos)
	}
}

// --- /agent ---

func TestCommand_Agent_Show(t *testing.T) {
	c := newCore(t, nil, nil)
	evs := collectEvents(context.Background(), c, "/agent")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "test") {
			found = true
		}
	}
	if !found {
		t.Errorf("/agent: agent name not in info events: %v", byRole(evs, "info"))
	}
}

func TestCommand_Agent_WhileRunning(t *testing.T) {
	unblock := make(chan struct{})
	be := defaultBE()
	be.respond = func(ctx context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return blockedStream(ctx, unblock), nil
	}
	c := newCore(t, be, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Submit(context.Background(), "hello", func(Event) {})
	}()
	waitState(t, c, "thinking")

	evs := collectEvents(context.Background(), c, "/agent other")

	close(unblock)
	<-done

	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "error") {
			found = true
		}
	}
	if !found {
		t.Errorf("/agent while running: expected error event, got: %v", byRole(evs, "info"))
	}
}

func TestCommand_Agent_NotFound(t *testing.T) {
	c := newCore(t, nil, nil) // agentsDir is a fresh temp dir
	evs := collectEvents(context.Background(), c, "/agent nonexistent")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "error") {
			found = true
		}
	}
	if !found {
		t.Errorf("/agent nonexistent: expected error event, got: %v", byRole(evs, "info"))
	}
}

// --- /agents ---

func TestCommand_Agents_Empty(t *testing.T) {
	c := newCore(t, nil, nil) // agentsDir is a fresh temp dir
	evs := collectEvents(context.Background(), c, "/agents")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "no agents found") {
			found = true
		}
	}
	if !found {
		t.Errorf("/agents with empty dir: expected 'no agents found', got: %v", byRole(evs, "info"))
	}
}

// --- /sessions ---

func TestCommand_Sessions_Empty(t *testing.T) {
	c := newCore(t, nil, nil) // sessionsDir is a fresh temp dir
	evs := collectEvents(context.Background(), c, "/sessions")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "no sessions found") {
			found = true
		}
	}
	if !found {
		t.Errorf("/sessions with empty dir: expected 'no sessions found', got: %v", byRole(evs, "info"))
	}
}

func TestCommand_Sessions_List(t *testing.T) {
	c := newCore(t, nil, nil)
	collectEvents(context.Background(), c, "hello") // creates and saves session
	evs := collectEvents(context.Background(), c, "/sessions")
	markedCurrent := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "* ") {
			markedCurrent = true
		}
	}
	if !markedCurrent {
		t.Errorf("/sessions: current session not marked with '* '; got: %v", byRole(evs, "info"))
	}
}

// --- /cwd ---

func TestCommand_CWD_Show(t *testing.T) {
	c := newCore(t, nil, nil)
	cwd := c.CWD()
	evs := collectEvents(context.Background(), c, "/cwd")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, cwd) {
			found = true
		}
	}
	if !found {
		t.Errorf("/cwd: path %q not in info events: %v", cwd, byRole(evs, "info"))
	}
}

func TestCommand_CWD_Set(t *testing.T) {
	c := newCore(t, nil, nil)
	dir := t.TempDir()
	evs := collectEvents(context.Background(), c, "/cwd "+dir)
	if got := c.CWD(); got != dir {
		t.Errorf("CWD() = %q; want %q", got, dir)
	}
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, dir) {
			found = true
		}
	}
	if !found {
		t.Errorf("/cwd set: new path not confirmed in info events: %v", byRole(evs, "info"))
	}
}

func TestCommand_CWD_SetNonExistent(t *testing.T) {
	c := newCore(t, nil, nil)
	old := c.CWD()
	evs := collectEvents(context.Background(), c, "/cwd /nonexistent/xyz/abc")
	if got := c.CWD(); got != old {
		t.Errorf("CWD changed to %q after invalid path; want unchanged %q", got, old)
	}
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "error") {
			found = true
		}
	}
	if !found {
		t.Errorf("/cwd nonexistent: expected error event, got: %v", byRole(evs, "info"))
	}
}

// --- /context ---

func TestCommand_Context_NoSession(t *testing.T) {
	c := newCore(t, nil, nil)
	evs := collectEvents(context.Background(), c, "/context")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "no active session") {
			found = true
		}
	}
	if !found {
		t.Errorf("/context before any turn: expected 'no active session', got: %v", byRole(evs, "info"))
	}
}

func TestCommand_Context_WithSession(t *testing.T) {
	c := newCore(t, nil, nil)
	collectEvents(context.Background(), c, "hello")
	evs := collectEvents(context.Background(), c, "/context")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "tokens") {
			found = true
		}
	}
	if !found {
		t.Errorf("/context after turn: expected token usage line, got: %v", byRole(evs, "info"))
	}
}

// --- /usage ---

func TestCommand_Usage_NoSession(t *testing.T) {
	c := newCore(t, nil, nil)
	evs := collectEvents(context.Background(), c, "/usage")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "no active session") {
			found = true
		}
	}
	if !found {
		t.Errorf("/usage before any turn: expected 'no active session', got: %v", byRole(evs, "info"))
	}
}

func TestCommand_Usage_WithSession(t *testing.T) {
	c := newCore(t, nil, nil)
	collectEvents(context.Background(), c, "hello")
	evs := collectEvents(context.Background(), c, "/usage")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "requests") {
			found = true
		}
	}
	if !found {
		t.Errorf("/usage after turn: expected 'requests' in output, got: %v", byRole(evs, "info"))
	}
}

// --- /history ---

func TestCommand_History_NoSession(t *testing.T) {
	c := newCore(t, nil, nil)
	evs := collectEvents(context.Background(), c, "/history")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "no active session") {
			found = true
		}
	}
	if !found {
		t.Errorf("/history before any turn: expected 'no active session', got: %v", byRole(evs, "info"))
	}
}

func TestCommand_History_WithSession(t *testing.T) {
	c := newCore(t, nil, nil)
	collectEvents(context.Background(), c, "remember this")
	evs := collectEvents(context.Background(), c, "/history")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "user") && strings.Contains(s, "remember this") {
			found = true
		}
	}
	if !found {
		t.Errorf("/history: user message not found in output: %v", byRole(evs, "info"))
	}
}

// --- /sp ---

func TestCommand_SP(t *testing.T) {
	c := newCore(t, nil, nil)
	evs := collectEvents(context.Background(), c, "/sp")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "test system prompt") {
			found = true
		}
	}
	if !found {
		t.Errorf("/sp: system prompt not in info events: %v", byRole(evs, "info"))
	}
}

// --- /agents with files ---

func TestCommand_Agents_List(t *testing.T) {
	c := newCore(t, nil, nil)
	if err := os.WriteFile(c.agentsDir+"/myagent.json", []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	evs := collectEvents(context.Background(), c, "/agents")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "myagent") {
			found = true
		}
	}
	if !found {
		t.Errorf("/agents with files: 'myagent' not in output: %v", byRole(evs, "info"))
	}
}

// --- hooksRan plural ---

func TestHooksRan(t *testing.T) {
	if got := hooksRan(1); got != "1 hook run" {
		t.Errorf("hooksRan(1) = %q; want %q", got, "1 hook run")
	}
	if got := hooksRan(3); got != "3 hooks run" {
		t.Errorf("hooksRan(3) = %q; want %q", got, "3 hooks run")
	}
}

// --- rate-limit retry ---

func TestRun_RateLimitRetry(t *testing.T) {
	callCount := 0
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		if callCount == 1 {
			return nil, &backend.RateLimitError{RetryAfter: time.Millisecond}
		}
		return textStream("ok"), nil
	}
	c := newCore(t, be, nil)
	var retryEvents []Event
	c.Submit(context.Background(), "hello", func(ev Event) {
		if ev.Role == "retry" {
			retryEvents = append(retryEvents, ev)
		}
	})
	if callCount != 2 {
		t.Errorf("backend called %d times; want 2 (retry + success)", callCount)
	}
	if len(retryEvents) == 0 {
		t.Error("no retry events emitted during rate-limit retry")
	}
}

// --- run: tool cancelled before execution ---

func TestRun_ToolCancelledBeforeExec(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callCount := 0
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		cancel() // cancel before tool loop runs
		ch := make(chan backend.StreamEvent, 1)
		ch <- backend.StreamEvent{
			ToolCalls:  []backend.ToolCall{{Name: "my_tool", Arguments: json.RawMessage(`{}`)}},
			Done:       true,
			StopReason: "tool_calls",
		}
		close(ch)
		return ch, nil
	}
	c := newCore(t, be, nil)
	c.Submit(ctx, "hello", func(Event) {})
	if callCount != 1 {
		t.Errorf("backend called %d times; want 1 (no retry after cancellation)", callCount)
	}
	if got := c.State(); got != "idle" {
		t.Errorf("State() = %q after tool cancellation; want idle", got)
	}
}

// --- run: ChatStream error while ctx cancelled (recordInterruption "request") ---

func TestRun_RequestCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		cancel()
		return nil, fmt.Errorf("request failed")
	}
	c := newCore(t, be, nil)
	c.Submit(ctx, "hello", func(Event) {})
	if got := c.State(); got != "idle" {
		t.Errorf("State() = %q after request cancellation; want idle", got)
	}
}

// --- run: Exec error while ctx cancelled, with pending inject ---

func TestRun_ExecCancelledWithInject(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callCount := 0
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		ch := make(chan backend.StreamEvent, 1)
		ch <- backend.StreamEvent{
			ToolCalls:  []backend.ToolCall{{Name: "my_tool", Arguments: json.RawMessage(`{}`)}},
			Done:       true,
			StopReason: "tool_calls",
		}
		close(ch)
		return ch, nil
	}
	c := newCore(t, be, nil)
	inject := "user interrupt"
	c.pendingInject.Store(&inject)
	c.loopcfg.Exec = func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		cancel() // ctx cancelled during exec
		return "", fmt.Errorf("exec cancelled")
	}
	c.Submit(ctx, "hello", func(Event) {})
	if callCount != 1 {
		t.Errorf("backend called %d times; want 1", callCount)
	}
	if got := c.State(); got != "idle" {
		t.Errorf("State() = %q after exec cancellation; want idle", got)
	}
}

// --- auto-compact with hook context injection ---

func TestAutoCompact_WithHookContext(t *testing.T) {
	callCount := 0
	be := &mockBackend{name: "mock", model: "test", ctxLen: 10}
	c := newCore(t, be, Hooks{
		HookPreCompact:  []string{`echo "pre-compact context"`},
		HookPostCompact: []string{`echo "post-compact context"`},
	})
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		if callCount == 1 {
			return textStream("summary text for compaction"), nil
		}
		return textStream("answer"), nil
	}
	c.session = newSession("goal")
	for range 3 {
		c.session.messages = append(c.session.messages,
			backend.Message{Role: "assistant", Content: "a long response that exceeds seven tokens of content"},
			backend.Message{Role: "user", Content: "follow up question with enough text to push over the limit"},
		)
	}
	evs := collectEvents(context.Background(), c, "next prompt")
	if callCount < 2 {
		t.Errorf("backend called %d times; want ≥2 (compact + turn)", callCount)
	}
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "hook run") {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'hook run' info event from compact hooks; got: %v", byRole(evs, "info"))
	}
}

// --- Core method accessors ---

func TestCore_AgentName(t *testing.T) {
	c := newCore(t, nil, nil)
	if got := c.AgentName(); got != "test" {
		t.Errorf("AgentName() = %q; want %q", got, "test")
	}
}

func TestCore_BackendName(t *testing.T) {
	c := newCore(t, nil, nil)
	if got := c.BackendName(); got != "mock" {
		t.Errorf("BackendName() = %q; want %q", got, "mock")
	}
}

func TestCore_ModelName(t *testing.T) {
	c := newCore(t, nil, nil)
	if got := c.ModelName(); got != "test" {
		t.Errorf("ModelName() = %q; want %q", got, "test")
	}
}

func TestCore_SystemPrompt(t *testing.T) {
	c := newCore(t, nil, nil)
	if got := c.SystemPrompt(); got != "test system prompt" {
		t.Errorf("SystemPrompt() = %q; want %q", got, "test system prompt")
	}
}

func TestCore_GenerationParams(t *testing.T) {
	c := newCore(t, nil, nil)
	p := c.GenerationParams()
	if p.MaxTokens != 0 {
		t.Errorf("GenerationParams().MaxTokens = %d; want 0 (default)", p.MaxTokens)
	}
}

func TestCore_SetGenerationParams_WhileRunning(t *testing.T) {
	unblock := make(chan struct{})
	be := defaultBE()
	be.respond = func(ctx context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return blockedStream(ctx, unblock), nil
	}
	c := newCore(t, be, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Submit(context.Background(), "hello", func(Event) {})
	}()
	waitState(t, c, "thinking")

	err := c.SetGenerationParams(backend.GenerationParams{})

	close(unblock)
	<-done

	if err == nil {
		t.Error("SetGenerationParams while running: want error, got nil")
	}
}

func TestCore_ListModels(t *testing.T) {
	be := &mockBackend{name: "mock", model: "test", ctxLen: 128000, models: []string{"a", "b"}}
	c := newCore(t, be, nil)
	got := c.ListModels()
	if !strings.Contains(got, "a") || !strings.Contains(got, "b") {
		t.Errorf("ListModels() = %q; want 'a' and 'b'", got)
	}
}

func TestCore_CtxSz_NoSession(t *testing.T) {
	c := newCore(t, nil, nil)
	if got := c.CtxSz(); got != "no active session" {
		t.Errorf("CtxSz() with no session = %q; want 'no active session'", got)
	}
}

func TestCore_CtxSz_WithSession(t *testing.T) {
	c := newCore(t, nil, nil)
	collectEvents(context.Background(), c, "hello")
	got := c.CtxSz()
	if !strings.Contains(got, "/") {
		t.Errorf("CtxSz() = %q; want token fraction like '10 / 128000 (0%%)'", got)
	}
}

func TestCore_Usage_NoSession(t *testing.T) {
	c := newCore(t, nil, nil)
	if got := c.Usage(); got != "no active session" {
		t.Errorf("Usage() with no session = %q; want 'no active session'", got)
	}
}

func TestCore_Usage_WithSession(t *testing.T) {
	c := newCore(t, nil, nil)
	collectEvents(context.Background(), c, "hello")
	got := c.Usage()
	if !strings.Contains(got, "requests") {
		t.Errorf("Usage() = %q; want string containing 'requests'", got)
	}
}

// --- SetSessionID ---

func TestSetSessionID_Rename(t *testing.T) {
	c := newCore(t, nil, nil)
	collectEvents(context.Background(), c, "hello") // saves session file
	oldID := c.sessionID
	newID := NewSessionID()
	if err := c.SetSessionID(newID); err != nil {
		t.Fatalf("SetSessionID: %v", err)
	}
	if c.sessionID != newID {
		t.Errorf("sessionID = %q; want %q", c.sessionID, newID)
	}
	if _, err := os.Stat(c.sessionsDir + "/" + oldID + ".json"); !os.IsNotExist(err) {
		t.Errorf("old session file still exists after rename; err=%v", err)
	}
	if _, err := os.Stat(c.sessionsDir + "/" + newID + ".json"); err != nil {
		t.Errorf("new session file not found after rename: %v", err)
	}
}

func TestSetSessionID_SameID(t *testing.T) {
	c := newCore(t, nil, nil)
	id := c.sessionID
	if err := c.SetSessionID(id); err != nil {
		t.Fatalf("SetSessionID with same ID: %v", err)
	}
	if c.sessionID != id {
		t.Errorf("sessionID changed: got %q; want %q", c.sessionID, id)
	}
}

// --- SetGenerationParams success ---

func TestCore_SetGenerationParams_Success(t *testing.T) {
	c := newCore(t, nil, nil)
	p := backend.GenerationParams{MaxTokens: 100}
	if err := c.SetGenerationParams(p); err != nil {
		t.Fatalf("SetGenerationParams: %v", err)
	}
	if got := c.GenerationParams().MaxTokens; got != 100 {
		t.Errorf("GenerationParams().MaxTokens = %d; want 100", got)
	}
}

// --- autoCompactLimit with zero ctxLen ---

func TestAutoCompactLimit_DefaultWhenZero(t *testing.T) {
	be := &mockBackend{name: "mock", model: "test", ctxLen: 0}
	c := newCore(t, be, nil)
	limit := c.autoCompactLimit(context.Background())
	want := defaultContextLength * 3 / 4
	if limit != want {
		t.Errorf("autoCompactLimit with ctxLen=0 = %d; want %d", limit, want)
	}
}

// --- agentSpawn hook with context output ---

func TestAgentSpawn_WithContext(t *testing.T) {
	c := newCore(t, nil, Hooks{HookAgentSpawn: []string{`echo "spawn context"`}})
	collectEvents(context.Background(), c, "first")
	if !strings.Contains(c.loopcfg.systemPrompt, "spawn context") {
		t.Errorf("system prompt does not contain spawn context: %q", c.loopcfg.systemPrompt)
	}
}

// --- retryCountdown: ctx cancelled during wait ---

func TestRun_RateLimitRetry_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callCount := 0
	be := defaultBE()
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()
		return nil, &backend.RateLimitError{RetryAfter: 10 * time.Second}
	}
	c := newCore(t, be, nil)
	c.Submit(ctx, "hello", func(Event) {})
	if callCount != 1 {
		t.Errorf("backend called %d times after cancelled retry; want 1", callCount)
	}
}

// --- compact: backend error ---

func TestManualCompact_BackendError(t *testing.T) {
	callCount := 0
	be := defaultBE()
	c := newCore(t, be, nil)
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		callCount++
		return nil, fmt.Errorf("compact backend error")
	}
	c.session = newSession("goal")
	for i := range 5 {
		c.session.messages = append(c.session.messages,
			backend.Message{Role: "assistant", Content: fmt.Sprintf("response %d", i)},
			backend.Message{Role: "user", Content: fmt.Sprintf("follow up %d", i)},
		)
	}
	evs := collectEvents(context.Background(), c, "/compact")
	if callCount == 0 {
		t.Fatal("backend not called for compact")
	}
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "compact error") {
			found = true
		}
	}
	if !found {
		t.Errorf("/compact backend error: expected 'compact error' event; got: %v", byRole(evs, "info"))
	}
}

// --- compact: empty summary from backend ---

func TestManualCompact_EmptySummary(t *testing.T) {
	be := defaultBE()
	c := newCore(t, be, nil)
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		ch := make(chan backend.StreamEvent, 1)
		ch <- backend.StreamEvent{Done: true, StopReason: "stop", Content: "   "}
		close(ch)
		return ch, nil
	}
	c.session = newSession("goal")
	for i := range 5 {
		c.session.messages = append(c.session.messages,
			backend.Message{Role: "assistant", Content: fmt.Sprintf("response %d", i)},
			backend.Message{Role: "user", Content: fmt.Sprintf("follow up %d", i)},
		)
	}
	evs := collectEvents(context.Background(), c, "/compact")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "compact error") {
			found = true
		}
	}
	if !found {
		t.Errorf("/compact empty summary: expected 'compact error'; got: %v", byRole(evs, "info"))
	}
}

// --- compact: session with tool-call messages (exercises flattenToolMessages) ---

func TestManualCompact_WithToolMessages(t *testing.T) {
	be := defaultBE()
	c := newCore(t, be, nil)
	be.respond = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		return textStream("summary"), nil
	}
	c.session = newSession("goal")
	c.session.messages = append(c.session.messages,
		backend.Message{
			Role:      "assistant",
			Content:   "calling tool",
			ToolCalls: []backend.ToolCall{{Name: "my_tool", Arguments: json.RawMessage(`{"key":"val"}`)}},
		},
		backend.Message{Role: "tool", Content: "tool result", ToolCallID: "1"},
		backend.Message{Role: "user", Content: "more stuff"},
		backend.Message{Role: "assistant", Content: "done"},
		backend.Message{Role: "user", Content: "follow up"},
	)
	evs := collectEvents(context.Background(), c, "/compact")
	found := false
	for _, s := range byRole(evs, "info") {
		if strings.Contains(s, "compacted") {
			found = true
		}
	}
	if !found {
		t.Errorf("/compact with tool messages: no 'compacted' event; got: %v", byRole(evs, "info"))
	}
}

// --- hook timeout ---

func TestHookTimeout_Branch(t *testing.T) {
	old := hookTimeout
	hookTimeout = 0 // 0s timeout fires immediately
	t.Cleanup(func() { hookTimeout = old })

	// A hook that sleeps longer than the timeout. The timeout branch kills the
	// process and returns HookResult{} (Ran=false), so the turn is NOT blocked
	// and the backend runs normally.
	hooks := Hooks{HookPreTurn: []string{"sleep 10"}}
	c := newCore(t, nil, hooks)
	evs := collectEvents(context.Background(), c, "hello")

	// Turn must have run: an assistant event proves the hook didn't block it.
	if got := byRole(evs, "assistant"); len(got) == 0 {
		t.Errorf("expected assistant event after hook timeout; hook must not have blocked the turn")
	}
	if c.State() != "idle" {
		t.Errorf("State() = %q after timeout hook; want idle", c.State())
	}
}

// --- /backend with injected newBackend ---

func TestCommand_Backend_Switch(t *testing.T) {
	c := newCore(t, nil, nil)
	newBE := &mockBackend{name: "injected", model: "new-model"}
	c.newBackend = func() (backend.Backend, error) { return newBE, nil }

	evs := collectEvents(context.Background(), c, "/backend other")
	infos := byRole(evs, "info")
	found := false
	for _, s := range infos {
		if strings.Contains(s, "injected") {
			found = true
		}
	}
	if !found {
		t.Errorf("/backend switch: expected 'injected' in info events; got %v", infos)
	}
	if c.loopcfg.Backend != newBE {
		t.Errorf("/backend switch: backend not updated")
	}
}

func TestCommand_Backend_Error(t *testing.T) {
	c := newCore(t, nil, nil)
	c.newBackend = func() (backend.Backend, error) { return nil, fmt.Errorf("no such backend") }

	evs := collectEvents(context.Background(), c, "/backend bad")
	infos := byRole(evs, "info")
	found := false
	for _, s := range infos {
		if strings.Contains(s, "no such backend") {
			found = true
		}
	}
	if !found {
		t.Errorf("/backend error: expected error message; got %v", infos)
	}
}

// --- extractToolResult ---

func TestExtractToolResult_Success(t *testing.T) {
	raw := json.RawMessage(`{"isError":false,"content":[{"type":"text","text":"hello"}]}`)
	text, isErr := extractToolResult(raw)
	if text != "hello" {
		t.Errorf("text = %q; want %q", text, "hello")
	}
	if isErr {
		t.Error("isError should be false")
	}
}

func TestExtractToolResult_IsError(t *testing.T) {
	raw := json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"something failed"}]}`)
	text, isErr := extractToolResult(raw)
	if text != "something failed" {
		t.Errorf("text = %q; want %q", text, "something failed")
	}
	if !isErr {
		t.Error("isError should be true")
	}
}

func TestExtractToolResult_MultipleContentItems(t *testing.T) {
	raw := json.RawMessage(`{"isError":false,"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`)
	text, _ := extractToolResult(raw)
	if text != "a\nb" {
		t.Errorf("text = %q; want %q", text, "a\nb")
	}
}

func TestExtractToolResult_NonTextItemsSkipped(t *testing.T) {
	raw := json.RawMessage(`{"isError":false,"content":[{"type":"image","text":"ignored"},{"type":"text","text":"kept"}]}`)
	text, _ := extractToolResult(raw)
	if text != "kept" {
		t.Errorf("text = %q; want %q", text, "kept")
	}
}

func TestExtractToolResult_InvalidJSON(t *testing.T) {
	raw := json.RawMessage(`not json`)
	text, isErr := extractToolResult(raw)
	if text != "not json" {
		t.Errorf("text = %q; want raw input on parse failure", text)
	}
	if isErr {
		t.Error("isError should be false on parse failure")
	}
}

// --- toolInfosToBackend ---

func TestToolInfosToBackend(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	infos := []tools.ToolInfo{
		{Name: "tool_a", Description: "does A.", InputSchema: schema},
		{Name: "tool_b", Description: "does B.", InputSchema: schema},
	}
	got := toolInfosToBackend(infos)
	if len(got) != 2 {
		t.Fatalf("len = %d; want 2", len(got))
	}
	if got[0].Name != "tool_a" || got[0].Description != "does A." {
		t.Errorf("got[0] = %+v", got[0])
	}
	if string(got[1].Parameters) != string(schema) {
		t.Errorf("got[1].Parameters = %s; want %s", got[1].Parameters, schema)
	}
}

func TestToolInfosToBackend_Empty(t *testing.T) {
	got := toolInfosToBackend(nil)
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %v", got)
	}
}

// --- RestoreSession round-trip ---

func TestRestoreSession_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.json")

	s := newSession("first user message")
	s.appendUserMessage("second message")
	s.messages = append(s.messages, backend.Message{Role: "assistant", Content: "reply"})

	if err := s.saveTo(path, "test-id", "test-agent"); err != nil {
		t.Fatalf("saveTo: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var ps PersistedSession
	if err := json.Unmarshal(data, &ps); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if ps.ID != "test-id" || ps.Agent != "test-agent" {
		t.Errorf("ps.ID=%q ps.Agent=%q", ps.ID, ps.Agent)
	}

	restored := RestoreSession(ps.Messages)
	if restored.goal != "first user message" {
		t.Errorf("goal = %q; want %q", restored.goal, "first user message")
	}
	if len(restored.messages) != len(s.messages) {
		t.Errorf("messages len = %d; want %d", len(restored.messages), len(s.messages))
	}
}

func TestRestoreSession_GoalFromFirstUserMessage(t *testing.T) {
	msgs := []backend.Message{
		{Role: "assistant", Content: "preamble"},
		{Role: "user", Content: "the real goal"},
		{Role: "user", Content: "second user msg"},
	}
	s := RestoreSession(msgs)
	if s.goal != "the real goal" {
		t.Errorf("goal = %q; want %q", s.goal, "the real goal")
	}
}

// --- SetEnv propagation ---

// mockEnvServer is a tools.Server that also implements tools.EnvSetter.
type mockEnvServer struct {
	mu  sync.Mutex
	env map[string]string
}

func (m *mockEnvServer) ListTools() ([]tools.ToolInfo, error)                              { return nil, nil }
func (m *mockEnvServer) CallTool(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockEnvServer) Close() {}
func (m *mockEnvServer) SetEnv(k, v string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.env == nil {
		m.env = make(map[string]string)
	}
	m.env[k] = v
}
func (m *mockEnvServer) get(k string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.env[k]
}

func newCoreWithExecServer(t *testing.T, srv *mockEnvServer) *agentCore {
	t.Helper()
	d := tools.NewDispatcher()
	d.AddServer("execute", srv)
	env := AgentEnv{
		Hooks:        Hooks{},
		systemPrompt: "test system prompt",
		dispatcher:   d,
	}
	c := NewAgentCore(AgentCoreConfig{
		Backend:       defaultBE(),
		AgentName:     "test",
		AgentsDir:     t.TempDir(),
		SessionsDir:   t.TempDir(),
		SessionID:     NewSessionID(),
		CWD:           t.TempDir(),
		Env:           env,
		NewDispatcher: tools.NewDispatcher,
	})
	t.Cleanup(c.Close)
	return c.(*agentCore)
}

func TestSetEnv_PropagatestoExecuteServer(t *testing.T) {
	srv := &mockEnvServer{}
	c := newCoreWithExecServer(t, srv)

	c.SetEnv("MY_KEY", "my_value")

	if got := srv.get("MY_KEY"); got != "my_value" {
		t.Errorf("execute server env MY_KEY = %q; want %q", got, "my_value")
	}
}

func TestSetEnv_StoredInCore(t *testing.T) {
	srv := &mockEnvServer{}
	c := newCoreWithExecServer(t, srv)

	c.SetEnv("FOO", "bar")
	c.SetEnv("BAZ", "qux")

	c.envMu.RLock()
	defer c.envMu.RUnlock()
	if c.env["FOO"] != "bar" {
		t.Errorf("env[FOO] = %q; want bar", c.env["FOO"])
	}
	if c.env["BAZ"] != "qux" {
		t.Errorf("env[BAZ] = %q; want qux", c.env["BAZ"])
	}
}

func TestSetEnv_ShellCommandSeesEnv(t *testing.T) {
	srv := &mockEnvServer{}
	c := newCoreWithExecServer(t, srv)

	c.SetEnv("OLLIE_TEST_VAR", "sentinel_value")

	evs := collectEvents(context.Background(), c, "!echo $OLLIE_TEST_VAR")
	infos := byRole(evs, "info")
	found := false
	for _, s := range infos {
		if strings.Contains(s, "sentinel_value") {
			found = true
		}
	}
	if !found {
		t.Errorf("shell command did not see OLLIE_TEST_VAR; info events: %v", infos)
	}
}

func TestSetEnv_NilDispatcher_NoPanic(t *testing.T) {
	c := newCore(t, nil, nil)
	c.dispatcher = nil
	// Must not panic.
	c.SetEnv("K", "V")
	if c.env["K"] != "V" {
		t.Errorf("env[K] = %q; want V", c.env["K"])
	}
}

// --- firstSentence ---

func TestFirstSentence_Period(t *testing.T) {
	if got := firstSentence("Does a thing. More detail."); got != "Does a thing." {
		t.Errorf("got %q", got)
	}
}

func TestFirstSentence_Newline(t *testing.T) {
	if got := firstSentence("Does a thing\nMore detail"); got != "Does a thing" {
		t.Errorf("got %q", got)
	}
}

func TestFirstSentence_TruncatesLong(t *testing.T) {
	long := strings.Repeat("x", 100)
	got := firstSentence(long)
	if len(got) != 80 || !strings.HasSuffix(got, "...") {
		t.Errorf("got %q (len %d)", got, len(got))
	}
}

func TestFirstSentence_ShortNoSentenceEnd(t *testing.T) {
	if got := firstSentence("short"); got != "short" {
		t.Errorf("got %q", got)
	}
}

// --- BuildAgentEnv (nil config path) ---

func setupCfgDir(t *testing.T, systemPrompt string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/prompts", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/prompts/SYSTEM_PROMPT.md", []byte(systemPrompt), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLIE_CFG_PATH", dir)
	return dir
}

func TestBuildAgentEnv_NilConfig(t *testing.T) {
	setupCfgDir(t, "base prompt")
	d := tools.NewDispatcher()
	env := BuildAgentEnv(nil, d, t.TempDir())

	if env.systemPrompt != "base prompt" {
		t.Errorf("systemPrompt = %q; want %q", env.systemPrompt, "base prompt")
	}
	if len(env.Hooks) != 0 {
		t.Errorf("expected no hooks; got %v", env.Hooks)
	}
	if len(env.tools) != 0 {
		t.Errorf("expected no tools; got %v", env.tools)
	}
	if len(env.Messages) != 0 {
		t.Errorf("expected no startup messages; got %v", env.Messages)
	}
}

func TestBuildAgentEnv_AgentPromptAppended(t *testing.T) {
	setupCfgDir(t, "base prompt")
	d := tools.NewDispatcher()
	cfg := &config.Config{Prompt: "agent suffix"}
	env := BuildAgentEnv(cfg, d, t.TempDir())

	if !strings.Contains(env.systemPrompt, "base prompt") {
		t.Errorf("systemPrompt missing base: %q", env.systemPrompt)
	}
	if !strings.Contains(env.systemPrompt, "agent suffix") {
		t.Errorf("systemPrompt missing agent suffix: %q", env.systemPrompt)
	}
}

func TestBuildAgentEnv_HooksAndParams(t *testing.T) {
	setupCfgDir(t, "base")
	d := tools.NewDispatcher()
	temp := 0.7
	cfg := &config.Config{
		Hooks:       map[string]config.HookCmds{"preTurn": {"echo hi"}},
		MaxTokens:   512,
		Temperature: &temp,
	}
	env := BuildAgentEnv(cfg, d, t.TempDir())

	if cmds := env.Hooks[HookPreTurn]; len(cmds) != 1 || cmds[0] != "echo hi" {
		t.Errorf("Hooks[preTurn] = %v", cmds)
	}
	if env.genParams.MaxTokens != 512 {
		t.Errorf("MaxTokens = %d; want 512", env.genParams.MaxTokens)
	}
	if env.genParams.Temperature == nil || *env.genParams.Temperature != 0.7 {
		t.Errorf("Temperature = %v; want 0.7", env.genParams.Temperature)
	}
}

func TestBuildAgentEnv_ExecUnknownTool(t *testing.T) {
	setupCfgDir(t, "base")
	d := tools.NewDispatcher()
	env := BuildAgentEnv(nil, d, t.TempDir())

	_, err := env.exec(context.Background(), "no_such_tool", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("expected unknown tool error; got %v", err)
	}
}
