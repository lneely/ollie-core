package backend

import (
	"fmt"
	"strings"
	"testing"
)

// collect runs streamOpenAISSE on raw SSE text and returns all events.
func collect(sse string) []StreamEvent {
	ch := make(chan StreamEvent, 32)
	go func() {
		defer close(ch)
		streamOpenAISSE(strings.NewReader(sse), ch)
	}()
	var evs []StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	return evs
}

func lastEvent(evs []StreamEvent) StreamEvent {
	return evs[len(evs)-1]
}

// --- basic text streaming ---

func TestSSE_TextDeltas(t *testing.T) {
	evs := collect(
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n" +
			"data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n" +
			"data: [DONE]\n\n",
	)
	var text string
	for _, ev := range evs {
		text += ev.Content
	}
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	fin := lastEvent(evs)
	if !fin.Done || fin.StopReason != "stop" {
		t.Errorf("final = %+v", fin)
	}
}

// --- usage in trailing chunk ---

func TestSSE_UsageChunk(t *testing.T) {
	evs := collect(
		"data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n" +
			"data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":42,\"completion_tokens\":7}}\n\n" +
			"data: [DONE]\n\n",
	)
	fin := lastEvent(evs)
	if fin.Usage.InputTokens != 42 || fin.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", fin.Usage)
	}
}

// --- usage inline with content ---

func TestSSE_UsageInline(t *testing.T) {
	evs := collect(
		"data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3}}\n\n" +
			"data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n" +
			"data: [DONE]\n\n",
	)
	fin := lastEvent(evs)
	if fin.Usage.InputTokens != 10 || fin.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", fin.Usage)
	}
}

// --- tool call accumulation ---

func TestSSE_ToolCallAccumulation(t *testing.T) {
	evs := collect(
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":""}}]}}]}` + "\n\n" +
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]}}]}` + "\n\n" +
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}` + "\n\n" +
			`data: {"choices":[{"finish_reason":"tool_calls","delta":{}}]}` + "\n\n" +
			"data: [DONE]\n\n",
	)
	fin := lastEvent(evs)
	if fin.StopReason != "tool_calls" {
		t.Errorf("stop = %q", fin.StopReason)
	}
	if len(fin.ToolCalls) != 1 {
		t.Fatalf("tool_calls len = %d", len(fin.ToolCalls))
	}
	tc := fin.ToolCalls[0]
	if tc.ID != "c1" || tc.Name != "f" || string(tc.Arguments) != `{"a":1}` {
		t.Errorf("tc = %+v", tc)
	}
}

// --- parallel tool calls ---

func TestSSE_ParallelToolCalls(t *testing.T) {
	evs := collect(
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"x","arguments":"{}"}}]}}]}` + "\n\n" +
			`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"y","arguments":"{}"}}]}}]}` + "\n\n" +
			`data: {"choices":[{"finish_reason":"tool_calls","delta":{}}]}` + "\n\n" +
			"data: [DONE]\n\n",
	)
	fin := lastEvent(evs)
	if len(fin.ToolCalls) != 2 {
		t.Fatalf("len = %d", len(fin.ToolCalls))
	}
	ids := map[string]bool{}
	for _, tc := range fin.ToolCalls {
		ids[tc.ID] = true
	}
	if !ids["a"] || !ids["b"] {
		t.Errorf("ids = %v", ids)
	}
}

// --- text + tool calls mixed ---

func TestSSE_TextThenToolCall(t *testing.T) {
	evs := collect(
		`data: {"choices":[{"delta":{"content":"thinking..."}}]}` + "\n\n" +
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}` + "\n\n" +
			`data: {"choices":[{"finish_reason":"tool_calls","delta":{}}]}` + "\n\n" +
			"data: [DONE]\n\n",
	)
	var text string
	for _, ev := range evs {
		text += ev.Content
	}
	if text != "thinking..." {
		t.Errorf("text = %q", text)
	}
	fin := lastEvent(evs)
	if len(fin.ToolCalls) != 1 || fin.ToolCalls[0].Name != "f" {
		t.Errorf("tool_calls = %+v", fin.ToolCalls)
	}
}

