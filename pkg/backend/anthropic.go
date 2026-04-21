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

const anthropicDefaultMaxTokens = 8192

// AnthropicBackend speaks the Anthropic Messages API.
type AnthropicBackend struct {
	apiKey  string
	model   string
	client  *http.Client
	BaseURL *url.URL
}

func NewAnthropic(apiKey string) (*AnthropicBackend, error) {
	u, _ := url.Parse("https://api.anthropic.com")
	b := &AnthropicBackend{apiKey: apiKey, client: &http.Client{}, BaseURL: u}
	b.model = b.DefaultModel()
	return b, nil
}

func (b *AnthropicBackend) Name() string         { return "anthropic" }
func (b *AnthropicBackend) DefaultModel() string { return "claude-sonnet-4-5" }
func (b *AnthropicBackend) Model() string        { return b.model }
func (b *AnthropicBackend) SetModel(m string)    { b.model = m }

func (b *AnthropicBackend) ContextLength(_ context.Context) int { return 200000 }

func (b *AnthropicBackend) Models(_ context.Context) []string {
	return []string{
		"claude-sonnet-4-5",
		"claude-opus-4",
		"claude-3-5-haiku-latest",
	}
}

// -- wire types --

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Stream      bool               `json:"stream"`
	Temperature *float64           `json:"temperature,omitempty"`
}

type anthropicMessage struct {
	Role    string                 `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"` // tool_result text
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// -- implementation --

func (b *AnthropicBackend) ChatStream(ctx context.Context, messages []Message, tools []Tool, params GenerationParams) (<-chan StreamEvent, error) {
	model := b.model
	system, wireMessages := buildAnthropicMessages(messages)

	maxTokens := params.MaxTokens
	if maxTokens == 0 {
		maxTokens = anthropicDefaultMaxTokens
	}

	areq := anthropicRequest{
		Model:       model,
		MaxTokens:   maxTokens,
		System:      system,
		Messages:    wireMessages,
		Stream:      true,
		Temperature: params.Temperature,
	}
	for _, t := range tools {
		schema := t.Parameters
		if schema == nil {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		areq.Tools = append(areq.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}

	data, _ := json.Marshal(areq)

	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL.JoinPath("/v1/messages").String(), bytes.NewReader(data))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Key", b.apiKey)
	httpReq.Header.Set("Anthropic-Version", "2023-06-01")

	return streamRequest(b.client, httpReq, "anthropic", streamAnthropicSSE)
}

// buildAnthropicMessages converts ollie messages to Anthropic wire format.
// System messages are extracted into the top-level system field.
// Consecutive tool messages are batched into a single user message with
// multiple tool_result blocks (Anthropic requires strictly alternating roles).
func buildAnthropicMessages(messages []Message) (system string, out []anthropicMessage) {
	for i := 0; i < len(messages); {
		m := messages[i]
		switch m.Role {
		case "system":
			if system != "" {
				system += "\n\n"
			}
			system += m.Content
			i++
		case "user":
			out = append(out, anthropicMessage{
				Role:    "user",
				Content: []anthropicContentBlock{{Type: "text", Text: m.Content}},
			})
			i++
		case "assistant":
			msg := anthropicMessage{Role: "assistant"}
			if m.Content != "" {
				msg.Content = append(msg.Content, anthropicContentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := tc.Arguments
				if input == nil {
					input = json.RawMessage("{}")
				}
				msg.Content = append(msg.Content, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: input,
				})
			}
			out = append(out, msg)
			i++
		case "tool":
			// Batch all consecutive tool messages into one user message.
			var blocks []anthropicContentBlock
			for i < len(messages) && messages[i].Role == "tool" {
				blocks = append(blocks, anthropicContentBlock{
					Type:      "tool_result",
					ToolUseID: messages[i].ToolCallID,
					Content:   messages[i].Content,
				})
				i++
			}
			out = append(out, anthropicMessage{Role: "user", Content: blocks})
		default:
			i++
		}
	}
	return
}

// streamAnthropicSSE reads the Anthropic SSE stream and sends StreamEvents to ch.
func streamAnthropicSSE(body io.Reader, ch chan<- StreamEvent) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	type toolAccum struct {
		id, name string
		args     strings.Builder
	}
	tools := map[int]*toolAccum{}

	var inputTokens, outputTokens int
	var stopReason string
	var curEvent, curData string
	done := false

	process := func(typ, data string) {
		if done {
			return
		}
		raw := []byte(data)
		switch typ {
		case "message_start":
			var v struct {
				Message struct {
					Usage struct {
						InputTokens int `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			json.Unmarshal(raw, &v) //nolint:errcheck
			inputTokens = v.Message.Usage.InputTokens

		case "content_block_start":
			var v struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			json.Unmarshal(raw, &v) //nolint:errcheck
			if v.ContentBlock.Type == "tool_use" {
				tools[v.Index] = &toolAccum{id: v.ContentBlock.ID, name: v.ContentBlock.Name}
			}

		case "content_block_delta":
			var v struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			json.Unmarshal(raw, &v) //nolint:errcheck
			switch v.Delta.Type {
			case "text_delta":
				if v.Delta.Text != "" {
					ch <- StreamEvent{Content: v.Delta.Text}
				}
			case "input_json_delta":
				if t := tools[v.Index]; t != nil {
					t.args.WriteString(v.Delta.PartialJSON)
				}
			}

		case "message_delta":
			var v struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			json.Unmarshal(raw, &v) //nolint:errcheck
			outputTokens = v.Usage.OutputTokens
			stopReason = v.Delta.StopReason

		case "message_stop":
			ev := StreamEvent{
				Done:       true,
				StopReason: mapAnthropicStopReason(stopReason),
				Usage:      Usage{InputTokens: inputTokens, OutputTokens: outputTokens},
			}
			for _, t := range tools {
				ev.ToolCalls = append(ev.ToolCalls, ToolCall{
					ID:        t.id,
					Name:      t.name,
					Arguments: json.RawMessage(t.args.String()),
				})
			}
			ch <- ev
			done = true

		case "error":
			var v struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			json.Unmarshal(raw, &v) //nolint:errcheck
			ch <- StreamEvent{Done: true, StopReason: "error: " + v.Error.Message}
			done = true
		}
	}

	for scanner.Scan() {
		if done {
			break
		}
		line := scanner.Text()
		if line == "" {
			if curEvent != "" && curData != "" {
				process(curEvent, curData)
			}
			curEvent = ""
			curData = ""
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			curEvent = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			curData = strings.TrimPrefix(line, "data: ")
		}
	}
	// Handle any trailing event not terminated by a blank line.
	if !done && curEvent != "" && curData != "" {
		process(curEvent, curData)
	}

	if err := scanner.Err(); err != nil && !done {
		ch <- StreamEvent{Done: true, StopReason: fmt.Sprintf("stream read: %v", err)}
	}
}

func mapAnthropicStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return reason
	}
}
