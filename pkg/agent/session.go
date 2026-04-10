package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"ollie/pkg/backend"
)

// PersistedSession is the on-disk format for a saved session.
type PersistedSession struct {
	ID       string            `json:"id"`
	Agent    string            `json:"agent,omitempty"`
	Messages []backend.Message `json:"messages"`
}

// SaveTo writes the full message history to path as JSON.
func (s *Session) saveTo(path, id, agentName string) error {
	ps := PersistedSession{
		ID:       id,
		Agent:    agentName,
		Messages: s.messages,
	}
	data, err := json.Marshal(ps)
	if err != nil {
		return fmt.Errorf("session save: %w", err)
	}
	return os.WriteFile(path, data, 0600)
}

// RestoreSession reconstructs a Session from a persisted message list.
func RestoreSession(messages []backend.Message) *Session {
	s := &Session{messages: messages}
	for _, m := range messages {
		if m.Role == "user" {
			s.goal = m.Content
			break
		}
	}
	return s
}

// Session is an ephemeral in-memory state backend.
type Session struct {
	goal     string
	messages []backend.Message
	complete bool
}

// newSession creates a new Session with an initial user message.
func newSession(goal string) *Session {
	s := &Session{goal: goal}
	s.messages = append(s.messages, backend.Message{Role: "user", Content: goal})
	return s
}

func (s *Session) Goal() string { return s.goal }

func (s *Session) history() []backend.Message {
	return s.messages
}

func (s *Session) isComplete() bool { return s.complete }

func (s *Session) update(assistant backend.Message, results []toolResult) error {
	s.messages = append(s.messages, assistant)
	for _, r := range results {
		s.messages = append(s.messages, backend.Message{
			Role:       "tool",
			Content:    r.Content,
			ToolCallID: r.ToolCallID,
		})
	}
	return nil
}

func (s *Session) markComplete() error {
	s.complete = true
	return nil
}

// Compact proactively summarizes older messages via an LLM call, replacing
// them with a single summary system message plus recent messages.
// Returns (n compacted, summary text, error); n==0 means nothing to compact.
func (s *Session) compact(ctx context.Context, b backend.Backend) (int, string, error) {
	// Keep the last few conversational turns as the "tail".
	const tailCount = 10
	var system, rest []backend.Message
	for _, m := range s.messages {
		if m.Role == "system" {
			system = append(system, m)
		} else {
			rest = append(rest, m)
		}
	}
	if len(rest) <= tailCount {
		return 0, "", nil
	}
	older := rest[:len(rest)-tailCount]
	tail := rest[len(rest)-tailCount:]

	var sb strings.Builder
	for _, m := range older {
		fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content)
	}
	prompt := "Summarize the following conversation history concisely, preserving key facts, decisions, and context:\n\n" + sb.String()

	ch, err := b.ChatStream(ctx, []backend.Message{
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
	s.messages = s.messages[:0]
	s.messages = append(s.messages, system...)
	s.messages = append(s.messages, backend.Message{
		Role:    "system",
		Content: "[conversation summary: " + summaryText + "]",
	})
	s.messages = append(s.messages, tail...)
	return len(older), summaryText, nil
}

// Rollback removes any trailing non-user messages from history, discarding
// an incomplete assistant turn caused by an interruption.
func (s *Session) rollback() {
	s.complete = false
	i := len(s.messages)
	for i > 0 && s.messages[i-1].Role != "user" {
		i--
	}
	s.messages = s.messages[:i]
}

func (s *Session) appendUserMessage(content string) {
	s.complete = false
	s.messages = append(s.messages, backend.Message{Role: "user", Content: content})
}

// contextStatsString returns a one-line human-readable context summary.
func (s *Session) contextStatsString() string {
	chars := 0
	for _, m := range s.messages {
		chars += len(m.Content)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Name) + len(tc.Arguments)
		}
	}
	return fmt.Sprintf("context: ~%d tokens (%d msgs)", chars/4, len(s.messages))
}

// contextDebug returns a multi-line breakdown of the history.
func (s *Session) contextDebug() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("=== %d messages ===\n", len(s.messages)))
	for i, m := range s.messages {
		preview := m.Content
		if len(preview) > 80 {
			preview = preview[:80] + "..."
		}
		chars := len(m.Content)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Name) + len(tc.Arguments)
		}
		sb.WriteString(fmt.Sprintf("  [%d] role=%-10s chars=%-6d %q\n", i, m.Role, chars, preview))
	}
	return sb.String()
}
