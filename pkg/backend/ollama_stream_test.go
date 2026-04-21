package backend

import (
	"fmt"
	"strings"
	"testing"
)

func collectOllama(ndjson string) []StreamEvent {
	ch := make(chan StreamEvent, 32)
	go func() {
		defer close(ch)
		streamOllamaNDJSON(strings.NewReader(ndjson), ch)
	}()
	var evs []StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	return evs
}

func TestOllamaNDJSON_TextStream(t *testing.T) {
	evs := collectOllama(
		"{\"message\":{\"role\":\"assistant\",\"content\":\"hello\"},\"done\":false}\n" +
			"{\"message\":{\"role\":\"assistant\",\"content\":\" world\"},\"done\":false}\n" +
			"{\"done\":true,\"done_reason\":\"stop\",\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"prompt_eval_count\":10,\"eval_count\":5}\n",
	)
	var text string
	for _, ev := range evs {
		text += ev.Content
	}
	if text != "hello world" {
		t.Errorf("text = %q", text)
	}
	fin := evs[len(evs)-1]
	if !fin.Done || fin.StopReason != "stop" {
		t.Errorf("final = %+v", fin)
	}
	if fin.Usage.InputTokens != 10 || fin.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", fin.Usage)
	}
}

func TestOllamaNDJSON_ToolCalls(t *testing.T) {
	evs := collectOllama(
		`{"done":true,"done_reason":"stop","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"f","arguments":{"k":"v"}}}]},"prompt_eval_count":1,"eval_count":1}` + "\n",
	)
	fin := evs[len(evs)-1]
	if len(fin.ToolCalls) != 1 {
		t.Fatalf("tool_calls len = %d", len(fin.ToolCalls))
	}
	if fin.ToolCalls[0].Name != "f" {
		t.Errorf("name = %q", fin.ToolCalls[0].Name)
	}
	if string(fin.ToolCalls[0].Arguments) != `{"k":"v"}` {
		t.Errorf("args = %s", fin.ToolCalls[0].Arguments)
	}
}

func TestOllamaNDJSON_DoneReasonEmpty(t *testing.T) {
	evs := collectOllama(
		"{\"done\":true,\"message\":{\"role\":\"assistant\",\"content\":\"\"}}\n",
	)
	if evs[0].StopReason != "stop" {
		t.Errorf("stop = %q; want stop (default)", evs[0].StopReason)
	}
}

func TestOllamaNDJSON_MalformedJSON(t *testing.T) {
	evs := collectOllama("{not json}\n")
	if len(evs) != 1 || !evs[0].Done {
		t.Fatal("expected Done event")
	}
	if !strings.Contains(evs[0].StopReason, "stream decode") {
		t.Errorf("stop = %q", evs[0].StopReason)
	}
}

func TestOllamaNDJSON_EmptyLines(t *testing.T) {
	evs := collectOllama(
		"\n\n" +
			"{\"message\":{\"role\":\"assistant\",\"content\":\"x\"},\"done\":false}\n" +
			"\n" +
			"{\"done\":true,\"done_reason\":\"stop\",\"message\":{\"role\":\"assistant\",\"content\":\"\"}}\n",
	)
	var text string
	for _, ev := range evs {
		text += ev.Content
	}
	if text != "x" {
		t.Errorf("text = %q", text)
	}
}

func TestOllamaNDJSON_EmptyContent(t *testing.T) {
	evs := collectOllama(
		"{\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":false}\n" +
			"{\"message\":{\"role\":\"assistant\",\"content\":\"hi\"},\"done\":false}\n" +
			"{\"done\":true,\"done_reason\":\"stop\",\"message\":{\"role\":\"assistant\",\"content\":\"\"}}\n",
	)
	// Empty content chunks should not produce events.
	var contentEvents int
	for _, ev := range evs {
		if ev.Content != "" {
			contentEvents++
		}
	}
	if contentEvents != 1 {
		t.Errorf("content events = %d; want 1", contentEvents)
	}
}

func TestOllamaNDJSON_EOFWithoutDone(t *testing.T) {
	evs := collectOllama(
		"{\"message\":{\"role\":\"assistant\",\"content\":\"partial\"},\"done\":false}\n",
	)
	// No done event — channel just closes. Content should still arrive.
	var text string
	for _, ev := range evs {
		text += ev.Content
	}
	if text != "partial" {
		t.Errorf("text = %q", text)
	}
}

func TestOllamaNDJSON_ScannerError(t *testing.T) {
	r := newErrAfterReader("{\"message\":{\"role\":\"assistant\",\"content\":\"hi\"},\"done\":false}\n", fmt.Errorf("connection reset"))
	ch := make(chan StreamEvent, 32)
	go func() {
		defer close(ch)
		streamOllamaNDJSON(r, ch)
	}()
	var evs []StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	fin := evs[len(evs)-1]
	if !fin.Done || !strings.Contains(fin.StopReason, "stream read") {
		t.Errorf("final = %+v", fin)
	}
}

// --- parseOllamaContextLength ---

func TestParseOllamaContextLength_Found(t *testing.T) {
	body := `{"model_info":{"general.context_length":8192}}`
	if cl := parseOllamaContextLength(strings.NewReader(body)); cl != 8192 {
		t.Errorf("cl = %d; want 8192", cl)
	}
}

func TestParseOllamaContextLength_DifferentPrefix(t *testing.T) {
	body := `{"model_info":{"llama.context_length":4096}}`
	if cl := parseOllamaContextLength(strings.NewReader(body)); cl != 4096 {
		t.Errorf("cl = %d; want 4096", cl)
	}
}

func TestParseOllamaContextLength_NoKey(t *testing.T) {
	body := `{"model_info":{"general.embedding_length":768}}`
	if cl := parseOllamaContextLength(strings.NewReader(body)); cl != 0 {
		t.Errorf("cl = %d; want 0", cl)
	}
}

func TestParseOllamaContextLength_EmptyModelInfo(t *testing.T) {
	body := `{"model_info":{}}`
	if cl := parseOllamaContextLength(strings.NewReader(body)); cl != 0 {
		t.Errorf("cl = %d; want 0", cl)
	}
}

func TestParseOllamaContextLength_MalformedJSON(t *testing.T) {
	if cl := parseOllamaContextLength(strings.NewReader("not json")); cl != 0 {
		t.Errorf("cl = %d; want 0", cl)
	}
}

func TestParseOllamaContextLength_ZeroValue(t *testing.T) {
	body := `{"model_info":{"general.context_length":0}}`
	if cl := parseOllamaContextLength(strings.NewReader(body)); cl != 0 {
		t.Errorf("cl = %d; want 0", cl)
	}
}

func TestParseOllamaContextLength_NonNumeric(t *testing.T) {
	body := `{"model_info":{"general.context_length":"not a number"}}`
	if cl := parseOllamaContextLength(strings.NewReader(body)); cl != 0 {
		t.Errorf("cl = %d; want 0", cl)
	}
}
