package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"ollie/pkg/backend"
)

const (
	compactionPrompt = `You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.

Include:
- Current progress and key decisions made
- Important context, constraints, or user preferences
- What remains to be done (clear next steps)
- Any critical data, examples, or references needed to continue

Be concise, structured, and focused on helping the next LLM seamlessly continue the work.`

	compactionSummaryPrefix = `Another language model started to solve this problem and produced a summary of its thinking process. You also have access to the state of the tools that were used by that language model. Use this to build on the work that has already been done and avoid duplicating work.

Here is the summary produced by the other language model, use the information in this summary to assist with your own analysis:`

	// compactionUserTokenBudget is the approximate token budget for preserving
	// recent user messages in the compacted history.
	compactionUserTokenBudget = 20000
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
	// Cumulative usage tracking.
	TotalInputTokens  int
	TotalOutputTokens int
	TotalRequests     int
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

func (s *Session) addUsage(u backend.Usage) {
	s.TotalInputTokens += u.InputTokens
	s.TotalOutputTokens += u.OutputTokens
	s.TotalRequests++
}

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

// PreCompactionSnapshot returns a copy of the current messages for persistence
// before compaction. Call this before compact().
func (s *Session) PreCompactionSnapshot() []backend.Message {
	return slices.Clone(s.messages)
}

// Compact summarizes the conversation via an LLM call, replacing the history
// with system messages + preserved user messages + a structured summary.
// Returns (n compacted, summary text, error); n==0 means nothing to compact.
func (s *Session) compact(ctx context.Context, b backend.Backend) (int, string, error) {
	if len(s.messages) <= 4 {
		return 0, "", nil
	}

	// Flatten tool calls into plain text so the compaction request
	// doesn't need tool schemas.
	flattened := flattenToolMessages(s.messages)
	flattened = append(flattened, backend.Message{
		Role:    "user",
		Content: compactionPrompt,
	})

	ch, err := b.ChatStream(ctx, flattened, nil, backend.GenerationParams{})
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
	if summaryText == "" {
		return 0, "", fmt.Errorf("compact: empty summary")
	}

	// Rebuild: system messages + preserved user messages + summary.
	beforeCount := len(s.messages)
	s.messages = buildCompactedHistory(s.messages, summaryText)
	return beforeCount - len(s.messages), summaryText, nil
}

// buildCompactedHistory constructs the post-compaction message list:
// system messages + recent user messages (within token budget) + summary.
func buildCompactedHistory(history []backend.Message, summary string) []backend.Message {
	var result []backend.Message
	for _, m := range history {
		if m.Role == "system" {
			result = append(result, m)
		}
	}
	result = append(result, selectUserMessages(history, compactionUserTokenBudget)...)
	result = append(result, backend.Message{
		Role:    "user",
		Content: compactionSummaryPrefix + "\n" + strings.TrimSpace(summary),
	})
	return result
}

// selectUserMessages picks recent user messages (newest first) up to a token budget.
func selectUserMessages(history []backend.Message, tokenBudget int) []backend.Message {
	var selected []backend.Message
	remaining := tokenBudget
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role != "user" {
			continue
		}
		text := strings.TrimSpace(m.Content)
		if text == "" || strings.HasPrefix(text, compactionSummaryPrefix) {
			continue
		}
		tokens := (len(text) + 3) / 4
		if tokens > remaining {
			break
		}
		remaining -= tokens
		selected = append(selected, backend.Message{Role: "user", Content: text})
	}
	slices.Reverse(selected)
	return selected
}

// flattenToolMessages converts tool call/result sequences into plain text
// so the compaction request doesn't include tool-specific structures that
// the API may reject when no tools are defined.
func flattenToolMessages(messages []backend.Message) []backend.Message {
	out := make([]backend.Message, 0, len(messages))
	for _, m := range messages {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			var sb strings.Builder
			if m.Content != "" {
				sb.WriteString(m.Content)
				sb.WriteString("\n\n")
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&sb, "[Tool call: %s(%s)]\n", tc.Name, string(tc.Arguments))
			}
			out = append(out, backend.Message{Role: "assistant", Content: sb.String()})
		case m.Role == "tool":
			text := m.Content
			if len(text) > 4000 {
				text = text[:4000] + "..."
			}
			out = append(out, backend.Message{
				Role:    "user",
				Content: fmt.Sprintf("[Tool result for %s]:\n%s", m.ToolCallID, text),
			})
		default:
			out = append(out, m)
		}
	}
	return out
}

func (s *Session) appendUserMessage(content string) {
	s.complete = false
	s.messages = append(s.messages, backend.Message{Role: "user", Content: content})
}

// contextStatsString returns a one-line human-readable context summary.
func (s *Session) contextStatsString() string {
	return fmt.Sprintf("context: ~%d tokens (%d msgs)", s.estimateTokens(), len(s.messages))
}

// estimateTokens returns a rough token count (~4 chars per token).
func (s *Session) estimateTokens() int {
	chars := 0
	for _, m := range s.messages {
		chars += len(m.Content)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Name) + len(tc.Arguments)
		}
	}
	return chars / 4
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
