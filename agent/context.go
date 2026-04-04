package agent

import (
	"fmt"
	"slices"
	"strings"

	"ollie/backend"
)

// ContextConfig controls the bounded context window behaviour.
// All sizes are in characters (a rough proxy for tokens; ~4 chars per token).
type ContextConfig struct {
	// SoftLimit: if assembled history exceeds this, begin evicting old messages.
	// Defaults to 24000 (~6k tokens).
	SoftLimit int

	// HardLimit: absolute ceiling; messages are truncated to fit.
	// Defaults to 96000 (~24k tokens).
	HardLimit int

	// MaxToolOutputChars: tool result messages longer than this are truncated
	// before being added to history. Defaults to 2000.
	MaxToolOutputChars int

	// TailMessages: always preserve the most recent N conversational turns
	// (user + plain-assistant messages) verbatim, even under eviction pressure.
	// Tool-call exchanges between those turns are included, but processed
	// tool exchanges do not count toward this limit.
	// Defaults to 6.
	TailMessages int

	// FixedOverheadChars is the estimated character count of fixed per-request
	// overhead (system prompt, tool schemas) sent outside the ContextBuilder.
	// Subtracted from the budget before greedy inclusion.
	// Defaults to 0.
	FixedOverheadChars int
}

func defaultContextConfig() ContextConfig {
	return ContextConfig{
		SoftLimit:          24_000,
		HardLimit:          96_000,
		MaxToolOutputChars: 2_000,
		TailMessages:       6,
	}
}

func (c *ContextConfig) setDefaults() {
	if c.SoftLimit <= 0 {
		c.SoftLimit = 24_000
	}
	if c.HardLimit <= 0 {
		c.HardLimit = 96_000
	}
	if c.MaxToolOutputChars <= 0 {
		c.MaxToolOutputChars = 2_000
	}
	if c.TailMessages <= 0 {
		c.TailMessages = 6
	}
}

// ContextBuilder manages a rolling bounded history window.
type ContextBuilder struct {
	cfg      ContextConfig
	messages []backend.Message // full unbounded log
}

// NewContextBuilder creates a ContextBuilder with the given config.
// Pass a zero-value ContextConfig to use all defaults.
func NewContextBuilder(cfg ContextConfig) *ContextBuilder {
	cfg.setDefaults()
	return &ContextBuilder{cfg: cfg}
}

// Append adds a message to the full history.
// Tool result messages are truncated to MaxToolOutputChars before storage.
func (cb *ContextBuilder) Append(m backend.Message) {
	if m.Role == "tool" && len(m.Content) > cb.cfg.MaxToolOutputChars {
		m.Content = m.Content[:cb.cfg.MaxToolOutputChars] +
			fmt.Sprintf("\n... [truncated %d chars]", len(m.Content)-cb.cfg.MaxToolOutputChars)
	}
	cb.messages = append(cb.messages, m)
}

// Messages returns the full stored message log (unbounded).
func (cb *ContextBuilder) Messages() []backend.Message {
	return cb.messages
}

// Truncate discards all messages after index i.
func (cb *ContextBuilder) Truncate(i int) {
	if i < len(cb.messages) {
		cb.messages = cb.messages[:i]
	}
}

// EvictedMessages returns the messages that would be dropped by buildBounded.
func (cb *ContextBuilder) EvictedMessages() []backend.Message {
	var rest []backend.Message
	for _, m := range cb.messages {
		if m.Role != "system" {
			rest = append(rest, m)
		}
	}
	ts := computeTailStart(rest, cb.cfg.TailMessages)
	older := rest[:ts]

	used := cb.cfg.FixedOverheadChars + msgSliceChars(cb.messages) - msgSliceChars(older)
	budget := cb.cfg.SoftLimit - used

	var included []backend.Message
	for i := len(older) - 1; i >= 0; i-- {
		size := msgChars(older[i])
		if budget-size < 0 {
			break
		}
		budget -= size
		included = append(included, older[i])
	}
	evictCount := len(older) - len(included)
	if evictCount == 0 {
		return nil
	}
	return older[:evictCount]
}

// BoundedHistory returns a context-window-safe slice of messages.
func (cb *ContextBuilder) BoundedHistory() []backend.Message {
	return cb.buildBounded(false)
}

