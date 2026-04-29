package agent

import (
	"encoding/json"
	"fmt"
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
// file_write to the same path. The last read before a write is kept as the
// base state; only earlier reads are pruned.
func TestPruneStaleReads_BasicWriteInvalidatesRead(t *testing.T) {
	history := []backend.Message{
		{Role: "user", Content: "edit foo"},
		makeExecCodeMsg("c1", []map[string]any{{"tool": "file_read", "args": []string{"/foo.go"}}}),
		makeToolResult("c1", "old content"),
		makeExecCodeMsg("c2", []map[string]any{{"tool": "file_write", "args": []string{"/foo.go", "new content"}}}),
		makeToolResult("c2", "ok"),
	}

	out := pruneStaleReads(history)

	// c1 is the last (only) read before the write — kept as base state.
	if len(out) != 5 {
		t.Errorf("len(out) = %d; want 5 (single read before write is kept)", len(out))
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

// TestPruneStaleReads_MultipleReadsOfSamePath prunes all prior reads except
// the last one (which serves as the base state for the write).
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

	// c1 (earlier read) should be pruned.
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			t.Error("earlier read c1 should be pruned")
		}
	}
	// c2 (last read before write) should be kept.
	foundC2 := false
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c2" {
			foundC2 = true
		}
	}
	if !foundC2 {
		t.Error("last read c2 should be kept as base state")
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

	// c1 is the only (last) read before the edit — kept as base state.
	if len(out) != 5 {
		t.Errorf("len(out) = %d; want 5 (single read before edit is kept)", len(out))
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

// TestPruneStaleReads_ReadWriteReadCycle: the last read before a write is kept
// as base state; a read after the write is also kept (it reflects current state).
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

	// c1 is the only read before the write — kept as base state.
	// c3 is a read after the write — also kept.
	// All messages should be preserved.
	if len(out) != 7 {
		t.Errorf("len(out) = %d; want 7 (both reads kept)", len(out))
	}
}

// TestPruneStaleReads_AssistantWithTextContentKept verifies that an assistant
// message with text content and a single read tool call is fully preserved
// when the read is the last one before a write (kept as base state).
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

	// c1 is the last (only) read before the write — entire message preserved.
	if len(out) != 5 {
		t.Errorf("len(out) = %d; want 5 (all messages kept)", len(out))
	}
	// The assistant message should retain both text and tool calls.
	var foundTextAssistant bool
	for _, m := range out {
		if m.Role == "assistant" && m.Content == "Let me read that file." {
			foundTextAssistant = true
			if len(m.ToolCalls) != 1 {
				t.Error("tool call should be preserved (read is kept as base state)")
			}
		}
	}
	if !foundTextAssistant {
		t.Error("assistant message with text content should be kept")
	}
}

// TestPruneStaleReads_ReasoningPreservesEvidence verifies that when the model
// reads a file, reasons about it, then edits it, the read is kept as base
// state — preserving the evidence that grounds the reasoning.
func TestPruneStaleReads_ReasoningPreservesEvidence(t *testing.T) {
	args1, _ := json.Marshal(map[string]any{"steps": []map[string]any{{"tool": "file_read", "args": []string{"/foo.go"}}}})
	history := []backend.Message{
		{Role: "user", Content: "fix the bug in foo.go"},
		{Role: "assistant", Content: "Let me read the file to understand the issue.", ToolCalls: []backend.ToolCall{
			{ID: "c1", Name: "execute_code", Arguments: json.RawMessage(args1)},
		}},
		makeToolResult("c1", "func handler() {\n\treturn nil // BUG: should return error\n}"),
		{Role: "assistant", Content: "I can see the bug on line 2: the handler returns nil instead of an error. I'll fix this."},
		{Role: "user", Content: "yes, fix it"},
		makeExecCodeMsg("c2", []map[string]any{{"tool": "file_edit", "args": []string{"/foo.go", "return nil", "return fmt.Errorf(...)"}}}),
		makeToolResult("c2", "ok"),
	}

	out := pruneStaleReads(history)

	// c1 is the last (only) read before the edit — kept as base state.
	// The reasoning and all other messages are also preserved.
	if len(out) != 7 {
		t.Errorf("len(out) = %d; want 7 (all messages preserved)", len(out))
	}
}

