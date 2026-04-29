package agent

import (
	"encoding/json"
	"testing"

	"ollie/pkg/backend"
)

// makeExecCodeMsg builds an assistant message with a single execute_code tool call.
func makeExecCodeMsg(id string, steps []map[string]any) backend.Message {
	args, _ := json.Marshal(map[string]any{"steps": steps})
	return backend.Message{
		Role: "assistant",
		ToolCalls: []backend.ToolCall{
			{ID: id, Name: "execute_code", Arguments: json.RawMessage(args)},
		},
	}
}

// makeToolResult builds a tool result message.
func makeToolResult(callID, content string) backend.Message {
	return backend.Message{Role: "tool", ToolCallID: callID, Content: content}
}

// TestPruneStaleReads_BasicWriteInvalidatesRead verifies that a file_read result
// AND its corresponding assistant tool call are removed after a subsequent
// file_write to the same path.
func TestPruneStaleReads_BasicWriteInvalidatesRead(t *testing.T) {
	history := []backend.Message{
		{Role: "user", Content: "edit foo"},
		makeExecCodeMsg("c1", []map[string]any{{"tool": "file_read", "args": []string{"/foo.go"}}}),
		makeToolResult("c1", "old content"),
		makeExecCodeMsg("c2", []map[string]any{{"tool": "file_write", "args": []string{"/foo.go", "new content"}}}),
		makeToolResult("c2", "ok"),
	}

	out := pruneStaleReads(history)

	// The file_read result (c1) and its assistant message should be gone.
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			t.Error("stale file_read result was not pruned")
		}
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID == "c1" {
					t.Error("stale tool call c1 was not pruned from assistant message")
				}
			}
		}
	}
	// user + assistant(c2) + tool(c2) = 3 messages
	if len(out) != 3 {
		t.Errorf("len(out) = %d; want 3", len(out))
	}
}

// TestPruneStaleReads_ReadAfterWriteKept verifies that a file_read occurring
// after a write is not pruned (it is fresh).
func TestPruneStaleReads_ReadAfterWriteKept(t *testing.T) {
	history := []backend.Message{
		{Role: "user", Content: "do stuff"},
		makeExecCodeMsg("c1", []map[string]any{{"tool": "file_write", "args": []string{"/foo.go", "new"}}}),
		makeToolResult("c1", "ok"),
		makeExecCodeMsg("c2", []map[string]any{{"tool": "file_read", "args": []string{"/foo.go"}}}),
		makeToolResult("c2", "new content"),
	}

	out := pruneStaleReads(history)

	if len(out) != len(history) {
		t.Errorf("len(out) = %d; want %d (fresh read after write should be kept)", len(out), len(history))
	}
}

// TestPruneStaleReads_DifferentPathNotPruned verifies that reads of a different
// path are unaffected by a write to an unrelated path.
func TestPruneStaleReads_DifferentPathNotPruned(t *testing.T) {
	history := []backend.Message{
		{Role: "user", Content: "do stuff"},
		makeExecCodeMsg("c1", []map[string]any{{"tool": "file_read", "args": []string{"/a.go"}}}),
		makeToolResult("c1", "a content"),
		makeExecCodeMsg("c2", []map[string]any{{"tool": "file_write", "args": []string{"/b.go", "new b"}}}),
		makeToolResult("c2", "ok"),
	}

	out := pruneStaleReads(history)

	if len(out) != len(history) {
		t.Errorf("read of /a.go should not be pruned by write to /b.go; len=%d want %d", len(out), len(history))
	}
}

// TestPruneStaleReads_NoWrites returns the history unchanged when there are no writes.
func TestPruneStaleReads_NoWrites(t *testing.T) {
	history := []backend.Message{
		{Role: "user", Content: "read stuff"},
		makeExecCodeMsg("c1", []map[string]any{{"tool": "file_read", "args": []string{"/foo.go"}}}),
		makeToolResult("c1", "content"),
	}

	out := pruneStaleReads(history)

	if len(out) != len(history) {
		t.Errorf("no writes: history should be unchanged; len=%d want %d", len(out), len(history))
	}
}

// TestPruneStaleReads_MultipleReadsOfSamePath prunes all stale reads of the
// same path, not just the most recent one.
func TestPruneStaleReads_MultipleReadsOfSamePath(t *testing.T) {
	history := []backend.Message{
		{Role: "user", Content: "x"},
		makeExecCodeMsg("c1", []map[string]any{{"tool": "file_read", "args": []string{"/foo.go"}}}),
		makeToolResult("c1", "v1"),
		makeExecCodeMsg("c2", []map[string]any{{"tool": "file_read", "args": []string{"/foo.go"}}}),
		makeToolResult("c2", "v1 again"),
		makeExecCodeMsg("c3", []map[string]any{{"tool": "file_write", "args": []string{"/foo.go", "v2"}}}),
		makeToolResult("c3", "ok"),
	}

	out := pruneStaleReads(history)

	for _, m := range out {
		if m.Role == "tool" && (m.ToolCallID == "c1" || m.ToolCallID == "c2") {
			t.Errorf("stale read %s was not pruned", m.ToolCallID)
		}
	}
	// c3 (write result) should still be present.
	found := false
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c3" {
			found = true
		}
	}
	if !found {
		t.Error("write result c3 should not have been pruned")
	}
}