// BoundedHistoryWithNotice is like BoundedHistory but injects a compaction
// notice message when older messages were dropped, so the model is aware.
func (cb *ContextBuilder) BoundedHistoryWithNotice() []backend.Message {
	return cb.buildBounded(true)
}

// buildBounded is the shared implementation for BoundedHistory and
// BoundedHistoryWithNotice.
//
// Strategy:
//  1. System messages are always included at the front.
//  2. The most recent TailMessages conversational turns (user + plain-assistant)
//     are always kept, along with any tool exchanges between or trailing them.
//  3. Older messages are included newest-first until SoftLimit is reached,
//     accounting for FixedOverheadChars (system prompt, tool schemas).
//  4. If total still exceeds HardLimit, oldest non-system/non-tail messages
//     are dropped atomically (assistant[tool_calls]+tool pairs together).
func (cb *ContextBuilder) buildBounded(injectNotice bool) []backend.Message {
	var system []backend.Message
	var rest []backend.Message

	for _, m := range cb.messages {
		if m.Role == "system" {
			system = append(system, m)
		} else {
			rest = append(rest, m)
		}
	}

	if len(rest) == 0 {
		return system
	}

	ts := computeTailStart(rest, cb.cfg.TailMessages)
	tail := rest[ts:]
	older := rest[:ts]

	// Budget: fixed overhead + system + tail chars subtracted up front.
	used := cb.cfg.FixedOverheadChars + msgSliceChars(system) + msgSliceChars(tail)
	budget := cb.cfg.SoftLimit - used

	// Greedily include older messages newest-first until budget exhausted.
	var included []backend.Message
	for i := len(older) - 1; i >= 0; i-- {
		size := msgChars(older[i])
		if budget-size < 0 {
			break
		}
		budget -= size
		included = append(included, older[i])
	}
	slices.Reverse(included)

	evicted := len(older) - len(included)

	result := make([]backend.Message, 0, len(system)+len(included)+len(tail)+1)
	result = append(result, system...)
	if injectNotice && evicted > 0 {
		result = append(result, contextSummaryLine(evicted))
	}
	result = append(result, included...)
	result = append(result, tail...)

	// Hard-limit safety: drop from front (after system) atomically.
	// Account for fixed overhead in the ceiling check.
	ceiling := cb.cfg.HardLimit - cb.cfg.FixedOverheadChars
	for totalChars(result) > ceiling && len(result) > len(system)+1 {
		drop := len(system)
		if result[drop].Role == "assistant" && len(result[drop].ToolCalls) > 0 {
			end := drop + 1
			for end < len(result) && result[end].Role == "tool" {
				end++
			}
			result = append(result[:drop], result[end:]...)
		} else {
			result = append(result[:drop], result[drop+1:]...)
		}
	}

	return sanitizeHistory(result)
}

// computeTailStart returns the index in rest where the tail begins.
//
// Only user and plain-assistant (no tool calls) messages count toward
// tailCount. Any trailing in-progress exchange (assistant[tool_calls] +
// consecutive tool results with nothing after) is always included and does
// not consume quota. This prevents processed tool exchanges from crowding
// out genuine conversational context.
func computeTailStart(rest []backend.Message, tailCount int) int {
	n := len(rest)
	if n == 0 || tailCount <= 0 {
		return 0
	}

	// Identify any trailing in-progress exchange: ends with tool result(s)
	// that have not yet been followed by an assistant reply.
	inProgStart := n
	if rest[n-1].Role == "tool" {
		j := n - 1
		for j > 0 && rest[j].Role == "tool" {
			j--
		}
		if rest[j].Role == "assistant" && len(rest[j].ToolCalls) > 0 {
			inProgStart = j
		}
	}

	// Count tailCount conversational messages (user or plain assistant)
	// from inProgStart-1 backward.
	counted := 0
	for i := inProgStart - 1; i >= 0; i-- {
		m := rest[i]
		if m.Role == "user" || (m.Role == "assistant" && len(m.ToolCalls) == 0) {
			counted++
			if counted >= tailCount {
				return i
			}
		}
	}
	// Fewer than tailCount conversational messages: include everything.
	return 0
}

