package backend_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ollie/pkg/backend"
)

func mustNewOpenAI(t *testing.T, name, baseURL, apiKey string) *backend.OpenAIBackend {
	t.Helper()
	b, err := backend.NewOpenAI(name, baseURL, apiKey)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustNewOllama(t *testing.T, baseURL string) *backend.OllamaBackend {
	t.Helper()
	b, err := backend.NewOllama(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustNewAnthropic(t *testing.T, apiKey string) *backend.AnthropicBackend {
	t.Helper()
	b, err := backend.NewAnthropic(apiKey)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// --- stub backend ---

type stubBackend struct {
	model    string
	streamFn func(context.Context, []backend.Message, []backend.Tool, backend.GenerationParams) (<-chan backend.StreamEvent, error)
}

func (s *stubBackend) Name() string                        { return "stub" }
func (s *stubBackend) DefaultModel() string                { return "stub-default" }
func (s *stubBackend) Model() string                       { return s.model }
func (s *stubBackend) SetModel(m string)                   { s.model = m }
func (s *stubBackend) ContextLength(_ context.Context) int { return 4096 }
func (s *stubBackend) Models(_ context.Context) []string   { return []string{"stub-default"} }
func (s *stubBackend) ChatStream(ctx context.Context, msgs []backend.Message, tools []backend.Tool, params backend.GenerationParams) (<-chan backend.StreamEvent, error) {
	if s.streamFn != nil {
		return s.streamFn(ctx, msgs, tools, params)
	}
	ch := make(chan backend.StreamEvent, 1)
	ch <- backend.StreamEvent{Done: true, StopReason: "stop"}
	close(ch)
	return ch, nil
}

// --- contract checks ---

// checkContract verifies Backend invariants that hold for every implementation.
func checkContract(t *testing.T, b backend.Backend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Name / DefaultModel
	if b.Name() == "" {
		t.Error("Name() must be non-empty")
	}
	if b.DefaultModel() == "" {
		t.Error("DefaultModel() must be non-empty")
	}

	// SetModel / Model
	b.SetModel("test-model")
	if got := b.Model(); got != "test-model" {
		t.Errorf("Model() = %q after SetModel; want test-model", got)
	}

	// ContextLength
	if cl := b.ContextLength(ctx); cl < 0 {
		t.Errorf("ContextLength() = %d; must be >= 0", cl)
	}

	// Models
	_ = b.Models(ctx) // must not panic

	// ChatStream: bare message → Done
	ch, err := b.ChatStream(ctx, []backend.Message{{Role: "user", Content: "ping"}}, nil, backend.GenerationParams{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var gotDone bool
	for ev := range ch {
		if ev.Done {
			gotDone = true
		}
	}
	if !gotDone {
		t.Error("ChatStream channel closed without Done==true")
	}
}

// checkStreamText verifies that text content arrives via StreamEvent.Content
// and the final event has Done==true with a non-empty StopReason.
func checkStreamText(t *testing.T, b backend.Backend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := b.ChatStream(ctx, []backend.Message{{Role: "user", Content: "hi"}}, nil, backend.GenerationParams{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var text string
	var final backend.StreamEvent
	for ev := range ch {
		text += ev.Content
		if ev.Done {
			final = ev
		}
	}
	if text == "" {
		t.Error("expected non-empty text content")
	}
	if !final.Done {
		t.Error("no Done event")
	}
	if final.StopReason == "" {
		t.Error("StopReason should be non-empty")
	}
}

// checkStreamUsage verifies that the Done event carries usage tokens.
func checkStreamUsage(t *testing.T, b backend.Backend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := b.ChatStream(ctx, []backend.Message{{Role: "user", Content: "hi"}}, nil, backend.GenerationParams{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var final backend.StreamEvent
	for ev := range ch {
		if ev.Done {
			final = ev
		}
	}
	if final.Usage.InputTokens <= 0 {
		t.Errorf("InputTokens = %d; want > 0", final.Usage.InputTokens)
	}
	if final.Usage.OutputTokens <= 0 {
		t.Errorf("OutputTokens = %d; want > 0", final.Usage.OutputTokens)
	}
}

// checkStreamToolCalls verifies that when tools are provided and the model
// returns tool calls, they appear in the Done event's ToolCalls slice.
func checkStreamToolCalls(t *testing.T, b backend.Backend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tools := []backend.Tool{{
		Name:        "get_weather",
		Description: "Get weather",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
	}}
	ch, err := b.ChatStream(ctx, []backend.Message{{Role: "user", Content: "weather in paris"}}, tools, backend.GenerationParams{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var final backend.StreamEvent
	for ev := range ch {
		if ev.Done {
			final = ev
		}
	}
	if len(final.ToolCalls) == 0 {
		t.Fatal("expected tool calls in Done event")
	}
	tc := final.ToolCalls[0]
	if tc.Name != "get_weather" {
		t.Errorf("tool call name = %q; want get_weather", tc.Name)
	}
	if len(tc.Arguments) == 0 {
		t.Error("tool call arguments empty")
	}
}

// checkStreamToolConversation verifies a multi-turn tool use conversation:
// user → assistant(tool_call) → tool(result) → assistant(text).
func checkStreamToolConversation(t *testing.T, b backend.Backend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	msgs := []backend.Message{
		{Role: "user", Content: "weather?"},
		{Role: "assistant", ToolCalls: []backend.ToolCall{{
			ID: "call_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"paris"}`),
		}}},
		{Role: "tool", Content: "sunny 22C", ToolCallID: "call_1"},
	}
	ch, err := b.ChatStream(ctx, msgs, nil, backend.GenerationParams{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var text string
	var gotDone bool
	for ev := range ch {
		text += ev.Content
		if ev.Done {
			gotDone = true
		}
	}
	if !gotDone {
		t.Error("no Done event")
	}
	if text == "" {
		t.Error("expected text response after tool result")
	}
}

// checkErrorHTTP verifies that non-200 HTTP responses return an error (not a channel).
func checkErrorHTTP(t *testing.T, b backend.Backend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := b.ChatStream(ctx, []backend.Message{{Role: "user", Content: "hi"}}, nil, backend.GenerationParams{})
	if err == nil {
		t.Error("expected error for non-200 HTTP")
	}
}

// checkErrorRateLimit verifies that 429 returns *RateLimitError.
func checkErrorRateLimit(t *testing.T, b backend.Backend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := b.ChatStream(ctx, []backend.Message{{Role: "user", Content: "hi"}}, nil, backend.GenerationParams{})
	if err == nil {
		t.Fatal("expected error for 429")
	}
	if _, ok := err.(*backend.RateLimitError); !ok {
		t.Errorf("error type = %T; want *RateLimitError", err)
	}
}

// --- test server helpers ---

// openAITextServer returns SSE with text content, usage, and stop reason.
func openAITextServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"+
				"data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n"+
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n"+
				"data: [DONE]\n\n",
		)
	}))
}

// openAIToolServer returns SSE with a tool call.
func openAIToolServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"paris\"}"}}]}}]}`+"\n\n"+
				`data: {"choices":[{"finish_reason":"tool_calls","delta":{}}]}`+"\n\n"+
				"data: [DONE]\n\n",
		)
	}))
}

// openAIErrorServer returns the given HTTP status code.
func openAIErrorServer(code int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code == 429 {
			w.Header().Set("Retry-After", "30")
		}
		w.WriteHeader(code)
		fmt.Fprintf(w, "error %d", code)
	}))
}

// ollamaTextServer returns NDJSON with text, usage, and stop.
func ollamaTextServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w,
			"{\"message\":{\"role\":\"assistant\",\"content\":\"hello\"},\"done\":false}\n"+
				"{\"done\":true,\"done_reason\":\"stop\",\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"prompt_eval_count\":10,\"eval_count\":5}\n",
		)
	}))
}

// ollamaToolServer returns NDJSON with a tool call.
func ollamaToolServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w,
			`{"done":true,"done_reason":"stop","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"paris"}}}]},"prompt_eval_count":10,"eval_count":5}`+"\n",
		)
	}))
}

