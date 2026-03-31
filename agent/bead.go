package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"ollie/backend"
)

// BeadRef identifies a bead by its mount and ID.
type BeadRef struct {
	Mount string // e.g. "pcloudcc-lneely"
	ID    string // e.g. "bd-abc"
}

// beadJSON is the wire format returned by read_bead.sh --property json.
type beadJSON struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Assignee    string   `json:"assignee"`
	Priority    int      `json:"priority"`
	Blockers    []string `json:"blockers,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	CloseReason string   `json:"close_reason,omitempty"`
}

// ScriptRunner executes a named tool script with the given arguments,
// returning its stdout. Implemented by the caller using ollie/exec.
type ScriptRunner func(tool string, args []string) (string, error)

// BeadState is a bead-backed State implementation.
// The bead provides the goal (title + description) and tracks status.
// Conversation history is kept in-memory for the duration of the session.
type BeadState struct {
	ref      BeadRef
	run      ScriptRunner
	bead     beadJSON
	history  []backend.Message
	complete bool
}

// LoadBead reads a bead from 9beads and returns a BeadState ready for use.
// It claims the bead before returning.
func LoadBead(ref BeadRef, run ScriptRunner) (*BeadState, error) {
	out, err := run("read_bead.sh", []string{
		"--mount", ref.Mount,
		"--id", ref.ID,
		"--property", "json",
	})
	if err != nil {
		return nil, fmt.Errorf("read bead %s/%s: %w", ref.Mount, ref.ID, err)
	}

	var b beadJSON
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &b); err != nil {
		return nil, fmt.Errorf("parse bead JSON: %w", err)
	}

	if _, err := run("claim_bead.sh", []string{
		"--mount", ref.Mount,
		"--id", ref.ID,
	}); err != nil {
		return nil, fmt.Errorf("claim bead %s/%s: %w", ref.Mount, ref.ID, err)
	}

	goal := b.Title
	if b.Description != "" {
		goal = b.Title + "\n\n" + b.Description
	}

	return &BeadState{
		ref:  ref,
		run:  run,
		bead: b,
		history: []backend.Message{
			{Role: "user", Content: goal},
		},
	}, nil
}

func (s *BeadState) Goal() string             { return s.history[0].Content }
func (s *BeadState) History() []backend.Message { return s.history }
func (s *BeadState) IsComplete() bool          { return s.complete }

func (s *BeadState) Update(assistant backend.Message, results []ToolResult) error {
	s.history = append(s.history, assistant)
	for _, r := range results {
		s.history = append(s.history, backend.Message{
			Role:       "tool",
			Content:    r.Content,
			ToolCallID: r.ToolCallID,
		})
	}
	return nil
}

func (s *BeadState) MarkComplete() error {
	if _, err := s.run("complete_bead.sh", []string{
		"--mount", s.ref.Mount,
		"--id", s.ref.ID,
	}); err != nil {
		return fmt.Errorf("complete bead %s/%s: %w", s.ref.Mount, s.ref.ID, err)
	}
	s.complete = true
	return nil
}