// --- malformed JSON ---

func TestSSE_MalformedJSON(t *testing.T) {
	evs := collect("data: {not json}\n\n")
	if len(evs) != 1 || !evs[0].Done {
		t.Fatal("expected single Done event")
	}
	if !strings.Contains(evs[0].StopReason, "stream decode") {
		t.Errorf("stop = %q", evs[0].StopReason)
	}
}

// --- empty choices array (usage-only chunk) ---

func TestSSE_EmptyChoicesSkipped(t *testing.T) {
	evs := collect(
		"data: {\"choices\":[]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n" +
			"data: [DONE]\n\n",
	)
	var text string
	for _, ev := range evs {
		text += ev.Content
	}
	if text != "x" {
		t.Errorf("text = %q", text)
	}
}

// --- non-data lines (comments, event: lines) are ignored ---

func TestSSE_NonDataLinesIgnored(t *testing.T) {
	evs := collect(
		": this is a comment\n" +
			"event: ping\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: [DONE]\n\n",
	)
	var text string
	for _, ev := range evs {
		text += ev.Content
	}
	if text != "ok" {
		t.Errorf("text = %q", text)
	}
}

// --- bare DONE with no prior content ---

func TestSSE_BareDone(t *testing.T) {
	evs := collect("data: [DONE]\n\n")
	if len(evs) != 1 || !evs[0].Done {
		t.Fatal("expected single Done event")
	}
	if evs[0].StopReason != "" {
		t.Errorf("stop = %q; want empty", evs[0].StopReason)
	}
}

// --- stream ends without DONE (EOF) ---

func TestSSE_EOFWithoutDone(t *testing.T) {
	evs := collect("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	// No Done event from scanner.Err (which is nil on clean EOF).
	// Channel just closes. The content event should still arrive.
	var text string
	for _, ev := range evs {
		text += ev.Content
	}
	if text != "partial" {
		t.Errorf("text = %q", text)
	}
}

// --- finish_reason "length" (max tokens) ---

func TestSSE_FinishReasonLength(t *testing.T) {
	evs := collect(
		"data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n" +
			"data: {\"choices\":[{\"finish_reason\":\"length\",\"delta\":{}}]}\n\n" +
			"data: [DONE]\n\n",
	)
	fin := lastEvent(evs)
	if fin.StopReason != "length" {
		t.Errorf("stop = %q; want length", fin.StopReason)
	}
}

// --- tool call ID/name arrive in later chunks ---

func TestSSE_ToolCallIDUpdatedLater(t *testing.T) {
	evs := collect(
		// First chunk: index only, no ID yet
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"","type":"function","function":{"name":"f","arguments":""}}]}}]}` + "\n\n" +
			// Second chunk: ID arrives
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"late_id","function":{"arguments":"{}"}}]}}]}` + "\n\n" +
			"data: [DONE]\n\n",
	)
	fin := lastEvent(evs)
	if len(fin.ToolCalls) != 1 {
		t.Fatalf("len = %d", len(fin.ToolCalls))
	}
	if fin.ToolCalls[0].ID != "late_id" {
		t.Errorf("id = %q; want late_id", fin.ToolCalls[0].ID)
	}
}

func TestSSE_ScannerError(t *testing.T) {
	// Reader that returns data then an error (not EOF).
	r := newErrAfterReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", fmt.Errorf("connection reset"))
	ch := make(chan StreamEvent, 32)
	go func() {
		defer close(ch)
		streamOpenAISSE(r, ch)
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

// --- whitespace-only lines between data lines ---

func TestSSE_WhitespaceLines(t *testing.T) {
	evs := collect(
		"  \n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
			"\t\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n" +
			"data: [DONE]\n\n",
	)
	var text string
	for _, ev := range evs {
		text += ev.Content
	}
	if text != "ab" {
		t.Errorf("text = %q", text)
	}
}
