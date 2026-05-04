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
	// Cumulative usage tracking.
	TotalInputTokens         int
	TotalCachedInputTokens   int
	TotalCacheCreationTokens int
	TotalOutputTokens        int
	TotalRequests            int
	Estimated                bool    // true if any usage was estimated rather than reported by the backend
	LastTurnCostUSD          float64 // cost of the most recently completed turn in USD
	SessionCostUSD           float64 // cumulative cost of all turns in this session in USD
	// per-turn accumulators; reset at the start of each turn
	turnInputTokens    int
	turnCachedTokens   int
	turnCreationTokens int
	turnOutputTokens   int
	turnCostUSD        float64 // >0 when backend reported cost directly (e.g. OpenRouter)
}

// newSession creates a new empty Session. The caller is responsible for
// appending the initial user message via appendUserMessage.
func newSession(goal string) *Session {
	return &Session{goal: goal}
}

func (s *Session) history() []backend.Message {
	return s.messages
}

func (s *Session) addUsage(u backend.Usage, estimated bool) {
	s.TotalInputTokens += u.InputTokens
	s.TotalCachedInputTokens += u.CachedInputTokens
	s.TotalCacheCreationTokens += u.CacheCreationTokens
	s.TotalOutputTokens += u.OutputTokens
	s.TotalRequests++
	s.turnInputTokens += u.InputTokens
	s.turnCachedTokens += u.CachedInputTokens
	s.turnCreationTokens += u.CacheCreationTokens
	s.turnOutputTokens += u.OutputTokens
	s.turnCostUSD += u.CostUSD
	if estimated {
		s.Estimated = true
	}
}

func (s *Session) resetTurnAccumulators() {
	s.turnInputTokens = 0
	s.turnCachedTokens = 0
	s.turnCreationTokens = 0
	s.turnOutputTokens = 0
	s.turnCostUSD = 0
}

// recordTurnCost computes and stores the last turn's cost, adding it to the
// session total. model is used for the pricing-table fallback when the backend
// did not report cost directly.
func (s *Session) recordTurnCost(model string) {
	var cost float64
	if s.turnCostUSD > 0 {
		cost = s.turnCostUSD
	} else {
		cost = computeCostUSD(model, backend.Usage{
			InputTokens:         s.turnInputTokens,
			CachedInputTokens:   s.turnCachedTokens,
			CacheCreationTokens: s.turnCreationTokens,
			OutputTokens:        s.turnOutputTokens,
		})
	}
	s.LastTurnCostUSD = cost
	s.SessionCostUSD += cost
}

func (s *Session) update(assistant backend.Message, results []toolResult) {
	s.messages = append(s.messages, assistant)
	for _, r := range results {
		s.messages = append(s.messages, backend.Message{
			Role:       "tool",
			Content:    r.Content,
			ToolCallID: r.ToolCallID,
		})
	}
}

// removeCancelledToolResults filters out tool results that were cancelled due
// to interrupt, keeping completed work. Also removes the corresponding tool
// calls from assistant messages to maintain a valid message sequence.
func (s *Session) removeCancelledToolResults() {
	// First pass: collect cancelled tool call IDs
	cancelled := make(map[string]bool)
	for _, m := range s.messages {
		if m.Role == "tool" && isCancelledToolResult(m.Content) {
			cancelled[m.ToolCallID] = true
		}
	}
	if len(cancelled) == 0 {
		return
	}

	// Second pass: filter messages and prune tool calls from assistant messages
	filtered := s.messages[:0]
	for _, m := range s.messages {
		if m.Role == "tool" && cancelled[m.ToolCallID] {
			continue
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			// Remove cancelled tool calls from this assistant message
			kept := m.ToolCalls[:0]
			for _, tc := range m.ToolCalls {
				if !cancelled[tc.ID] {
					kept = append(kept, tc)
				}
			}
			if len(kept) == 0 && m.Content == "" {
				// All tool calls cancelled and no text content - skip message
				continue
			}
			m.ToolCalls = kept
		}
		filtered = append(filtered, m)
	}
	s.messages = filtered
}

func isCancelledToolResult(content string) bool {
	return strings.Contains(content, `"status":"cancelled"`) ||
		strings.Contains(content, "tool execution interrupted by user")
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
	s.messages = buildCompactedHistory(summaryText)
	return beforeCount - len(s.messages), summaryText, nil
}

// buildCompactedHistory constructs the post-compaction message list.
func buildCompactedHistory(summary string) []backend.Message {
	return []backend.Message{{
		Role:    "user",
		Content: compactionSummaryPrefix + "\n" + strings.TrimSpace(summary),
	}}
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
	s.messages = append(s.messages, backend.Message{Role: "user", Content: content})
}

// cloneMessages returns a deep copy of the message slice.
func cloneMessages(msgs []backend.Message) []backend.Message {
	out := make([]backend.Message, len(msgs))
	for i, m := range msgs {
		out[i] = backend.Message{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		if len(m.ToolCalls) > 0 {
			out[i].ToolCalls = make([]backend.ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				out[i].ToolCalls[j] = backend.ToolCall{
					ID:        tc.ID,
					Name:      tc.Name,
					Arguments: append(json.RawMessage(nil), tc.Arguments...),
				}
			}
		}
	}
	return out
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