// ollamaErrorServer returns the given HTTP status code.
func ollamaErrorServer(code int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code == 429 {
			w.Header().Set("Retry-After", "30")
		}
		w.WriteHeader(code)
		fmt.Fprintf(w, "error %d", code)
	}))
}

// anthropicTextServer returns Anthropic SSE with text, usage, and stop.
func anthropicTextServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w,
			"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n"+
				"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"+
				"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		)
	}))
}

// anthropicToolServer returns Anthropic SSE with a tool call.
func anthropicToolServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w,
			"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n"+
				"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"get_weather\"}}\n\n"+
				"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"paris\\\"}\"}}\n\n"+
				"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		)
	}))
}

// anthropicErrorServer returns the given HTTP status code.
func anthropicErrorServer(code int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code == 429 {
			w.Header().Set("Retry-After", "30")
		}
		w.WriteHeader(code)
		fmt.Fprintf(w, "error %d", code)
	}))
}

// --- stub contract ---

func TestStubContract(t *testing.T) {
	checkContract(t, &stubBackend{model: "stub-default"})
}

func TestStubStreamText(t *testing.T) {
	s := &stubBackend{model: "m"}
	s.streamFn = func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		ch := make(chan backend.StreamEvent, 2)
		ch <- backend.StreamEvent{Content: "hi"}
		ch <- backend.StreamEvent{Done: true, StopReason: "stop", Usage: backend.Usage{InputTokens: 1, OutputTokens: 1}}
		close(ch)
		return ch, nil
	}
	checkStreamText(t, s)
	checkStreamUsage(t, s)
}