// Len returns the number of stored messages.
func (cb *ContextBuilder) Len() int { return len(cb.messages) }

// ApproxTokens returns a rough token estimate for the bounded history
// using the 4-chars-per-token heuristic.
func (cb *ContextBuilder) ApproxTokens() int {
	return totalChars(cb.BoundedHistory()) / 4
}

// --- helpers ----------------------------------------------------------------

func msgChars(m backend.Message) int {
	n := len(m.Content)
	for _, tc := range m.ToolCalls {
		n += len(tc.Name) + len(tc.Arguments)
	}
	return n
}

func msgSliceChars(msgs []backend.Message) int {
	total := 0
	for _, m := range msgs {
		total += msgChars(m)
	}
	return total
}

func totalChars(msgs []backend.Message) int {
	return msgSliceChars(msgs)
}

// contextSummaryLine produces a one-line summary injected when old messages
// are evicted, so the model knows compaction occurred.
func contextSummaryLine(evicted int) backend.Message {
	return backend.Message{
		Role:    "user",
		Content: fmt.Sprintf("[context compacted: %d earlier messages omitted to stay within token budget]", evicted),
	}
}

// sanitizeHistory removes tool messages that are not preceded by an assistant
// message with tool calls.  This prevents 400 errors from backends that
// reject tool messages not immediately following an assistant[tool_calls]
// turn.  The situation arises when context compaction evicts an
// assistant[tool_calls] message while its paired tool[result] messages remain
// in the tail window, or when the compaction notice (a user message) is
// injected immediately before such a tail.
func sanitizeHistory(msgs []backend.Message) []backend.Message {
	result := make([]backend.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "tool" {
			// Walk backward through already-accepted messages to find the
			// nearest non-tool predecessor.
			preceded := false
			for j := len(result) - 1; j >= 0; j-- {
				if result[j].Role != "tool" {
					preceded = result[j].Role == "assistant" && len(result[j].ToolCalls) > 0
					break
				}
			}
			if !preceded {
				continue // drop orphaned tool message
			}
		}
		result = append(result, m)
	}
	return result
}

// ContextStats describes the current state of the context window.
type ContextStats struct {
	StoredMessages  int
	BoundedMessages int
	ApproxTokens    int
	Evicted         int
	SoftLimit       int
	HardLimit       int
}

func (cb *ContextBuilder) Stats() ContextStats {
	bounded := cb.BoundedHistory()
	evicted := len(cb.messages) - len(bounded)
	if evicted < 0 {
		evicted = 0
	}
	return ContextStats{
		StoredMessages:  len(cb.messages),
		BoundedMessages: len(bounded),
		ApproxTokens:    (totalChars(bounded) + cb.cfg.FixedOverheadChars) / 4,
		Evicted:         evicted,
		SoftLimit:       cb.cfg.SoftLimit,
		HardLimit:       cb.cfg.HardLimit,
	}
}

// ContextStatsString returns a one-line human-readable summary.
func (cb *ContextBuilder) ContextStatsString() string {
	s := cb.Stats()
	evictedStr := ""
	if s.Evicted > 0 {
		evictedStr = fmt.Sprintf(", %d evicted", s.Evicted)
	}
	return fmt.Sprintf("context: ~%d tokens (%d/%d msgs%s)",
		s.ApproxTokens, s.BoundedMessages, s.StoredMessages, evictedStr)
}

// FormatContextDebug returns a multi-line breakdown of the bounded history
// useful for debug output.
func (cb *ContextBuilder) FormatContextDebug() string {
	var sb strings.Builder
	s := cb.Stats()
	sb.WriteString(fmt.Sprintf("=== context window: ~%d tokens | %d bounded / %d stored | soft=%d hard=%d ===\n",
		s.ApproxTokens, s.BoundedMessages, s.StoredMessages, s.SoftLimit/4, s.HardLimit/4))
	for i, m := range cb.BoundedHistory() {
		preview := m.Content
		if len(preview) > 80 {
			preview = preview[:80] + "..."
		}
		sb.WriteString(fmt.Sprintf("  [%d] role=%-10s chars=%-6d %q\n", i, m.Role, msgChars(m), preview))
	}
	return sb.String()
}
