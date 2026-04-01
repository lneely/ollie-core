package agent

import (
	"fmt"
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

	// TailMessages: always preserve the most recent N messages verbatim,
	// even under eviction pressure. Defaults to 6.
	TailMessages int
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

// BoundedHistory returns a context-window-safe slice of messages.
//
// Strategy:
//  1. System messages are always included at the front.
//  2. The most recent TailMessages non-system messages are always kept.
//  3. Older messages are included newest-first until SoftLimit is reached.
//  4. If total still exceeds HardLimit, oldest non-system/non-tail messages
//     are dropped entirely.
func (cb *ContextBuilder) BoundedHistory() []backend.Message {
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

	// Always keep the tail.
	tailStart := len(rest) - cb.cfg.TailMessages
	if tailStart < 0 {
		tailStart = 0
	}
	tail := rest[tailStart:]
	older := rest[:tailStart]

	// Budget remaining after system + tail.
	used := msgSliceChars(system) + msgSliceChars(tail)
	budget := cb.cfg.SoftLimit - used

	// Greedily include older messages newest-first until budget exhausted.
	var included []backend.Message
	for i := len(older) - 1; i >= 0; i-- {
		size := msgChars(older[i])
		if budget-size < 0 {
			break
		}
		budget -= size
		included = append([]backend.Message{older[i]}, included...)
	}

	result := make([]backend.Message, 0, len(system)+len(included)+len(tail))
	result = append(result, system...)
	result = append(result, included...)
	result = append(result, tail...)

	// Hard-limit safety: truncate from the front (after system) if still over.
	for totalChars(result) > cb.cfg.HardLimit && len(result) > len(system)+1 {
		// Drop the oldest non-system message.
		result = append(result[:len(system)], result[len(system)+1:]...)
	}

	return result
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

// BoundedHistoryWithNotice is like BoundedHistory but injects a compaction
// notice message when older messages were dropped, so the model is aware.
func (cb *ContextBuilder) BoundedHistoryWithNotice() []backend.Message {
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

	tailStart := len(rest) - cb.cfg.TailMessages
	if tailStart < 0 {
		tailStart = 0
	}
	tail := rest[tailStart:]
	older := rest[:tailStart]

	used := msgSliceChars(system) + msgSliceChars(tail)
	budget := cb.cfg.SoftLimit - used

	var included []backend.Message
	for i := len(older) - 1; i >= 0; i-- {
		size := msgChars(older[i])
		if budget-size < 0 {
			break
		}
		budget -= size
		included = append([]backend.Message{older[i]}, included...)
	}

	evicted := len(older) - len(included)

	result := make([]backend.Message, 0, len(system)+len(included)+len(tail)+1)
	result = append(result, system...)
	if evicted > 0 {
		result = append(result, contextSummaryLine(evicted))
	}
	result = append(result, included...)
	result = append(result, tail...)

	for totalChars(result) > cb.cfg.HardLimit && len(result) > len(system)+1 {
		result = append(result[:len(system)], result[len(system)+1:]...)
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
	var system []backend.Message
	for _, m := range cb.messages {
		if m.Role == "system" {
			system = append(system, m)
		}
	}
	evicted := len(cb.messages) - len(bounded)
	if evicted < 0 {
		evicted = 0
	}
	return ContextStats{
		StoredMessages:  len(cb.messages),
		BoundedMessages: len(bounded),
		ApproxTokens:    totalChars(bounded) / 4,
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
