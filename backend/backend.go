// Package backend defines the Backend interface and shared types for LLM
// providers. All backends speak the same canonical types; provider-specific
// wire formats are handled inside each implementation.
package backend

import (
	"context"
	"encoding/json"
)

// Message is a single conversation turn.
type Message struct {
	Role       string     `json:"role"`                   // "system" | "user" | "assistant" | "tool"
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // set by assistant when calling tools
	ToolCallID string     `json:"tool_call_id,omitempty"` // set on role=tool replies (required by OpenAI)
}

// Tool describes a callable function exposed to the model.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema object
}

// ToolCall is the model's request to invoke a function.
type ToolCall struct {
	ID        string          `json:"id,omitempty"` // provider-assigned; may be empty (Ollama)
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // always a JSON object
}

// Usage holds token counts for a single Chat call.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// StreamEvent is a single increment from a streaming chat call.
// Content is an incremental text delta (append, not replace).
// ToolCalls accumulates complete calls; they may arrive on any event.
// The final event has Done==true.
type StreamEvent struct {
	Content    string     // incremental text delta (may be "")
	ToolCalls  []ToolCall // complete tool calls assembled so far
	Done       bool
	StopReason string // meaningful when Done==true
	Usage      Usage  // meaningful only when Done==true
}

// Backend is the interface all LLM providers must implement.
// Streaming is the only supported mode; backends that wrap blocking APIs
// should implement ChatStream as a single-event stream.
type Backend interface {
	ChatStream(ctx context.Context, model string, messages []Message, tools []Tool) (<-chan StreamEvent, error)
}
