package backend_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollie/pkg/backend"
)

// stubBackend is a minimal Backend used to verify the contract.
type stubBackend struct {
	model string
}

func (s *stubBackend) Name() string                        { return "stub" }
func (s *stubBackend) DefaultModel() string                { return "stub-default" }
func (s *stubBackend) Model() string                       { return s.model }
func (s *stubBackend) SetModel(m string)                   { s.model = m }
func (s *stubBackend) ContextLength(_ context.Context) int { return 4096 }
func (s *stubBackend) Models(_ context.Context) []string   { return []string{"stub-default"} }
func (s *stubBackend) ChatStream(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
	ch := make(chan backend.StreamEvent, 1)
	ch <- backend.StreamEvent{Done: true, StopReason: "end_turn"}
	close(ch)
	return ch, nil
}

// checkContract verifies Backend invariants. Call it with any Backend implementation.
func checkContract(t *testing.T, b backend.Backend) {
	t.Helper()

	if b.Name() == "" {
		t.Error("Name() must be non-empty")
	}
	if b.DefaultModel() == "" {
		t.Error("DefaultModel() must be non-empty")
	}

	b.SetModel("test-model")
	if got := b.Model(); got != "test-model" {
		t.Errorf("Model() = %q after SetModel; want %q", got, "test-model")
	}

	msgs := []backend.Message{{Role: "user", Content: "ping"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := b.ChatStream(ctx, msgs, nil, backend.GenerationParams{})
	if err != nil {
		t.Fatalf("ChatStream returned error: %v", err)
	}
	var gotDone bool
	for ev := range ch {
		if ev.Done {
			gotDone = true
		}
	}
	if !gotDone {
		t.Error("ChatStream channel closed without a Done==true event")
	}
}

func TestStubBackendContract(t *testing.T) {
	checkContract(t, &stubBackend{model: "stub-default"})
}

func TestOllamaContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte(`{"done":true,"done_reason":"stop","message":{"role":"assistant","content":""}}` + "\n"))
	}))
	defer srv.Close()
	checkContract(t, backend.NewOllama(srv.URL))
}

func TestOpenAIContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	checkContract(t, backend.NewOpenAI("openai", srv.URL, "fake-key"))
}

func TestCopilotContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	// Copilot is an OpenAI backend; exercise it via NewOpenAI with the copilot name.
	checkContract(t, backend.NewOpenAI("copilot", srv.URL, "fake-token"))
}

func TestAnthropicContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		))
	}))
	defer srv.Close()
	b := backend.NewAnthropic("fake-key")
	b.BaseURL = srv.URL
	checkContract(t, b)
}

func TestCodeWhispererContract(t *testing.T) {
	// Static auth has no DefaultEndpoint, so ChatStream terminates immediately
	// with Done==true via the endpoint-resolution error path. This verifies that
	// the contract invariant — stream always terminates with Done — holds even
	// on error paths.
	b, err := backend.NewCodeWhisperer("fake-token")
	if err != nil {
		t.Fatalf("NewCodeWhisperer: %v", err)
	}
	checkContract(t, b)
}

func TestRateLimitError(t *testing.T) {
	e := &backend.RateLimitError{RetryAfter: 30 * time.Second, Message: "quota exceeded"}
	want := "rate limited (retry after 30s): quota exceeded"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q; want %q", got, want)
	}

	e2 := &backend.RateLimitError{Message: "over limit"}
	want2 := "rate limited: over limit"
	if got := e2.Error(); got != want2 {
		t.Errorf("Error() = %q; want %q", got, want2)
	}
}
