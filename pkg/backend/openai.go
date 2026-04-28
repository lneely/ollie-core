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
	"strconv"
	"strings"
	"time"
)

// OpenAIBackend speaks the OpenAI /v1/chat/completions wire format.
// Compatible with OpenRouter, OpenAI, and any other OpenAI-compatible API.
type OpenAIBackend struct {
	name         string
	baseURL      *url.URL
	apiKey       string
	model        string
	client       *http.Client
	extraHeaders map[string]string // optional; applied after Authorization
	ctxLength    int               // cached; 0 = not yet fetched
	ctxModel     string            // model the cached value is for
	cachedModels []openAIModelInfo // cached model list
}

type openAIModelInfo struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
}

func NewOpenAI(name, baseURL, apiKey string) (*OpenAIBackend, error) {
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	b := &OpenAIBackend{name: name, baseURL: u, apiKey: apiKey, client: &http.Client{}}
	b.model = b.DefaultModel()
	return b, nil
}

func (b *OpenAIBackend) Name() string         { return b.name }
func (b *OpenAIBackend) Model() string        { return b.model }
func (b *OpenAIBackend) SetModel(m string)    { b.model = m; b.ctxLength = 0 }

func (b *OpenAIBackend) fetchModels(ctx context.Context) []openAIModelInfo {
	if len(b.cachedModels) > 0 {
		return b.cachedModels
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", b.baseURL.JoinPath("/v1/models").String(), nil)
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}
	resp, err := b.client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return nil
	}
	defer resp.Body.Close()
	var result struct {
		Data []openAIModelInfo `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return nil
	}
	b.cachedModels = result.Data
	return b.cachedModels
}

func (b *OpenAIBackend) ContextLength(ctx context.Context) int {
	if b.ctxLength > 0 && b.ctxModel == b.model {
		return b.ctxLength
	}
	for _, m := range b.fetchModels(ctx) {
		if m.ID == b.model {
			b.ctxLength = m.ContextLength
			b.ctxModel = b.model
			return b.ctxLength
		}
	}
	return 0
}

func (b *OpenAIBackend) Models(ctx context.Context) []string {
	models := b.fetchModels(ctx)
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return ids
}
func (b *OpenAIBackend) DefaultModel() string {
	switch b.name {
	case "openrouter":
		return "deepseek/deepseek-v3.2"
	case "anthropic":
		return "claude-sonnet-4-5"
	default:
		return "qwen3.5:9b"
	}
}

// -- wire types --

type openAIContentBlock struct {
	Type         string              `json:"type"`
	Text         string              `json:"text"`
	CacheControl *anthropicCacheCtrl `json:"cache_control,omitempty"`
}

type openAIMessage struct {
	Role          string               `json:"role"`
	Content       *string              `json:"content"`
	ContentBlocks []openAIContentBlock `json:"-"` // non-nil overrides Content during marshaling
	ToolCalls     []openAIToolCall     `json:"tool_calls,omitempty"`
	ToolCallID    string               `json:"tool_call_id,omitempty"`
}

// MarshalJSON encodes content as an array of blocks when ContentBlocks is set
// (used for provider-specific cache_control extensions), otherwise falls back
// to the standard *string Content field.
func (m openAIMessage) MarshalJSON() ([]byte, error) {
	if len(m.ContentBlocks) > 0 {
		return json.Marshal(struct {
			Role       string               `json:"role"`
			Content    []openAIContentBlock `json:"content"`
			ToolCalls  []openAIToolCall     `json:"tool_calls,omitempty"`
			ToolCallID string               `json:"tool_call_id,omitempty"`
		}{m.Role, m.ContentBlocks, m.ToolCalls, m.ToolCallID})
	}
	type plain openAIMessage
	return json.Marshal(plain(m))
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
	Model            string               `json:"model"`
	Messages         []openAIMessage      `json:"messages"`
	Tools            []openAITool         `json:"tools,omitempty"`
	Stream           bool                 `json:"stream"`
	StreamOptions    *openAIStreamOptions `json:"stream_options,omitempty"`
	MaxTokens        int                  `json:"max_tokens,omitempty"`
	Temperature      *float64             `json:"temperature,omitempty"`
	FrequencyPenalty *float64             `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64             `json:"presence_penalty,omitempty"`
}

