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

// --- /mcp ---

func TestCommand_MCP(t *testing.T) {
	c := newCore(t, nil, nil)
	evs := collectEvents(context.Background(), c, "/mcp")
	if len(byRole(evs, "info")) == 0 {
		t.Error("/mcp: no info events emitted")
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
