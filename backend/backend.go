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
	Role       string     `json:"role"`                  // "system" | "user" | "assistant" | "tool"
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`  // set by assistant when calling tools
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

// Response is the model's reply for one Chat call.
type Response struct {
	Message    Message
	StopReason string // "stop" | "tool_calls" | "length" | ...
	Usage      Usage
}

// StreamEvent is a single increment from a streaming Chat call.
// Content is an incremental text delta (append, not replace).
// ToolCalls are complete calls ready for execution when non-empty.
// The final event has Done==true and the fully assembled Message.
type StreamEvent struct {
	Content    string     // incremental text delta (may be "")
	ToolCalls  []ToolCall // complete tool calls, if any assembled this tick
	Done       bool
	StopReason string     // meaningful when Done==true
	Usage      Usage      // meaningful only when Done==true
}

// Backend is the interface all LLM providers must implement.
type Backend interface {
	// Chat sends messages to the model and returns its response.
	// tools may be nil for plain completion requests.
	Chat(ctx context.Context, model string, messages []Message, tools []Tool) (*Response, error)
}

// StreamingBackend is an optional interface; backends that support streaming
// should implement it. Use a type assertion to check.
type StreamingBackend interface {
	// ChatStream returns a channel of incremental events. The channel is
	// closed after the final Done event.
	ChatStream(ctx context.Context, model string, messages []Message, tools []Tool) (<-chan StreamEvent, error)
}