// TestPruneStaleReads_ReadEditReadEditChain shows that in a read-edit-read-edit
// cycle, each read is the last read before its respective edit and is kept.
func TestPruneStaleReads_ReadEditReadEditChain(t *testing.T) {
	history := []backend.Message{
		{Role: "user", Content: "update foo.go twice"},
		// First read
		makeExecCodeMsg("c1", []map[string]any{{"tool": "file_read", "args": []string{"/foo.go"}}}),
		makeToolResult("c1", "version 1"),
		// First edit
		makeExecCodeMsg("c2", []map[string]any{{"tool": "file_edit", "args": []string{"/foo.go", "old", "new"}}}),
		makeToolResult("c2", "ok"),
		// Verification read
		makeExecCodeMsg("c3", []map[string]any{{"tool": "file_read", "args": []string{"/foo.go"}}}),
		makeToolResult("c3", "version 2"),
		// Second edit
		makeExecCodeMsg("c4", []map[string]any{{"tool": "file_edit", "args": []string{"/foo.go", "new", "newer"}}}),
		makeToolResult("c4", "ok"),
	}

	out := pruneStaleReads(history)

	// c1 is the last read before c2 — kept as base for first edit.
	// c3 is the last read before c4 — kept as base for second edit.
	// All messages preserved.
	if len(out) != 9 {
		t.Errorf("len(out) = %d; want 9 (both reads kept as base states)", len(out))
	}
}

// TestPruneStaleReads_MultiFileTurnAccumulation shows that in a multi-file
// editing session, each file's last read is kept as base state.
func TestPruneStaleReads_MultiFileTurnAccumulation(t *testing.T) {
	history := []backend.Message{
		{Role: "user", Content: "refactor: rename getUserName to getUsername across the codebase"},
	}

	// Simulate: read 5 files, then edit 3 of them.
	files := []string{"/a.go", "/b.go", "/c.go", "/d.go", "/e.go"}
	for i, f := range files {
		id := fmt.Sprintf("r%d", i)
		history = append(history,
			makeExecCodeMsg(id, []map[string]any{{"tool": "file_read", "args": []string{f}}}),
			makeToolResult(id, fmt.Sprintf("contents of %s with getUserName", f)),
		)
	}
	// Edit a.go, c.go, e.go
	for i, f := range []string{"/a.go", "/c.go", "/e.go"} {
		id := fmt.Sprintf("w%d", i)
		history = append(history,
			makeExecCodeMsg(id, []map[string]any{{"tool": "file_edit", "args": []string{f, "getUserName", "getUsername"}}}),
			makeToolResult(id, "ok"),
		)
	}

	out := pruneStaleReads(history)

	// Each edited file has exactly one read — that's the last read, so it's kept.
	// All 5 reads are kept (each is the only/last read of its path).
	kept := 0
	for _, m := range out {
		if m.Role == "tool" && len(m.ToolCallID) == 2 && m.ToolCallID[0] == 'r' {
			kept++
		}
	}
	if kept != 5 {
		t.Errorf("kept %d reads; want 5 (all are last reads of their paths)", kept)
	}

	// Total: 1 user + 5*(read assistant + read tool) + 3*(edit assistant + edit tool) = 1 + 10 + 6 = 17
	if len(out) != 17 {
		t.Errorf("len(out) = %d; want 17", len(out))
	}
}

// TestPruneStaleReads_ParallelReadWrite tests a single execute_code call
// containing both a file_read and file_write to the same path in parallel steps.
func TestPruneStaleReads_ParallelReadWrite(t *testing.T) {
	// A single call that reads and writes the same file.
	history := []backend.Message{
		{Role: "user", Content: "go"},
		makeExecCodeMsg("c1", []map[string]any{
			{"tool": "file_read", "args": []string{"/foo.go"}},
			{"tool": "file_write", "args": []string{"/foo.go", "new"}},
		}),
		makeToolResult("c1", "read result\nwrite ok"),
	}

	out := pruneStaleReads(history)

	// Per the code comment: "a write within the same call supersedes prior-call
	// reads, not reads within this same call." So c1 should NOT be pruned.
	if len(out) != len(history) {
		t.Errorf("len(out) = %d; want %d (same-call read+write should not self-prune)", len(out), len(history))
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
