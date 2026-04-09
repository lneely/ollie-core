package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"ollie/internal/backend"
)

// PersistedSession is the on-disk format for a saved session.
type PersistedSession struct {
	ID       string            `json:"id"`
	Agent    string            `json:"agent,omitempty"`
	Messages []backend.Message `json:"messages"`
}

// SaveTo writes the full message history to path as JSON.
func (s *Session) SaveTo(path, id, agentName string) error {
	ps := PersistedSession{
		ID:       id,
		Agent:    agentName,
		Messages: s.ctx.Messages(),
	}
	data, err := json.Marshal(ps)
	if err != nil {
		return fmt.Errorf("session save: %w", err)
	}
	return os.WriteFile(path, data, 0600)
}

// RestoreSession reconstructs a Session from a persisted message list,
// applying cfg to the ContextBuilder.
func RestoreSession(messages []backend.Message, cfg ContextConfig) *Session {
	s := &Session{ctx: NewContextBuilder(cfg)}
	for _, m := range messages {
		s.ctx.Append(m)
	}
	for _, m := range messages {
		if m.Role == "user" {
			s.goal = m.Content
			break
		}
	}
	return s
}

// Session is an ephemeral in-memory state backend.
// It lives only for the duration of the process; nothing is persisted.
//
// History() returns a bounded context window via ContextBuilder to prevent
// prompt explosion across multi-step agent loops. Use NewSessionWithConfig
// to override the default limits.
type Session struct {
	goal     string
	ctx      *ContextBuilder
	complete bool
}

// NewSession creates a Session with default context window limits.
func NewSession(goal string) *Session {
	return NewSessionWithConfig(goal, ContextConfig{})
}

// NewSessionWithConfig creates a Session with explicit context window limits.
// Pass a zero-value ContextConfig to use all defaults.
func NewSessionWithConfig(goal string, cfg ContextConfig) *Session {
	s := &Session{
		goal: goal,
		ctx:  NewContextBuilder(cfg),
	}
	s.ctx.Append(backend.Message{Role: "user", Content: goal})
	return s
}

func (s *Session) Goal() string { return s.goal }

// History returns the bounded history window safe for passing to the backend.
// A compaction notice is injected when older messages have been evicted.
func (s *Session) History() []backend.Message {
	return s.ctx.BoundedHistoryWithNotice()
}

func (s *Session) IsComplete() bool { return s.complete }

func (s *Session) Update(assistant backend.Message, results []ToolResult) error {
	s.ctx.Append(assistant)
	for _, r := range results {
		s.ctx.Append(backend.Message{
			Role:       "tool",
			Content:    r.Content,
			ToolCallID: r.ToolCallID,
		})
	}
	return nil
}

func (s *Session) MarkComplete() error {
	s.complete = true
	return nil
}

// Compact proactively summarizes all messages older than the tail window via
// an LLM call, replacing them with a single summary system message.
// Unlike the previous eviction-based approach, this works for any session size.
// Returns (n compacted, summary text, error); n==0 means nothing to compact.
func (s *Session) Compact(ctx context.Context, b backend.Backend, model string) (int, string, error) {
	older := s.ctx.OlderMessages()
	if len(older) == 0 {
		return 0, "", nil
	}

	// Build a prompt asking the model to summarize the older messages.
	var sb strings.Builder
	for _, m := range older {
		fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content)
	}
	prompt := "Summarize the following conversation history concisely, preserving key facts, decisions, and context:\n\n" + sb.String()

	ch, err := b.ChatStream(ctx, model, []backend.Message{
		{Role: "user", Content: prompt},
	}, nil, backend.GenerationParams{})
	if err != nil {
		return 0, "", fmt.Errorf("compact: %w", err)
	}

	var summary strings.Builder
	for ev := range ch {
		if ev.Content != "" {
			summary.WriteString(ev.Content)
		}
		if ev.Done {
			break
		}
	}

	summaryText := strings.TrimSpace(summary.String())

	// Rebuild: system messages + summary + tail.
	system := s.ctx.SystemMessages()
	tail := s.ctx.TailWindow()
	s.ctx.Truncate(0)
	for _, m := range system {
		s.ctx.Append(m)
	}
	s.ctx.Append(backend.Message{
		Role:    "system",
		Content: "[conversation summary: " + summaryText + "]",
	})
	for _, m := range tail {
		s.ctx.Append(m)
	}
	return len(older), summaryText, nil
}

// Rollback removes any trailing non-user messages from history, discarding
// an incomplete assistant turn caused by an interruption.
func (s *Session) Rollback() {
	s.complete = false
	msgs := s.ctx.Messages()
	i := len(msgs)
	for i > 0 && msgs[i-1].Role != "user" {
		i--
	}
	s.ctx.Truncate(i)
}
// flag so the loop will run again on the next call to Loop.Run.
func (s *Session) AppendUserMessage(content string) {
	s.complete = false
	s.ctx.Append(backend.Message{Role: "user", Content: content})
}

// ContextStats returns stats about the current context window.
func (s *Session) ContextStats() ContextStats {
	return s.ctx.Stats()
}

// ContextStatsString returns a one-line human-readable context summary.
func (s *Session) ContextStatsString() string {
	return s.ctx.ContextStatsString()
}

// ContextDebug returns a multi-line breakdown of the bounded history.
func (s *Session) ContextDebug() string {
	return s.ctx.FormatContextDebug()
}
