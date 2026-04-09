package agent

import (
	"context"
	"errors"
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
	Submit(ctx context.Context, input string, handler EventHandler)

	// Prompt returns the display prompt string for the current session state.
	Prompt() string

	// Interrupt cancels the current in-progress agent turn.
	// Returns true if an action was running and was cancelled.
	Interrupt(cause error) bool
}