// TestPruneStaleReads_FileEditAlsoPrunes verifies that file_edit (not just
// file_write) also marks prior reads as stale.
func TestPruneStaleReads_FileEditAlsoPrunes(t *testing.T) {
	history := []backend.Message{
		{Role: "user", Content: "edit"},
		makeExecCodeMsg("c1", []map[string]any{{"tool": "file_read", "args": []string{"/foo.go"}}}),
		makeToolResult("c1", "original"),
		makeExecCodeMsg("c2", []map[string]any{{"tool": "file_edit", "args": []string{"/foo.go", "patch"}}}),
		makeToolResult("c2", "ok"),
	}

	out := pruneStaleReads(history)

	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			t.Error("file_edit did not mark prior file_read as stale")
		}
	}
}

// TestExtractExecCodePaths_Basic covers the path extraction helper directly.
func TestExtractExecCodePaths_Basic(t *testing.T) {
	args := json.RawMessage(`{"steps":[
		{"tool":"file_read","args":["/a.go"]},
		{"tool":"file_write","args":["/b.go","content"]},
		{"tool":"file_edit","args":["/c.go","patch"]},
		{"code":"echo hi"}
	]}`)

	reads, writes := extractExecCodePaths(args)

	if len(reads) != 1 || reads[0] != "/a.go" {
		t.Errorf("reads = %v; want [/a.go]", reads)
	}
	if len(writes) != 2 {
		t.Errorf("writes = %v; want [/b.go /c.go]", writes)
	}
}

// TestPruneStaleReads_ReadWriteReadCycle is the primary correctness invariant:
// read before a write is pruned (both result and call); read after the same write is kept.
func TestPruneStaleReads_ReadWriteReadCycle(t *testing.T) {
	history := []backend.Message{
		{Role: "user", Content: "update foo"},
		makeExecCodeMsg("c1", []map[string]any{{"tool": "file_read", "args": []string{"/foo.go"}}}),
		makeToolResult("c1", "old content"),
		makeExecCodeMsg("c2", []map[string]any{{"tool": "file_write", "args": []string{"/foo.go", "new content"}}}),
		makeToolResult("c2", "ok"),
		makeExecCodeMsg("c3", []map[string]any{{"tool": "file_read", "args": []string{"/foo.go"}}}),
		makeToolResult("c3", "new content"),
	}

	out := pruneStaleReads(history)

	// c1 (pre-write read) must be gone; c3 (post-write read) must remain.
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			t.Error("pre-write read result c1 should have been pruned")
		}
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID == "c1" {
					t.Error("pre-write tool call c1 should have been pruned from assistant")
				}
			}
		}
	}
	found := false
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c3" {
			found = true
		}
	}
	if !found {
		t.Error("post-write read result c3 should be kept")
	}
	// user + assistant(c2) + tool(c2) + assistant(c3) + tool(c3) = 5 messages
	if len(out) != 5 {
		t.Errorf("len(out) = %d; want 5", len(out))
	}
}

// TestPruneStaleReads_AssistantWithTextContentKept verifies that an assistant
// message with text content is kept even when its tool calls are pruned.
func TestPruneStaleReads_AssistantWithTextContentKept(t *testing.T) {
	// Build an assistant message with both text content and a tool call.
	args, _ := json.Marshal(map[string]any{"steps": []map[string]any{{"tool": "file_read", "args": []string{"/foo.go"}}}})
	history := []backend.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "Let me read that file.", ToolCalls: []backend.ToolCall{
			{ID: "c1", Name: "execute_code", Arguments: json.RawMessage(args)},
		}},
		makeToolResult("c1", "old"),
		makeExecCodeMsg("c2", []map[string]any{{"tool": "file_write", "args": []string{"/foo.go", "new"}}}),
		makeToolResult("c2", "ok"),
	}

	out := pruneStaleReads(history)

	// The assistant message with text should be kept (with tool calls removed).
	var foundTextAssistant bool
	for _, m := range out {
		if m.Role == "assistant" && m.Content == "Let me read that file." {
			foundTextAssistant = true
			if len(m.ToolCalls) != 0 {
				t.Error("stale tool call should have been removed from assistant message")
			}
		}
	}
	if !foundTextAssistant {
		t.Error("assistant message with text content should be kept")
	}
}

// TestPruneStaleReads_NonExecuteCodeUnaffected verifies that tool calls with
// names other than "execute_code" are ignored by the path tracker and never
// cause unrelated tool results to be pruned.
func TestPruneStaleReads_NonExecuteCodeUnaffected(t *testing.T) {
	history := []backend.Message{
		{Role: "user", Content: "go"},
		// A non-execute_code call that happens to reference a path.
		{Role: "assistant", ToolCalls: []backend.ToolCall{
			{ID: "c1", Name: "some_other_tool", Arguments: json.RawMessage(`{"path":"/foo.go"}`)},
		}},
		makeToolResult("c1", "other tool result"),
		// execute_code write — should not prune the unrelated result above.
		makeExecCodeMsg("c2", []map[string]any{{"tool": "file_write", "args": []string{"/foo.go", "new"}}}),
		makeToolResult("c2", "ok"),
	}

	out := pruneStaleReads(history)

	if len(out) != len(history) {
		t.Errorf("len(out) = %d; want %d (non-execute_code results must not be pruned)", len(out), len(history))
	}
}
