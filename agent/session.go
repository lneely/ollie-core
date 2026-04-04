package agent

import "ollie/backend"

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
