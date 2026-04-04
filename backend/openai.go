package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	Model         string               `json:"model"`
	Messages      []openAIMessage      `json:"messages"`
	Tools         []openAITool         `json:"tools,omitempty"`
	Stream        bool                 `json:"stream"`
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type openAIDelta struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
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

func (b *OpenAIBackend) ChatStream(ctx context.Context, model string, messages []Message, tools []Tool) (<-chan StreamEvent, error) {
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
		Model:         model,
		Messages:      wireMessages,
		Tools:         wireTools,
		Stream:        true,
		StreamOptions: &openAIStreamOptions{IncludeUsage: true},
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
	if resp.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		return nil, &RateLimitError{RetryAfter: retryAfter, Message: string(body)}
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai HTTP %d: %s", resp.StatusCode, body)
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

		// finishReason and usage arrive in separate trailing chunks.
		var finishReason string
		var usage Usage

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if line == "data: [DONE]" {
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

			var wire openAIStreamResponse
			if err := json.Unmarshal([]byte(line[6:]), &wire); err != nil {
				ch <- StreamEvent{Done: true, StopReason: fmt.Sprintf("stream decode: %v", err)}
				return
			}

			if wire.Usage.PromptTokens > 0 {
				usage.InputTokens = wire.Usage.PromptTokens
			}
			if wire.Usage.CompletionTokens > 0 {
				usage.OutputTokens = wire.Usage.CompletionTokens
			}

			if len(wire.Choices) == 0 {
				continue
			}

			choice := wire.Choices[0]
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
			if choice.Delta.Content != "" {
				ch <- StreamEvent{Content: choice.Delta.Content}
			}

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

// parseRetryAfter parses the Retry-After header value, which may be an integer
// number of seconds or an HTTP-date. Returns zero if the header is absent or
// unparseable.
func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	header = strings.TrimSpace(header)
	if secs, err := strconv.Atoi(header); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
