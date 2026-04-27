// Package backend defines the Backend interface and shared types for LLM
// providers. All backends speak the same canonical types; provider-specific
// wire formats are handled inside each implementation.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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

// Usage holds token counts and optional cost for a single Chat call.
// CostUSD is non-zero only when the backend reports it directly (e.g. OpenRouter).
type Usage struct {
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

// StreamEvent is a single increment from a streaming chat call.
// Content is an incremental text delta (append, not replace).
// Reasoning is an incremental thinking/reasoning delta (append, not replace).
// ToolCalls accumulates complete calls; they may arrive on any event.
// The final event has Done==true.
type StreamEvent struct {
	Content    string     // incremental text delta (may be "")
	Reasoning  string     // incremental thinking delta (may be ""); Anthropic extended thinking, OpenAI reasoning
	ToolCalls  []ToolCall // complete tool calls assembled so far
	Done       bool
	StopReason string // meaningful when Done==true
	Usage      Usage  // meaningful only when Done==true
}

// RateLimitError is returned when the backend responds with HTTP 429.
// RetryAfter is the suggested wait duration; zero means no hint was given.
type RateLimitError struct {
	RetryAfter time.Duration
	Message    string
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("rate limited (retry after %v): %s", e.RetryAfter, e.Message)
	}
	return fmt.Sprintf("rate limited: %s", e.Message)
}

// TransientError is returned for retryable backend failures: 5xx HTTP responses
// and network-level errors (connection reset, DNS failure, etc.).
type TransientError struct {
	Message string
}

func (e *TransientError) Error() string { return e.Message }

// ContextOverflowError is returned when the request exceeds the model's
// context window. The caller should compact history and retry.
type ContextOverflowError struct {
	Message string
}

func (e *ContextOverflowError) Error() string { return e.Message }

// GenerationParams controls sampling behaviour for a single ChatStream call.
// Zero values mean "use the API default".
type GenerationParams struct {
	MaxTokens        int      // 0 = no limit
	Temperature      *float64 // nil = API default
	FrequencyPenalty *float64 // nil = API default
	PresencePenalty  *float64 // nil = API default
	ThinkingBudget   int      // Anthropic extended thinking token budget; 0 = disabled
}

// Backend is the interface all LLM providers must implement.
// Streaming is the only supported mode; backends that wrap blocking APIs
// should implement ChatStream as a single-event stream.
type Backend interface {
	ChatStream(ctx context.Context, messages []Message, tools []Tool, params GenerationParams) (<-chan StreamEvent, error)
	// Name returns a short human-readable label for this backend (e.g. "ollama", "openrouter").
	Name() string
	// DefaultModel returns a reasonable default model name for this backend.
	DefaultModel() string
	// Model returns the currently active model name.
	Model() string
	// SetModel changes the active model.
	SetModel(model string)
	// ContextLength returns the context window size in tokens for the active model.
	// Returns 0 if unknown. May make an API call; results should be cached by the implementation.
	ContextLength(ctx context.Context) int
	// Models returns the list of available model IDs from the provider.
	Models(ctx context.Context) []string
}

// streamRequest executes req via client, handles status errors, and spawns a
// goroutine that feeds resp.Body through parseFn into the returned channel.
// label is used in error messages (e.g. "openai", "ollama").
// Returns *RateLimitError for 429, *TransientError for 5xx and network errors,
// and a plain error for other non-200 responses.
func streamRequest(client *http.Client, req *http.Request, label string, parseFn func(io.Reader, chan<- StreamEvent)) (<-chan StreamEvent, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, &TransientError{Message: err.Error()}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &RateLimitError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")), Message: string(body)}
	}
	if resp.StatusCode >= 500 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &TransientError{Message: fmt.Sprintf("%s HTTP %d: %s", label, resp.StatusCode, string(body))}
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		msg := string(body)
		if resp.StatusCode == http.StatusBadRequest && isContextOverflow(msg) {
			return nil, &ContextOverflowError{Message: fmt.Sprintf("%s HTTP %d: %s", label, resp.StatusCode, msg)}
		}
		return nil, fmt.Errorf("%s HTTP %d: %s", label, resp.StatusCode, msg)
	}
	ch := make(chan StreamEvent, 8)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		parseFn(resp.Body, ch)
	}()
	return ch, nil
}

// isContextOverflow returns true when a 400 response body indicates the request
// exceeded the model's context window.
func isContextOverflow(body string) bool {
	markers := []string{
		"context_length_exceeded", // OpenAI/OpenRouter error code
		"prompt is too long",      // Anthropic
		"Please reduce the length", // OpenAI prose
		"too many tokens",
	}
	lower := strings.ToLower(body)
	for _, m := range markers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}
