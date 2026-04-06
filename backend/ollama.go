package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// OllamaBackend speaks the Ollama /api/chat wire format.
type OllamaBackend struct {
	baseURL string
	client  *http.Client
}

func NewOllama(baseURL string) *OllamaBackend {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	return &OllamaBackend{baseURL: baseURL, client: &http.Client{}}
}

// -- wire types --

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type ollamaToolCall struct {
	Function ollamaFunction `json:"function"`
}

type ollamaFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ollamaTool struct {
	Type     string             `json:"type"`
	Function ollamaToolFunction `json:"function"`
}

type ollamaToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
}

type ollamaChatResponse struct {
	Message         ollamaMessage `json:"message"`
	Done            bool          `json:"done"`
	PromptEvalCount int           `json:"prompt_eval_count"`
	EvalCount       int           `json:"eval_count"`
	DoneReason      string        `json:"done_reason"`
}

// -- implementation --

func (b *OllamaBackend) ChatStream(ctx context.Context, model string, messages []Message, tools []Tool, _ GenerationParams) (<-chan StreamEvent, error) {
	wireMessages := make([]ollamaMessage, len(messages))
	for i, m := range messages {
		wireMessages[i] = ollamaMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			wireMessages[i].ToolCalls = append(wireMessages[i].ToolCalls, ollamaToolCall{
				Function: ollamaFunction{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
	}

	var wireTools []ollamaTool
	for _, t := range tools {
		wireTools = append(wireTools, ollamaTool{
			Type: "function",
			Function: ollamaToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}

	data, err := json.Marshal(ollamaChatRequest{
		Model:    model,
		Messages: wireMessages,
		Tools:    wireTools,
		Stream:   true,
	})
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/api/chat", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		return nil, &RateLimitError{RetryAfter: retryAfter, Message: string(body)}
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("ollama HTTP %d: %s", resp.StatusCode, body)
	}

	ch := make(chan StreamEvent, 8)

	go func() {
		defer close(ch)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

		var accumulated []ToolCall

		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			var wire ollamaChatResponse
			if err := json.Unmarshal(line, &wire); err != nil {
				ch <- StreamEvent{Done: true, StopReason: fmt.Sprintf("stream decode: %v", err)}
				return
			}

			for _, tc := range wire.Message.ToolCalls {
				accumulated = append(accumulated, ToolCall{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}

			if wire.Done {
				stopReason := "stop"
				if wire.DoneReason != "" {
					stopReason = wire.DoneReason
				}
				ch <- StreamEvent{
					Done:       true,
					StopReason: stopReason,
					Usage:      Usage{InputTokens: wire.PromptEvalCount, OutputTokens: wire.EvalCount},
					ToolCalls:  accumulated,
				}
				return
			}

			ev := StreamEvent{}
			if wire.Message.Content != "" {
				ev.Content = wire.Message.Content
			}
			if ev.Content != "" {
				ch <- ev
			}
		}
		if err := scanner.Err(); err != nil {
			ch <- StreamEvent{Done: true, StopReason: fmt.Sprintf("stream read: %v", err)}
		}
	}()

	return ch, nil
}
