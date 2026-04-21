package backend

import (
	"encoding/json"
	"testing"
)

// --- encodeOpenAIMessages ---

func TestEncodeMessages_UserContent(t *testing.T) {
	wire := encodeOpenAIMessages([]Message{{Role: "user", Content: "hello"}})
	if len(wire) != 1 {
		t.Fatalf("len = %d; want 1", len(wire))
	}
	if wire[0].Role != "user" || wire[0].Content == nil || *wire[0].Content != "hello" {
		t.Errorf("got %+v", wire[0])
	}
}

func TestEncodeMessages_SystemContent(t *testing.T) {
	wire := encodeOpenAIMessages([]Message{{Role: "system", Content: "be helpful"}})
	if wire[0].Role != "system" || *wire[0].Content != "be helpful" {
		t.Errorf("got %+v", wire[0])
	}
}

func TestEncodeMessages_EmptyContent(t *testing.T) {
	wire := encodeOpenAIMessages([]Message{{Role: "user", Content: ""}})
	if wire[0].Content == nil {
		t.Error("content should be non-nil pointer to empty string")
	}
	if *wire[0].Content != "" {
		t.Errorf("content = %q; want empty", *wire[0].Content)
	}
}

func TestEncodeMessages_AssistantWithToolCalls_NullContent(t *testing.T) {
	wire := encodeOpenAIMessages([]Message{{
		Role: "assistant",
		ToolCalls: []ToolCall{{
			ID: "c1", Name: "run", Arguments: json.RawMessage(`{}`),
		}},
	}})
	if wire[0].Content != nil {
		t.Error("content must be nil when tool_calls present")
	}
	if len(wire[0].ToolCalls) != 1 {
		t.Fatalf("tool_calls len = %d", len(wire[0].ToolCalls))
	}
	tc := wire[0].ToolCalls[0]
	if tc.ID != "c1" || tc.Type != "function" || tc.Function.Name != "run" || tc.Function.Arguments != "{}" {
		t.Errorf("tool_call = %+v", tc)
	}
}

func TestEncodeMessages_AssistantTextOnly_HasContent(t *testing.T) {
	wire := encodeOpenAIMessages([]Message{{Role: "assistant", Content: "sure"}})
	if wire[0].Content == nil || *wire[0].Content != "sure" {
		t.Errorf("got %+v", wire[0])
	}
	if len(wire[0].ToolCalls) != 0 {
		t.Error("tool_calls should be empty")
	}
}

func TestEncodeMessages_ToolResult(t *testing.T) {
	wire := encodeOpenAIMessages([]Message{{Role: "tool", Content: "ok", ToolCallID: "c1"}})
	if wire[0].ToolCallID != "c1" {
		t.Errorf("tool_call_id = %q", wire[0].ToolCallID)
	}
	if wire[0].Content == nil || *wire[0].Content != "ok" {
		t.Errorf("content = %v", wire[0].Content)
	}
}

func TestEncodeMessages_MultipleToolCalls(t *testing.T) {
	wire := encodeOpenAIMessages([]Message{{
		Role: "assistant",
		ToolCalls: []ToolCall{
			{ID: "a", Name: "x", Arguments: json.RawMessage(`{}`)},
			{ID: "b", Name: "y", Arguments: json.RawMessage(`{"k":"v"}`)},
		},
	}})
	if len(wire[0].ToolCalls) != 2 {
		t.Fatalf("len = %d; want 2", len(wire[0].ToolCalls))
	}
	if wire[0].ToolCalls[1].Function.Arguments != `{"k":"v"}` {
		t.Errorf("args = %q", wire[0].ToolCalls[1].Function.Arguments)
	}
}

func TestEncodeMessages_Empty(t *testing.T) {
	wire := encodeOpenAIMessages(nil)
	if len(wire) != 0 {
		t.Errorf("len = %d; want 0", len(wire))
	}
}

func TestEncodeMessages_FullConversation(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "f", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", Content: "result", ToolCallID: "c1"},
		{Role: "assistant", Content: "done"},
	}
	wire := encodeOpenAIMessages(msgs)
	if len(wire) != 5 {
		t.Fatalf("len = %d; want 5", len(wire))
	}
	// system: has content
	if wire[0].Content == nil {
		t.Error("system content nil")
	}
	// assistant with tool_calls: null content
	if wire[2].Content != nil {
		t.Error("assistant[2] content should be nil")
	}
	// tool: has content + tool_call_id
	if wire[3].ToolCallID != "c1" {
		t.Error("tool missing tool_call_id")
	}
	// final assistant: has content
	if wire[4].Content == nil || *wire[4].Content != "done" {
		t.Error("assistant[4] content wrong")
	}
}

// --- encodeOpenAITools ---

func TestEncodeTools_Empty(t *testing.T) {
	wire := encodeOpenAITools(nil)
	if wire != nil {
		t.Errorf("got %v; want nil", wire)
	}
}

func TestEncodeTools_Single(t *testing.T) {
	wire := encodeOpenAITools([]Tool{{
		Name: "f", Description: "desc", Parameters: json.RawMessage(`{"type":"object"}`),
	}})
	if len(wire) != 1 {
		t.Fatalf("len = %d", len(wire))
	}
	if wire[0].Type != "function" {
		t.Errorf("type = %q", wire[0].Type)
	}
	if wire[0].Function.Name != "f" || wire[0].Function.Description != "desc" {
		t.Errorf("function = %+v", wire[0].Function)
	}
	if string(wire[0].Function.Parameters) != `{"type":"object"}` {
		t.Errorf("parameters = %s", wire[0].Function.Parameters)
	}
}

func TestEncodeTools_Multiple(t *testing.T) {
	wire := encodeOpenAITools([]Tool{
		{Name: "a", Description: "da", Parameters: json.RawMessage(`{}`)},
		{Name: "b", Description: "db", Parameters: json.RawMessage(`{}`)},
	})
	if len(wire) != 2 {
		t.Fatalf("len = %d", len(wire))
	}
	if wire[0].Function.Name != "a" || wire[1].Function.Name != "b" {
		t.Error("names wrong")
	}
}

// --- JSON round-trip: verify wire format matches OpenAI spec ---

func TestEncodeMessages_JSONRoundTrip(t *testing.T) {
	wire := encodeOpenAIMessages([]Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "f", Arguments: json.RawMessage(`{"a":1}`)}}},
	})
	data, err := json.Marshal(wire[0])
	if err != nil {
		t.Fatal(err)
	}
	// content must be absent (null), not empty string
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	if string(raw["content"]) != "null" {
		t.Errorf("content JSON = %s; want null", raw["content"])
	}
	// tool_calls[0].type must be "function"
	var parsed openAIMessage
	json.Unmarshal(data, &parsed)
	if parsed.ToolCalls[0].Type != "function" {
		t.Errorf("type = %q", parsed.ToolCalls[0].Type)
	}
	// arguments must be a string, not an object
	if parsed.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Errorf("arguments = %q", parsed.ToolCalls[0].Function.Arguments)
	}
}

func TestEncodeRequest_ToolsOmittedWhenEmpty(t *testing.T) {
	req := openAIChatRequest{
		Model:    "m",
		Messages: encodeOpenAIMessages([]Message{{Role: "user", Content: "hi"}}),
		Tools:    encodeOpenAITools(nil),
		Stream:   true,
	}
	data, _ := json.Marshal(req)
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	if _, ok := raw["tools"]; ok {
		t.Error("tools should be omitted when nil")
	}
}