type openAIUsage struct {
	PromptTokens         int     `json:"prompt_tokens"`
	CompletionTokens     int     `json:"completion_tokens"`
	Cost                 float64 `json:"cost"` // OpenRouter only
	PromptTokensDetails  struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type openAIDelta struct {
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	Reasoning        string           `json:"reasoning,omitempty"`         // OpenAI o-series
	ReasoningContent string           `json:"reasoning_content,omitempty"` // OpenRouter
	ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
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

// -- encoding --

// encodeOpenAIMessages converts canonical Messages to the OpenAI wire format.
func encodeOpenAIMessages(messages []Message) []openAIMessage {
	wire := make([]openAIMessage, len(messages))
	for i, m := range messages {
		wm := openAIMessage{
			Role:       m.Role,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, openAIToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: openAIFunctionCall{
					Name:      tc.Name,
					Arguments: string(tc.Arguments),
				},
			})
		}
		// OpenAI spec: content must be null (not "") when tool_calls is present.
		if len(wm.ToolCalls) == 0 {
			wm.Content = &m.Content
		}
		wire[i] = wm
	}
	return wire
}

// encodeOpenAITools converts canonical Tools to the OpenAI wire format.
func encodeOpenAITools(tools []Tool) []openAITool {
	var wire []openAITool
	for _, t := range tools {
		wire = append(wire, openAITool{
			Type: "function",
			Function: openAIToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return wire
}

// -- stream parsing --

// parseDSMLToolCalls recovers tool calls from DeepSeek's native DSML format,
// which leaks into the content stream when OpenRouter fails to convert it.
// Format: <｜DSML｜invoke name="TOOL"><｜DSML｜parameter name="P">VALUE</｜DSML｜parameter>
func parseDSMLToolCalls(s string) []ToolCall {
	const (
		invokeOpen = "<｜DSML｜invoke name=\""
		paramOpen  = "<｜DSML｜parameter name=\""
		paramClose = "</｜DSML｜parameter>"
		anyTag     = "<｜DSML｜"
	)
	var calls []ToolCall
	for {
		idx := strings.Index(s, invokeOpen)
		if idx < 0 {
			break
		}
		s = s[idx+len(invokeOpen):]
		q := strings.IndexByte(s, '"')
		if q < 0 {
			break
		}
		toolName := s[:q]
		s = s[q:]

		params := map[string]json.RawMessage{}
		for {
			pidx := strings.Index(s, paramOpen)
			if pidx < 0 {
				break
			}
			s = s[pidx+len(paramOpen):]
			pq := strings.IndexByte(s, '"')
			if pq < 0 {
				break
			}
			paramName := s[:pq]
			s = s[pq:]
			gt := strings.IndexByte(s, '>')
			if gt < 0 {
				break
			}
			s = s[gt+1:]
			var value string
			if ci := strings.Index(s, paramClose); ci >= 0 {
				value = strings.TrimSpace(s[:ci])
				s = s[ci+len(paramClose):]
			} else if ti := strings.Index(s, anyTag); ti >= 0 {
				value = strings.TrimSpace(s[:ti])
				s = s[ti:]
			} else {
				value = strings.TrimSpace(s)
				s = ""
			}
			if value != "" {
				params[paramName] = json.RawMessage(value)
			}
		}
		if len(params) > 0 {
			if argsJSON, err := json.Marshal(params); err == nil {
				calls = append(calls, ToolCall{
					ID:        fmt.Sprintf("dsml-%s-%d", toolName, len(calls)),
					Name:      toolName,
					Arguments: argsJSON,
				})
			}
		}
		if s == "" {
			break
		}
	}
	return calls
}

// streamOpenAISSE reads an OpenAI-format SSE stream from r and sends
// StreamEvents to ch. It is the core parsing loop, separated from HTTP
// transport so it can be tested with an io.Reader directly.
func streamOpenAISSE(r io.Reader, ch chan<- StreamEvent) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	// Accumulate tool call fragments by index (OpenAI sends args incrementally).
	type tcAccum struct {
		id, name, args string
	}
	accum := make(map[int]*tcAccum)

	// finishReason and usage arrive in separate trailing chunks.
	var finishReason string
	var usage Usage

	// DeepSeek quirk: when OpenRouter fails to convert DeepSeek's native DSML
	// tool-call tokens to standard tool_calls chunks, they leak into the content
	// stream as raw text. Detect and buffer them; parse at [DONE] if no
	// structured tool calls accumulated.
	var dsmlBuf strings.Builder
	var seenDSML bool

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
			if seenDSML && len(ev.ToolCalls) == 0 {
				ev.ToolCalls = append(ev.ToolCalls, parseDSMLToolCalls(dsmlBuf.String())...)
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
			cached := wire.Usage.PromptTokensDetails.CachedTokens
			usage.CachedInputTokens = cached
			usage.InputTokens = wire.Usage.PromptTokens - cached
		}
		if wire.Usage.CompletionTokens > 0 {
			usage.OutputTokens = wire.Usage.CompletionTokens
		}
		if wire.Usage.Cost > 0 {
			usage.CostUSD = wire.Usage.Cost
		}

		if len(wire.Choices) == 0 {
			continue
		}

		choice := wire.Choices[0]
		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
		}
		if r := choice.Delta.Reasoning; r != "" {
			ch <- StreamEvent{Reasoning: r}
		} else if r := choice.Delta.ReasoningContent; r != "" {
			ch <- StreamEvent{Reasoning: r}
		}
		if content := choice.Delta.Content; content != "" {
			if seenDSML || strings.Contains(content, "<｜DSML｜") {
				seenDSML = true
				dsmlBuf.WriteString(content)
			} else {
				ch <- StreamEvent{Content: content}
			}
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
}

// -- implementation --

func (b *OpenAIBackend) ChatStream(ctx context.Context, messages []Message, tools []Tool, params GenerationParams) (<-chan StreamEvent, error) {
	wireMessages := encodeOpenAIMessages(messages)
	if b.name == "openrouter" && strings.Contains(strings.ToLower(b.model), "claude-") {
		for i := range wireMessages {
			if wireMessages[i].Role == "system" && wireMessages[i].Content != nil {
				wireMessages[i].ContentBlocks = []openAIContentBlock{{
					Type:         "text",
					Text:         *wireMessages[i].Content,
					CacheControl: &anthropicCacheCtrl{Type: "ephemeral"},
				}}
				wireMessages[i].Content = nil
			}
		}
	}
	req := openAIChatRequest{
		Model:            b.model,
		Messages:         wireMessages,
		Tools:            encodeOpenAITools(tools),
		Stream:           true,
		StreamOptions:    &openAIStreamOptions{IncludeUsage: true},
		MaxTokens:        params.MaxTokens,
		Temperature:      params.Temperature,
		FrequencyPenalty: params.FrequencyPenalty,
		PresencePenalty:  params.PresencePenalty,
	}

	data, _ := json.Marshal(req)

	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL.JoinPath("/v1/chat/completions").String(), bytes.NewReader(data))
	httpReq.Header.Set("Content-Type", "application/json")
	if b.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.apiKey)
	}
	for k, v := range b.extraHeaders {
		httpReq.Header.Set(k, v)
	}

	return streamRequest(b.client, httpReq, "openai", streamOpenAISSE)
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
