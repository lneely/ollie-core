package agent

import "ollie/backend"

// Session is an ephemeral in-memory state backend.
// It lives only for the duration of the process; nothing is persisted.
type Session struct {
	goal     string
	history  []backend.Message
	complete bool
}

// NewSession creates a Session with the goal already placed as the first
// user message in history, so the loop can call backend.Chat immediately.
func NewSession(goal string) *Session {
	return &Session{
		goal: goal,
		history: []backend.Message{
			{Role: "user", Content: goal},
		},
	}
}

func (s *Session) Goal() string             { return s.goal }
func (s *Session) History() []backend.Message { return s.history }
func (s *Session) IsComplete() bool          { return s.complete }

func (s *Session) Update(assistant backend.Message, results []ToolResult) error {
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

func (s *Session) MarkComplete() error {
	s.complete = true
	return nil
}
