package agent

import "ollie/backend"

// ToolResult holds the output of a single tool call.
type ToolResult struct {
	ToolCallID string // may be empty (Ollama does not always set this)
	Name       string
	Content    string
	IsError    bool
}

// State is the interface both ephemeral and bead-backed state must satisfy.
// The loop reads from it on Observe, writes to it on Update.
type State interface {
	// Goal returns the prompt or task description driving this session.
	Goal() string

	// History returns the full conversation history for the current session.
	// The first entry is always the initial user message containing the goal.
	History() []backend.Message

	// Update appends the assistant's reply and any tool results to the history.
	// Called once per loop iteration after Act completes.
	Update(assistant backend.Message, results []ToolResult) error

	// MarkComplete records that the goal has been achieved.
	MarkComplete() error

	// IsComplete returns true once MarkComplete has been called.
	IsComplete() bool
}