func TestStubStreamToolCalls(t *testing.T) {
	s := &stubBackend{model: "m"}
	s.streamFn = func(_ context.Context, _ []backend.Message, tools []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		ch := make(chan backend.StreamEvent, 1)
		ch <- backend.StreamEvent{
			Done: true, StopReason: "tool_calls",
			ToolCalls: []backend.ToolCall{{Name: tools[0].Name, Arguments: json.RawMessage(`{"city":"paris"}`)}},
		}
		close(ch)
		return ch, nil
	}
	checkStreamToolCalls(t, s)
}

// --- OpenAI ---

func TestOpenAIContract(t *testing.T) {
	srv := openAITextServer()
	defer srv.Close()
	checkContract(t, mustNewOpenAI(t, "openai", srv.URL, "k"))
}

func TestOpenAIStreamText(t *testing.T) {
	srv := openAITextServer()
	defer srv.Close()
	b := mustNewOpenAI(t, "openai", srv.URL, "k")
	checkStreamText(t, b)
	checkStreamUsage(t, b)
}

func TestOpenAIStreamToolCalls(t *testing.T) {
	srv := openAIToolServer()
	defer srv.Close()
	checkStreamToolCalls(t, mustNewOpenAI(t, "openai", srv.URL, "k"))
}

func TestOpenAIStreamToolConversation(t *testing.T) {
	srv := openAITextServer()
	defer srv.Close()
	checkStreamToolConversation(t, mustNewOpenAI(t, "openai", srv.URL, "k"))
}

func TestOpenAIErrorHTTP(t *testing.T) {
	for _, code := range []int{400, 401, 403, 500, 503} {
		t.Run(fmt.Sprintf("%d", code), func(t *testing.T) {
			srv := openAIErrorServer(code)
			defer srv.Close()
			checkErrorHTTP(t, mustNewOpenAI(t, "openai", srv.URL, "k"))
		})
	}
}

func TestOpenAIErrorRateLimit(t *testing.T) {
	srv := openAIErrorServer(429)
	defer srv.Close()
	checkErrorRateLimit(t, mustNewOpenAI(t, "openai", srv.URL, "k"))
}

// --- Copilot (OpenAI variant) ---

func TestCopilotContract(t *testing.T) {
	srv := openAITextServer()
	defer srv.Close()
	checkContract(t, mustNewOpenAI(t, "copilot", srv.URL, "k"))
}

func TestCopilotStreamText(t *testing.T) {
	srv := openAITextServer()
	defer srv.Close()
	b := mustNewOpenAI(t, "copilot", srv.URL, "k")
	checkStreamText(t, b)
	checkStreamUsage(t, b)
}

// --- Ollama ---

func TestOllamaContract(t *testing.T) {
	srv := ollamaTextServer()
	defer srv.Close()
	checkContract(t, mustNewOllama(t, srv.URL))
}

func TestOllamaStreamText(t *testing.T) {
	srv := ollamaTextServer()
	defer srv.Close()
	b := mustNewOllama(t, srv.URL)
	checkStreamText(t, b)
	checkStreamUsage(t, b)
}

