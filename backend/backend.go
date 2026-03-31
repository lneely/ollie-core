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

// Backend is the interface all LLM providers must implement.
type Backend interface {
	// Chat sends messages to the model and returns its response.
	// tools may be nil for plain completion requests.
	Chat(ctx context.Context, model string, messages []Message, tools []Tool) (*Response, error)
}
