package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// OllamaBackend speaks the Ollama /api/chat wire format.
type OllamaBackend struct {
	baseURL   *url.URL
	model     string
	client    *http.Client
	ctxLength int
	ctxModel  string
}

func NewOllama(baseURL string) (*OllamaBackend, error) {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	b := &OllamaBackend{baseURL: u, client: &http.Client{}}
	b.model = b.DefaultModel()
	return b, nil
}

func (b *OllamaBackend) Name() string         { return "ollama" }
func (b *OllamaBackend) DefaultModel() string { return "qwen3.5:9b" }
func (b *OllamaBackend) Model() string        { return b.model }
func (b *OllamaBackend) SetModel(m string)    { b.model = m; b.ctxLength = 0 }

func (b *OllamaBackend) Models(ctx context.Context) []string {
	req, _ := http.NewRequestWithContext(ctx, "GET", b.baseURL.JoinPath("/api/tags").String(), nil)
	resp, err := b.client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return nil
	}
	defer resp.Body.Close()
	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return nil
	}
	names := make([]string, len(result.Models))
	for i, m := range result.Models {
		names[i] = m.Name
	}
	return names
}

func (b *OllamaBackend) ContextLength(ctx context.Context) int {
	if b.ctxLength > 0 && b.ctxModel == b.model {
		return b.ctxLength
	}
	body, _ := json.Marshal(map[string]string{"name": b.model})
	req, _ := http.NewRequestWithContext(ctx, "POST", b.baseURL.JoinPath("/api/show").String(), bytes.NewReader(body))
	resp, err := b.client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return 0
	}
	defer resp.Body.Close()
	cl := parseOllamaContextLength(resp.Body)
	if cl > 0 {
		b.ctxLength = cl
		b.ctxModel = b.model
	}
	return cl
}

// parseOllamaContextLength extracts the context_length from an /api/show response body.
func parseOllamaContextLength(r io.Reader) int {
	var result struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	if json.NewDecoder(r).Decode(&result) != nil {
		return 0
	}
	for k, v := range result.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			if f, ok := v.(float64); ok && f > 0 {
				return int(f)
			}
		}
	}
	return 0
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

func (b *OllamaBackend) ChatStream(ctx context.Context, messages []Message, tools []Tool, _ GenerationParams) (<-chan StreamEvent, error) {
	model := b.model
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

	data, _ := json.Marshal(ollamaChatRequest{
		Model:    model,
		Messages: wireMessages,
		Tools:    wireTools,
		Stream:   true,
	})

	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL.JoinPath("/api/chat").String(), bytes.NewReader(data))
	httpReq.Header.Set("Content-Type", "application/json")

	return streamRequest(b.client, httpReq, "ollama", streamOllamaNDJSON)
}

// streamOllamaNDJSON reads Ollama's newline-delimited JSON stream from r
// and sends StreamEvents to ch.
func streamOllamaNDJSON(r io.Reader, ch chan<- StreamEvent) {
	scanner := bufio.NewScanner(r)
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

		if wire.Message.Content != "" {
			ch <- StreamEvent{Content: wire.Message.Content}
		}
	}
	if err := scanner.Err(); err != nil {
		ch <- StreamEvent{Done: true, StopReason: fmt.Sprintf("stream read: %v", err)}
	}
}
