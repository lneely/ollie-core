package agent

import (
	"context"
	"errors"
	"sync"
)

// ErrInterrupted is returned when the user cancels an agent turn (Ctrl-C).
var ErrInterrupted = errors.New("interrupted")

// Event is a typed output event emitted during an agent turn or in response
// to a command.
type Event struct {
	Role    string
	Name    string
	Content string
}

// EventHandler receives events from the agent.
type EventHandler func(Event)

// Core is the interface between a frontend (TUI, HTTP handler, etc.) and the
// agent engine. All output from the agent is delivered via EventHandler.
type Core interface {
	// Submit processes one line of user input. Slash commands and shell
	// shortcuts are dispatched synchronously; any other input starts an agent
	// turn that streams events to handler until the turn is complete.
	// After the turn, any queued prompts are drained sequentially.
	Submit(ctx context.Context, input string, handler EventHandler)

	// Prompt returns the display prompt string for the current session state.
	Prompt() string

	// Interrupt cancels the current in-progress agent turn.
	// Returns true if an action was running and was cancelled.
	Interrupt(cause error) bool

	// Inject sends a message that will be appended to the next tool result
	// as a user interruption. If no turn is running, it is silently dropped.
	Inject(prompt string)

	// Queue pushes a prompt onto the FIFO for execution after the current
	// turn completes.
	Queue(prompt string)

	// PopQueue removes and returns the next queued prompt.
	// Returns ("", false) if the queue is empty.
	PopQueue() (string, bool)

	// IsRunning returns true if an agent turn is currently in progress.
	IsRunning() bool

	// State returns the current agent state: "idle", "thinking", or "calling: <tool>".
	State() string

	// Reply returns the assistant text from the most recently completed turn.
	// Cleared when a new prompt is submitted.
	Reply() string

	// AgentName returns the name of the active agent.
	AgentName() string

	// BackendName returns the name of the active backend (e.g. "anthropic", "ollama").
	BackendName() string

	// ModelName returns the name of the active model.
	ModelName() string

	// CtxSz returns the estimated context size as a one-line summary.
	CtxSz() string

	// Usage returns billed token counts as a one-line summary.
	Usage() string

	// ListModels returns available model names, one per line.
	ListModels() string

	// ListServers returns all registered tool servers and their tools,
	// grouped by server name.
	ListServers() string

	// CWD returns the current working directory used for tool execution.
	CWD() string

	// SetCWD changes the working directory for tool execution and
	// updates the system prompt. Returns an error if the path does not exist.
	SetCWD(dir string) error

	// SetSessionID renames the session: updates the in-memory ID, renames
	// persisted files on disk, and propagates to the execute server env.
	SetSessionID(newID string) error

	// SystemPrompt returns the fully rendered system prompt for this session.
	SystemPrompt() string
}

// PromptFIFO is a simple thread-safe FIFO for queued prompts.
type PromptFIFO struct {
	mu    sync.Mutex
	items []string
}

func (f *PromptFIFO) Push(s string) {
	f.mu.Lock()
	f.items = append(f.items, s)
	f.mu.Unlock()
}

func (f *PromptFIFO) Pop() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.items) == 0 {
		return "", false
	}
	s := f.items[0]
	f.items = f.items[1:]
	return s, true
}