func TestOllamaStreamToolCalls(t *testing.T) {
	srv := ollamaToolServer()
	defer srv.Close()
	checkStreamToolCalls(t, mustNewOllama(t, srv.URL))
}

func TestOllamaStreamToolConversation(t *testing.T) {
	srv := ollamaTextServer()
	defer srv.Close()
	checkStreamToolConversation(t, mustNewOllama(t, srv.URL))
}

func TestOllamaErrorHTTP(t *testing.T) {
	for _, code := range []int{400, 500, 503} {
		t.Run(fmt.Sprintf("%d", code), func(t *testing.T) {
			srv := ollamaErrorServer(code)
			defer srv.Close()
			checkErrorHTTP(t, mustNewOllama(t, srv.URL))
		})
	}
}

func TestOllamaErrorRateLimit(t *testing.T) {
	srv := ollamaErrorServer(429)
	defer srv.Close()
	checkErrorRateLimit(t, mustNewOllama(t, srv.URL))
}

// --- Anthropic ---

func TestAnthropicContract(t *testing.T) {
	srv := anthropicTextServer()
	defer srv.Close()
	b := mustNewAnthropic(t, "k")
	b.BaseURL, _ = url.Parse(srv.URL)
	checkContract(t, b)
}

func TestAnthropicStreamText(t *testing.T) {
	srv := anthropicTextServer()
	defer srv.Close()
	b := mustNewAnthropic(t, "k")
	b.BaseURL, _ = url.Parse(srv.URL)
	checkStreamText(t, b)
	checkStreamUsage(t, b)
}

func TestAnthropicStreamToolCalls(t *testing.T) {
	srv := anthropicToolServer()
	defer srv.Close()
	b := mustNewAnthropic(t, "k")
	b.BaseURL, _ = url.Parse(srv.URL)
	checkStreamToolCalls(t, b)
}

func TestAnthropicStreamToolConversation(t *testing.T) {
	srv := anthropicTextServer()
	defer srv.Close()
	b := mustNewAnthropic(t, "k")
	b.BaseURL, _ = url.Parse(srv.URL)
	checkStreamToolConversation(t, b)
}

func TestAnthropicErrorHTTP(t *testing.T) {
	for _, code := range []int{400, 401, 500, 503} {
		t.Run(fmt.Sprintf("%d", code), func(t *testing.T) {
			srv := anthropicErrorServer(code)
			defer srv.Close()
			b := mustNewAnthropic(t, "k")
			b.BaseURL, _ = url.Parse(srv.URL)
			checkErrorHTTP(t, b)
		})
	}
}

func TestAnthropicErrorRateLimit(t *testing.T) {
	srv := anthropicErrorServer(429)
	defer srv.Close()
	b := mustNewAnthropic(t, "k")
	b.BaseURL, _ = url.Parse(srv.URL)
	checkErrorRateLimit(t, b)
}

// --- Ollama: Models and ContextLength HTTP paths ---

func ollamaModelsServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": "qwen:7b"}, {"name": "llama3:8b"}},
			})
		case "/api/show":
			json.NewEncoder(w).Encode(map[string]any{
				"model_info": map[string]any{"general.context_length": 8192.0},
			})
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestOllamaModels_Success(t *testing.T) {
	srv := ollamaModelsServer()
	defer srv.Close()
	b := mustNewOllama(t, srv.URL)
	models := b.Models(context.Background())
	if len(models) != 2 || models[0] != "qwen:7b" {
		t.Errorf("models = %v", models)
	}
}

func TestOllamaModels_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	b := mustNewOllama(t, srv.URL)
	if models := b.Models(context.Background()); models != nil {
		t.Errorf("models = %v; want nil", models)
	}
}

func TestOllamaModels_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json")
	}))
	defer srv.Close()
	b := mustNewOllama(t, srv.URL)
	if models := b.Models(context.Background()); models != nil {
		t.Errorf("models = %v; want nil", models)
	}
}

func TestOllamaModels_ConnectionRefused(t *testing.T) {
	b := mustNewOllama(t, "http://127.0.0.1:1")
	if models := b.Models(context.Background()); models != nil {
		t.Errorf("models = %v; want nil", models)
	}
}

func TestOllamaContextLength_Success(t *testing.T) {
	srv := ollamaModelsServer()
	defer srv.Close()
	b := mustNewOllama(t, srv.URL)
	if cl := b.ContextLength(context.Background()); cl != 8192 {
		t.Errorf("cl = %d; want 8192", cl)
	}
}

