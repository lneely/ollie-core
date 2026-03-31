package backend

import (
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
}

// -- implementation --

func (b *OllamaBackend) Chat(ctx context.Context, model string, messages []Message, tools []Tool) (*Response, error) {
	wireMessages := make([]ollamaMessage, len(messages))
	for i, m := range messages {
		wireMessages[i] = ollamaMessage{Role: m.Role, Content: m.Content}
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

	req := ollamaChatRequest{
		Model:    model,
		Messages: wireMessages,
		Tools:    wireTools,
		Stream:   false,
	}

	data, err := json.Marshal(req)
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama HTTP %d: %s", resp.StatusCode, body)
	}

	var wire ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, err
	}

	msg := Message{Role: wire.Message.Role, Content: wire.Message.Content}
	for _, tc := range wire.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}

	stopReason := "stop"
	if len(msg.ToolCalls) > 0 {
		stopReason = "tool_calls"
	}

	return &Response{
		Message:    msg,
		StopReason: stopReason,
		Usage:      Usage{InputTokens: wire.PromptEvalCount, OutputTokens: wire.EvalCount},
	}, nil
}
