package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- HTTP integration: request encoding ---

func TestChatStream_RequestWireFormat(t *testing.T) {
	var got openAIChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %s; want POST", r.Method)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s; want /v1/chat/completions", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("Authorization = %q", auth)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	temp := 0.7
	b := mustOpenAI("openai", srv.URL, "test-key")
	b.SetModel("gpt-4")

	ch, err := b.ChatStream(context.Background(),
		[]Message{
			{Role: "system", Content: "you are helpful"},
			{Role: "user", Content: "hello"},
		},
		[]Tool{{Name: "get_weather", Description: "Get weather", Parameters: json.RawMessage(`{"type":"object"}`)}},
		GenerationParams{MaxTokens: 100, Temperature: &temp},
	)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	if got.Model != "gpt-4" {
		t.Errorf("model = %q", got.Model)
	}
	if !got.Stream {
		t.Error("stream should be true")
	}
	if got.StreamOptions == nil || !got.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage should be true")
	}
	if got.MaxTokens != 100 {
		t.Errorf("max_tokens = %d", got.MaxTokens)
	}
	if got.Temperature == nil || *got.Temperature != 0.7 {
		t.Errorf("temperature = %v", got.Temperature)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages len = %d", len(got.Messages))
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tools = %+v", got.Tools)
	}
}

func TestChatStream_NoAuthWhenKeyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("Authorization = %q; want empty", auth)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	b := mustOpenAI("openai", srv.URL, "")
	ch, _ := b.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, GenerationParams{})
	for range ch {
	}
}

func TestChatStream_ExtraHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("X-Custom"); v != "val" {
			t.Errorf("X-Custom = %q; want val", v)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	b := mustOpenAI("openai", srv.URL, "k")
	b.extraHeaders = map[string]string{"X-Custom": "val"}
	ch, _ := b.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, GenerationParams{})
	for range ch {
	}
}

func TestChatStream_OmitsZeroParams(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	b := mustOpenAI("openai", srv.URL, "k")
	ch, _ := b.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, GenerationParams{})
	for range ch {
	}

	s := string(raw)
	for _, field := range []string{"max_tokens", "temperature", "frequency_penalty", "presence_penalty"} {
		if strings.Contains(s, field) {
			t.Errorf("zero-value %s should be omitted", field)
		}
	}
}

func TestChatStream_AllParamsPresent(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	temp, fp, pp := 0.5, 0.3, 0.1
	b := mustOpenAI("openai", srv.URL, "k")
	ch, _ := b.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
		GenerationParams{MaxTokens: 200, Temperature: &temp, FrequencyPenalty: &fp, PresencePenalty: &pp})
	for range ch {
	}

	s := string(raw)
	for _, field := range []string{"max_tokens", "temperature", "frequency_penalty", "presence_penalty"} {
		if !strings.Contains(s, field) {
			t.Errorf("expected %s in request body", field)
		}
	}
}

// --- HTTP error handling ---

func TestChatStream_RateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "quota exceeded")
	}))
	defer srv.Close()

	b := mustOpenAI("openai", srv.URL, "k")
	_, err := b.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, GenerationParams{})
	rle, ok := err.(*RateLimitError)
	if !ok {
		t.Fatalf("type = %T; want *RateLimitError", err)
	}
	if rle.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v", rle.RetryAfter)
	}
	if !strings.Contains(rle.Message, "quota exceeded") {
		t.Errorf("Message = %q", rle.Message)
	}
}

func TestChatStream_RateLimitNoRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "slow down")
	}))
	defer srv.Close()

	b := mustOpenAI("openai", srv.URL, "k")
	_, err := b.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, GenerationParams{})
	rle, ok := err.(*RateLimitError)
	if !ok {
		t.Fatalf("type = %T", err)
	}
	if rle.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v; want 0", rle.RetryAfter)
	}
}

func TestChatStream_HTTPError(t *testing.T) {
	for _, code := range []int{400, 401, 403, 500, 502, 503} {
		t.Run(fmt.Sprintf("HTTP_%d", code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				fmt.Fprintf(w, "error %d", code)
			}))
			defer srv.Close()

			b := mustOpenAI("openai", srv.URL, "k")
			_, err := b.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, GenerationParams{})
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%d", code)) {
				t.Errorf("error = %q; want to contain %d", err.Error(), code)
			}
		})
	}
}

func TestChatStream_ConnectionRefused(t *testing.T) {
	b := mustOpenAI("openai", "http://127.0.0.1:1", "k")
	_, err := b.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, GenerationParams{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestChatStream_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block forever; context cancellation should unblock the client.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	b := mustOpenAI("openai", srv.URL, "k")
	_, err := b.ChatStream(ctx, []Message{{Role: "user", Content: "hi"}}, nil, GenerationParams{})
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- /v1/models endpoint ---

func TestModels_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Method != "GET" {
			t.Errorf("method = %s; want GET", r.Method)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer k" {
			t.Errorf("Authorization = %q", auth)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "gpt-4", "context_length": 8192},
				{"id": "gpt-3.5-turbo", "context_length": 4096},
			},
		})
	}))
	defer srv.Close()

	b := mustOpenAI("openai", srv.URL, "k")
	models := b.Models(context.Background())
	if len(models) != 2 || models[0] != "gpt-4" {
		t.Errorf("models = %v", models)
	}
}

