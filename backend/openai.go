package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	ID       string             `json:"id"`
	Index    int                `json:"index,omitempty"`
	Type     string             `json:"type"`
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

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIChatRequest struct {
	Model         string              `json:"model"`
	Messages      []openAIMessage     `json:"messages"`
	Tools         []openAITool        `json:"tools,omitempty"`
	Stream        bool                `json:"stream"`
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type openAIChatResponse struct {
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

type openAIDelta struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIChoice struct {
	Delta        openAIDelta   `json:"delta,omitempty"`
	Message      openAIMessage `json:"message,omitempty"`
	FinishReason string        `json:"finish_reason"`
}

type openAIStreamChoice struct {
	Index        int         `json:"index"`
	Delta        openAIDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

type openAIStreamResponse struct {
	Choices []openAIStreamChoice `json:"choices"`
	Usage   openAIUsage          `json:"usage"`
}

// -- implementation --

func (b *OpenAIBackend) doChat(ctx context.Context, model string, messages []Message, tools []Tool, stream bool) (*http.Response, error) {
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
		Stream:   stream,
	}

	// Ask OpenAI to include token usage in the stream. Without this flag
	// the API omits usage entirely from SSE chunks.
	if stream {
		req.StreamOptions = &openAIStreamOptions{IncludeUsage: true}
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

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai HTTP %d: %s", resp.StatusCode, body)
	}

	return resp, nil
}

func (b *OpenAIBackend) Chat(ctx context.Context, model string, messages []Message, tools []Tool) (*Response, error) {
	resp, err := b.doChat(ctx, model, messages, tools, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

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

func (b *OpenAIBackend) ChatStream(ctx context.Context, model string, messages []Message, tools []Tool) (<-chan StreamEvent, error) {
	resp, err := b.doChat(ctx, model, messages, tools, true)
	if err != nil {
		return nil, err
	}

	ch := make(chan StreamEvent, 8)

	go func() {
		defer close(ch)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

		// Accumulate tool call fragments by index (OpenAI sends args incrementally).
		type tcAccum struct {
			id, name, args string
		}
		accum := make(map[int]*tcAccum)

		// finishReason and accumulated usage are tracked separately because
		// OpenAI sends usage in a trailing chunk *after* the finish_reason chunk.
		var finishReason string
		var usage Usage

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if line == "data: [DONE]" {
				// Stream finished. Emit the Done event with whatever usage
				// we have accumulated (may arrive before or after this sentinel).
				ev := StreamEvent{
					Done:       true,
					StopReason: finishReason,
					Usage:      usage,
				}
				for _, a := range accum {
					ev.ToolCalls = append(ev.ToolCalls, ToolCall{
						ID:        a.id,
						Name:      a.name,
						Arguments: json.RawMessage(a.args),
					})
				}
				ch <- ev
				return
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := line[6:] // strip "data: "

			var wire openAIStreamResponse
			if err := json.Unmarshal([]byte(payload), &wire); err != nil {
				ch <- StreamEvent{Done: true, StopReason: fmt.Sprintf("stream decode: %v", err)}
				return
			}

			// Accumulate usage from every chunk; it will be non-zero only on
			// the trailing usage chunk that OpenAI sends after finish_reason.
			if wire.Usage.PromptTokens > 0 {
				usage.InputTokens = wire.Usage.PromptTokens
			}
			if wire.Usage.CompletionTokens > 0 {
				usage.OutputTokens = wire.Usage.CompletionTokens
			}

			if len(wire.Choices) == 0 {
				// Choices-less chunk (e.g. the trailing usage-only chunk).
				continue
			}

			choice := wire.Choices[0]

			// Record finish reason when it arrives.
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}

			// Text content delta.
			if choice.Delta.Content != "" {
				ch <- StreamEvent{Content: choice.Delta.Content}
			}

			// Tool call fragments — accumulate, do not emit yet.
			for _, dtc := range choice.Delta.ToolCalls {
				idx := dtc.Index
				if _, ok := accum[idx]; !ok {
					accum[idx] = &tcAccum{id: dtc.ID, name: dtc.Function.Name}
				}
				a := accum[idx]
				if dtc.ID != "" {
					a.id = dtc.ID
				}
				if dtc.Function.Name != "" {
					a.name = dtc.Function.Name
				}
				a.args += dtc.Function.Arguments
			}
		}
		if err := scanner.Err(); err != nil {
			ch <- StreamEvent{Done: true, StopReason: fmt.Sprintf("stream read: %v", err)}
		}
	}()

	return ch, nil
}
