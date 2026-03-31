package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// OpenAIBackend speaks the OpenAI /v1/chat/completions wire format.
// Compatible with OpenRouter, OpenAI, and any other OpenAI-compatible API.
type OpenAIBackend struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewOpenAI(baseURL, apiKey string) *OpenAIBackend {
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	return &OpenAIBackend{baseURL: baseURL, apiKey: apiKey, client: &http.Client{}}
}

// -- wire types --

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string, not object
}

type openAITool struct {
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openAIChatRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Tools    []openAITool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type openAIChatResponse struct {
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

type openAIChoice struct {
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

// -- implementation --

func (b *OpenAIBackend) Chat(ctx context.Context, model string, messages []Message, tools []Tool) (*Response, error) {
	wireMessages := make([]openAIMessage, len(messages))
	for i, m := range messages {
		wireMessages[i] = openAIMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			wireMessages[i].ToolCalls = append(wireMessages[i].ToolCalls, openAIToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: openAIFunctionCall{
					Name:      tc.Name,
					Arguments: string(tc.Arguments),
				},
			})
		}
	}

	var wireTools []openAITool
	for _, t := range tools {
		wireTools = append(wireTools, openAITool{
			Type: "function",
			Function: openAIToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}

	req := openAIChatRequest{
		Model:    model,
		Messages: wireMessages,
		Tools:    wireTools,
		Stream:   false,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if b.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.apiKey)
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai HTTP %d: %s", resp.StatusCode, body)
	}

	var wire openAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, err
	}

	if len(wire.Choices) == 0 {
		return nil, fmt.Errorf("openai: empty choices in response")
	}

	choice := wire.Choices[0]
	msg := Message{Role: choice.Message.Role, Content: choice.Message.Content}
	for _, tc := range choice.Message.ToolCalls {
		// Arguments arrive as a JSON string; convert to RawMessage.
		args := json.RawMessage(tc.Function.Arguments)
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: args,
		})
	}

	return &Response{
		Message:    msg,
		StopReason: choice.FinishReason,
		Usage:      Usage{InputTokens: wire.Usage.PromptTokens, OutputTokens: wire.Usage.CompletionTokens},
	}, nil
}