func TestOllamaContextLength_Cached(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{
			"model_info": map[string]any{"general.context_length": 4096.0},
		})
	}))
	defer srv.Close()
	b := mustNewOllama(t, srv.URL)
	b.ContextLength(context.Background())
	b.ContextLength(context.Background())
	if calls != 1 {
		t.Errorf("calls = %d; want 1", calls)
	}
}

func TestOllamaContextLength_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	b := mustNewOllama(t, srv.URL)
	if cl := b.ContextLength(context.Background()); cl != 0 {
		t.Errorf("cl = %d; want 0", cl)
	}
}

func TestOllamaContextLength_ConnectionRefused(t *testing.T) {
	b := mustNewOllama(t, "http://127.0.0.1:1")
	if cl := b.ContextLength(context.Background()); cl != 0 {
		t.Errorf("cl = %d; want 0", cl)
	}
}

// --- CodeWhisperer ---
// Intentionally untested. Reverse-engineered Kiro/CodeWhisperer protocol
// requires a live session. See codewhisperer.go.

// --- RateLimitError ---

func TestRateLimitError(t *testing.T) {
	tests := []struct {
		err  *backend.RateLimitError
		want string
	}{
		{&backend.RateLimitError{RetryAfter: 30 * time.Second, Message: "quota exceeded"}, "rate limited (retry after 30s): quota exceeded"},
		{&backend.RateLimitError{Message: "over limit"}, "rate limited: over limit"},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("Error() = %q; want %q", got, tt.want)
		}
	}
}

// --- Anthropic: wire format edge cases ---

func TestAnthropicNilToolParameters(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w,
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		)
	}))
	defer srv.Close()

	b := mustNewAnthropic(t, "k")
	b.BaseURL, _ = url.Parse(srv.URL)
	ch, err := b.ChatStream(context.Background(),
		[]backend.Message{{Role: "user", Content: "hi"}},
		[]backend.Tool{{Name: "f", Description: "d", Parameters: nil}},
		backend.GenerationParams{},
	)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	// nil Parameters should become {"type":"object","properties":{}}
	if !strings.Contains(string(gotBody), `"input_schema":{"type":"object","properties":{}}`) {
		t.Errorf("body = %s", gotBody)
	}
}

func TestAnthropicMaxTokensDefault(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w,
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		)
	}))
	defer srv.Close()

	b := mustNewAnthropic(t, "k")
	b.BaseURL, _ = url.Parse(srv.URL)
	ch, err := b.ChatStream(context.Background(),
		[]backend.Message{{Role: "user", Content: "hi"}},
		nil,
		backend.GenerationParams{}, // MaxTokens == 0
	)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if !strings.Contains(string(gotBody), `"max_tokens":8192`) {
		t.Errorf("body = %s; want max_tokens:8192", gotBody)
	}
}

func TestAnthropicMaxTokensExplicit(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w,
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		)
	}))
	defer srv.Close()

	b := mustNewAnthropic(t, "k")
	b.BaseURL, _ = url.Parse(srv.URL)
	ch, err := b.ChatStream(context.Background(),
		[]backend.Message{{Role: "user", Content: "hi"}},
		nil,
		backend.GenerationParams{MaxTokens: 256},
	)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if !strings.Contains(string(gotBody), `"max_tokens":256`) {
		t.Errorf("body = %s; want max_tokens:256", gotBody)
	}
}

// --- error path: SSE stream error event (Anthropic) ---

func TestAnthropicStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: error\ndata: {\"error\":{\"message\":\"overloaded\"}}\n\n")
	}))
	defer srv.Close()

	b := mustNewAnthropic(t, "k")
	b.BaseURL, _ = url.Parse(srv.URL)
	ch, err := b.ChatStream(context.Background(), []backend.Message{{Role: "user", Content: "hi"}}, nil, backend.GenerationParams{})
	if err != nil {
		t.Fatal(err)
	}
	var final backend.StreamEvent
	for ev := range ch {
		if ev.Done {
			final = ev
		}
	}
	if !final.Done {
		t.Error("expected Done")
	}
	if !strings.Contains(final.StopReason, "overloaded") {
		t.Errorf("StopReason = %q; want to contain 'overloaded'", final.StopReason)
	}
}