func TestModels_Cached(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "m1"}},
		})
	}))
	defer srv.Close()

	b := mustOpenAI("openai", srv.URL, "k")
	b.Models(context.Background())
	b.Models(context.Background())
	if calls != 1 {
		t.Errorf("API calls = %d; want 1", calls)
	}
}

func TestModels_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	b := mustOpenAI("openai", srv.URL, "k")
	if models := b.Models(context.Background()); len(models) != 0 {
		t.Errorf("models = %v; want empty", models)
	}
}

func TestModels_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json")
	}))
	defer srv.Close()

	b := mustOpenAI("openai", srv.URL, "k")
	if models := b.Models(context.Background()); len(models) != 0 {
		t.Errorf("models = %v; want empty", models)
	}
}

func TestModels_NoAuthWhenKeyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("Authorization = %q; want empty", auth)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
	}))
	defer srv.Close()

	b := mustOpenAI("openai", srv.URL, "")
	b.Models(context.Background())
}

// --- ContextLength ---

func TestContextLength_Lookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "gpt-4", "context_length": 8192}},
		})
	}))
	defer srv.Close()

	b := mustOpenAI("openai", srv.URL, "k")
	b.SetModel("gpt-4")
	if cl := b.ContextLength(context.Background()); cl != 8192 {
		t.Errorf("context_length = %d", cl)
	}
}

func TestContextLength_Cached(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "gpt-4", "context_length": 8192}},
		})
	}))
	defer srv.Close()

	b := mustOpenAI("openai", srv.URL, "k")
	b.SetModel("gpt-4")
	b.ContextLength(context.Background())
	b.ContextLength(context.Background())
	if calls != 1 {
		t.Errorf("API calls = %d; want 1", calls)
	}
}

func TestContextLength_InvalidatedBySetModel(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "a", "context_length": 100},
				{"id": "b", "context_length": 200},
			},
		})
	}))
	defer srv.Close()

	b := mustOpenAI("openai", srv.URL, "k")
	b.SetModel("a")
	if cl := b.ContextLength(context.Background()); cl != 100 {
		t.Errorf("cl = %d; want 100", cl)
	}
	b.SetModel("b")
	if cl := b.ContextLength(context.Background()); cl != 200 {
		t.Errorf("cl = %d; want 200", cl)
	}
}

func TestContextLength_UnknownModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
	}))
	defer srv.Close()

	b := mustOpenAI("openai", srv.URL, "k")
	b.SetModel("nonexistent")
	if cl := b.ContextLength(context.Background()); cl != 0 {
		t.Errorf("cl = %d; want 0", cl)
	}
}

// --- constructor / defaults ---

func TestNewOpenAI_DefaultBaseURL(t *testing.T) {
	b := mustOpenAI("openai", "", "k")
	if b.baseURL.String() != "https://api.openai.com" {
		t.Errorf("baseURL = %q", b.baseURL)
	}
}

func TestNewOpenAI_DefaultModel(t *testing.T) {
	b := mustOpenAI("openai", "http://x", "k")
	if b.Model() != b.DefaultModel() {
		t.Errorf("model = %q; want %q", b.Model(), b.DefaultModel())
	}
}

func TestDefaultModel(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"openrouter", "deepseek/deepseek-v3.2"},
		{"anthropic", "claude-sonnet-4-5"},
		{"openai", "qwen3.5:9b"},
		{"unknown", "qwen3.5:9b"},
	}
	for _, tt := range tests {
		if got := (&OpenAIBackend{name: tt.name}).DefaultModel(); got != tt.want {
			t.Errorf("DefaultModel(%q) = %q; want %q", tt.name, got, tt.want)
		}
	}
}

func TestSetModel_ClearsCache(t *testing.T) {
	b := &OpenAIBackend{ctxLength: 100, ctxModel: "old"}
	b.SetModel("new")
	if b.ctxLength != 0 {
		t.Error("SetModel should clear ctxLength")
	}
	if b.model != "new" {
		t.Errorf("model = %q", b.model)
	}
}

func TestName(t *testing.T) {
	b := mustOpenAI("myname", "http://x", "k")
	if b.Name() != "myname" {
		t.Errorf("Name() = %q", b.Name())
	}
}

// --- parseRetryAfter ---

func TestParseRetryAfter_Empty(t *testing.T) {
	if d := parseRetryAfter(""); d != 0 {
		t.Errorf("got %v", d)
	}
}

func TestParseRetryAfter_Seconds(t *testing.T) {
	if d := parseRetryAfter("120"); d != 120*time.Second {
		t.Errorf("got %v", d)
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	future := time.Now().Add(60 * time.Second).UTC().Format(http.TimeFormat)
	d := parseRetryAfter(future)
	if d < 50*time.Second || d > 70*time.Second {
		t.Errorf("got %v; want ~60s", d)
	}
}

func TestParseRetryAfter_PastDate(t *testing.T) {
	past := time.Now().Add(-60 * time.Second).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(past); d != 0 {
		t.Errorf("got %v; want 0 for past date", d)
	}
}

func TestParseRetryAfter_Invalid(t *testing.T) {
	if d := parseRetryAfter("not-a-number"); d != 0 {
		t.Errorf("got %v", d)
	}
}

func TestParseRetryAfter_Whitespace(t *testing.T) {
	if d := parseRetryAfter("  60  "); d != 60*time.Second {
		t.Errorf("got %v", d)
	}
}
