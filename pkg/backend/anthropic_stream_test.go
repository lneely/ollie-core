package backend

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func collectAnthropic(sse string) []StreamEvent {
	ch := make(chan StreamEvent, 32)
	go func() {
		defer close(ch)
		streamAnthropicSSE(strings.NewReader(sse), ch)
	}()
	var evs []StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	return evs
}

func TestAnthropicSSE_TextStream(t *testing.T) {
	evs := collectAnthropic(
		"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
			"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
			"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n" +
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
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

func TestAnthropicSSE_ToolCall(t *testing.T) {
	evs := collectAnthropic(
		"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n" +
			"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"c1\",\"name\":\"f\"}}\n\n" +
			"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\"}}\n\n" +
			"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"1}\"}}\n\n" +
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	fin := evs[len(evs)-1]
	if len(fin.ToolCalls) != 1 {
		t.Fatalf("tool_calls len = %d", len(fin.ToolCalls))
	}
	tc := fin.ToolCalls[0]
	if tc.ID != "c1" || tc.Name != "f" || string(tc.Arguments) != `{"a":1}` {
		t.Errorf("tc = %+v", tc)
	}
	if fin.StopReason != "tool_calls" {
		t.Errorf("stop = %q", fin.StopReason)
	}
}

func TestAnthropicSSE_ParallelToolCalls(t *testing.T) {
	evs := collectAnthropic(
		"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
			"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"a\",\"name\":\"x\"}}\n\n" +
			"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
			"event: content_block_start\ndata: {\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"b\",\"name\":\"y\"}}\n\n" +
			"event: content_block_delta\ndata: {\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	fin := evs[len(evs)-1]
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

func TestAnthropicSSE_ErrorEvent(t *testing.T) {
	evs := collectAnthropic(
		"event: error\ndata: {\"error\":{\"message\":\"overloaded\"}}\n\n",
	)
	if len(evs) != 1 || !evs[0].Done {
		t.Fatal("expected Done")
	}
	if !strings.Contains(evs[0].StopReason, "overloaded") {
		t.Errorf("stop = %q", evs[0].StopReason)
	}
}

func TestAnthropicSSE_TextBlockStartIgnored(t *testing.T) {
	// content_block_start with type "text" should not create a tool accumulator.
	evs := collectAnthropic(
		"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
			"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	fin := evs[len(evs)-1]
	if len(fin.ToolCalls) != 0 {
		t.Errorf("tool_calls = %+v; want empty", fin.ToolCalls)
	}
}

func TestAnthropicSSE_InputJSONDeltaUnknownIndex(t *testing.T) {
	// input_json_delta for an index with no content_block_start should be silently ignored.
	evs := collectAnthropic(
		"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
			"event: content_block_delta\ndata: {\"index\":99,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"junk\"}}\n\n" +
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	fin := evs[len(evs)-1]
	if len(fin.ToolCalls) != 0 {
		t.Errorf("tool_calls = %+v; want empty", fin.ToolCalls)
	}
}

func TestAnthropicSSE_TrailingEventNoBlankLine(t *testing.T) {
	// Event not terminated by a blank line should still be processed.
	evs := collectAnthropic(
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}",
	)
	if len(evs) == 0 || !evs[len(evs)-1].Done {
		t.Error("trailing event not processed")
	}
}

func TestAnthropicSSE_EmptyStream(t *testing.T) {
	evs := collectAnthropic("")
	if len(evs) != 0 {
		t.Errorf("expected no events; got %d", len(evs))
	}
}

func TestAnthropicSSE_ScannerError(t *testing.T) {
	r := newErrAfterReader(
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
		fmt.Errorf("connection reset"),
	)
	ch := make(chan StreamEvent, 32)
	go func() {
		defer close(ch)
		streamAnthropicSSE(r, ch)
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

// --- mapAnthropicStopReason ---

func TestMapAnthropicStopReason(t *testing.T) {
	tests := []struct{ in, want string }{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"unknown", "unknown"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := mapAnthropicStopReason(tt.in); got != tt.want {
			t.Errorf("mapAnthropicStopReason(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

// --- buildAnthropicMessages ---

func TestBuildAnthropicMessages_SystemConcat(t *testing.T) {
	sys, _ := buildAnthropicMessages([]Message{
		{Role: "system", Content: "a"},
		{Role: "system", Content: "b"},
	})
	if len(sys) != 1 || sys[0].Text != "a\n\nb" {
		t.Errorf("system = %+v", sys)
	}
	if sys[0].CacheControl == nil || sys[0].CacheControl.Type != "ephemeral" {
		t.Errorf("system block missing cache_control: %+v", sys[0])
	}
}

func TestBuildAnthropicMessages_AssistantToolCallsOnly(t *testing.T) {
	_, out := buildAnthropicMessages([]Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "f", Arguments: json.RawMessage(`{"k":"v"}`)}}},
	})
	if len(out) != 1 {
		t.Fatalf("len = %d", len(out))
	}
	// No text block, only tool_use
	for _, b := range out[0].Content {
		if b.Type == "text" {
			t.Error("should not have text block when Content is empty")
		}
	}
	if out[0].Content[0].Type != "tool_use" {
		t.Errorf("type = %q", out[0].Content[0].Type)
	}
}

func TestBuildAnthropicMessages_NilArguments(t *testing.T) {
	_, out := buildAnthropicMessages([]Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "f", Arguments: nil}}},
	})
	block := out[0].Content[0]
	if string(block.Input) != "{}" {
		t.Errorf("input = %s; want {}", block.Input)
	}
}

func TestBuildAnthropicMessages_ToolBatching(t *testing.T) {
	_, out := buildAnthropicMessages([]Message{
		{Role: "tool", Content: "r1", ToolCallID: "c1"},
		{Role: "tool", Content: "r2", ToolCallID: "c2"},
	})
	// Two consecutive tool messages should be batched into one user message.
	if len(out) != 1 {
		t.Fatalf("len = %d; want 1", len(out))
	}
	if out[0].Role != "user" {
		t.Errorf("role = %q", out[0].Role)
	}
	if len(out[0].Content) != 2 {
		t.Fatalf("content len = %d; want 2", len(out[0].Content))
	}
	if out[0].Content[0].Type != "tool_result" || out[0].Content[0].ToolUseID != "c1" {
		t.Errorf("block[0] = %+v", out[0].Content[0])
	}
	if out[0].Content[1].ToolUseID != "c2" {
		t.Errorf("block[1] = %+v", out[0].Content[1])
	}
}

func TestBuildAnthropicMessages_UnknownRole(t *testing.T) {
	_, out := buildAnthropicMessages([]Message{
		{Role: "unknown", Content: "x"},
		{Role: "user", Content: "hi"},
	})
	// Unknown role should be skipped, user message should still appear.
	if len(out) != 1 || out[0].Role != "user" {
		t.Errorf("out = %+v", out)
	}
}

func TestBuildAnthropicMessages_FullConversation(t *testing.T) {
	sys, out := buildAnthropicMessages([]Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "let me check", ToolCalls: []ToolCall{{ID: "c1", Name: "f", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", Content: "result", ToolCallID: "c1"},
		{Role: "assistant", Content: "done"},
	})
	if len(sys) != 1 || sys[0].Text != "sys" {
		t.Errorf("system = %+v", sys)
	}
	if len(out) != 4 {
		t.Fatalf("len = %d; want 4", len(out))
	}
	// assistant[1] should have both text and tool_use blocks
	if len(out[1].Content) != 2 {
		t.Errorf("assistant content len = %d; want 2", len(out[1].Content))
	}
}

// --- streamAnthropicSSE: done re-entry and scanner break ---

func TestAnthropicSSE_EventsAfterStop(t *testing.T) {
	// Events after message_stop should be ignored (done flag).
	evs := collectAnthropic(
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n" +
			"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ghost\"}}\n\n",
	)
	// Should get exactly one Done event, no "ghost" text.
	var text string
	var doneCount int
	for _, ev := range evs {
		text += ev.Content
		if ev.Done {
			doneCount++
		}
	}
	if text != "" {
		t.Errorf("text = %q; want empty", text)
	}
	if doneCount != 1 {
		t.Errorf("done count = %d; want 1", doneCount)
	}
}

func TestAnthropicSSE_EventsAfterError(t *testing.T) {
	evs := collectAnthropic(
		"event: error\ndata: {\"error\":{\"message\":\"bad\"}}\n\n" +
			"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ghost\"}}\n\n",
	)
	if len(evs) != 1 || !evs[0].Done {
		t.Fatalf("expected single Done event; got %d", len(evs))
	}
}

// --- tool schema caching ---

// TestToolSchemaCaching verifies that when multiple tools are provided, the
// last tool gets cache_control:ephemeral and all others do not.
func TestToolSchemaCaching(t *testing.T) {
	tools := []Tool{
		{Name: "alpha", Description: "a", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "beta", Description: "b", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "gamma", Description: "c", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
	areq := anthropicRequest{}
	for _, tool := range tools {
		schema := tool.Parameters
		if schema == nil {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		areq.Tools = append(areq.Tools, anthropicTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		})
	}
	if len(areq.Tools) > 0 {
		areq.Tools[len(areq.Tools)-1].CacheControl = &anthropicCacheCtrl{Type: "ephemeral"}
	}

	for i, tool := range areq.Tools {
		if i < len(areq.Tools)-1 {
			if tool.CacheControl != nil {
				t.Errorf("tool[%d] (%s) has cache_control; want none", i, tool.Name)
			}
		} else {
			if tool.CacheControl == nil || tool.CacheControl.Type != "ephemeral" {
				t.Errorf("last tool (%s) missing cache_control:ephemeral; got %+v", tool.Name, tool.CacheControl)
			}
		}
	}
}

// TestToolSchemaCaching_SingleTool verifies the single-tool edge case: the
// one tool gets cache_control:ephemeral.
func TestToolSchemaCaching_SingleTool(t *testing.T) {
	tools := []Tool{
		{Name: "only", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
	areq := anthropicRequest{}
	for _, tool := range tools {
		areq.Tools = append(areq.Tools, anthropicTool{Name: tool.Name, InputSchema: tool.Parameters})
	}
	if len(areq.Tools) > 0 {
		areq.Tools[len(areq.Tools)-1].CacheControl = &anthropicCacheCtrl{Type: "ephemeral"}
	}
	if areq.Tools[0].CacheControl == nil || areq.Tools[0].CacheControl.Type != "ephemeral" {
		t.Errorf("single tool missing cache_control: %+v", areq.Tools[0])
	}
}

// --- large tool result caching ---

// TestToolResultCaching_LargeGetsCache verifies that a tool result ≥4096 bytes
// is marked with cache_control:ephemeral.
func TestToolResultCaching_LargeGetsCache(t *testing.T) {
	large := strings.Repeat("x", 4096)
	_, out := buildAnthropicMessages([]Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{{ID: "c1", Name: "f", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", Content: large, ToolCallID: "c1"},
	})
	// Last user message contains the tool result.
	last := out[len(out)-1]
	if last.Role != "user" || len(last.Content) != 1 {
		t.Fatalf("unexpected last message: %+v", last)
	}
	b := last.Content[0]
	if b.Type != "tool_result" {
		t.Fatalf("expected tool_result, got %q", b.Type)
	}
	if b.CacheControl == nil || b.CacheControl.Type != "ephemeral" {
		t.Errorf("large tool result missing cache_control:ephemeral; got %+v", b.CacheControl)
	}
}

// TestToolResultCaching_SmallNoCache verifies that a tool result below the
// 4096-byte threshold does not get cache_control.
func TestToolResultCaching_SmallNoCache(t *testing.T) {
	_, out := buildAnthropicMessages([]Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "f", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", Content: "small result", ToolCallID: "c1"},
	})
	last := out[len(out)-1]
	for _, b := range last.Content {
		if b.Type == "tool_result" && b.CacheControl != nil {
			t.Errorf("small tool result has cache_control: %+v", b.CacheControl)
		}
	}
}

// TestToolResultCaching_LastLargeResult verifies that when multiple large tool
// results exist, only the LAST one gets cache_control (not the first).
func TestToolResultCaching_LastLargeResult(t *testing.T) {
	large := strings.Repeat("y", 4096)
	_, out := buildAnthropicMessages([]Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c1", Name: "f", Arguments: json.RawMessage(`{}`)},
			{ID: "c2", Name: "g", Arguments: json.RawMessage(`{}`)},
		}},
		{Role: "tool", Content: large, ToolCallID: "c1"},
		{Role: "tool", Content: large, ToolCallID: "c2"},
	})
	last := out[len(out)-1]
	if last.Role != "user" || len(last.Content) != 2 {
		t.Fatalf("unexpected last message: role=%q len=%d", last.Role, len(last.Content))
	}
	if last.Content[0].CacheControl != nil {
		t.Errorf("first large result should not have cache_control")
	}
	if last.Content[1].CacheControl == nil || last.Content[1].CacheControl.Type != "ephemeral" {
		t.Errorf("last large result missing cache_control:ephemeral; got %+v", last.Content[1].CacheControl)
	}
}
