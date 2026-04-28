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
// is removed from history after a subsequent file_write to the same path.
func TestPruneStaleReads_BasicWriteInvalidatesRead(t *testing.T) {
	history := []backend.Message{
		{Role: "user", Content: "edit foo"},
		makeExecCodeMsg("c1", []map[string]any{{"tool": "file_read", "args": []string{"/foo.go"}}}),
		makeToolResult("c1", "old content"),
		makeExecCodeMsg("c2", []map[string]any{{"tool": "file_write", "args": []string{"/foo.go", "new content"}}}),
		makeToolResult("c2", "ok"),
	}

	out := pruneStaleReads(history)

	// The file_read result (c1) should be gone; everything else stays.
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			t.Error("stale file_read result was not pruned")
		}
	}
	if len(out) != len(history)-1 {
		t.Errorf("len(out) = %d; want %d", len(out), len(history)-1)
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
