package agent

import (
	"encoding/json"

	"ollie/pkg/backend"
)

// pruneStaleReads returns a copy of history with tool result messages removed
// when they correspond to a file_read that was superseded by a subsequent
// file_write or file_edit to the same path within an execute_code call.
//
// Only tool-script steps inside execute_code are analysed; inline code steps
// are ignored (paths are not reliably extractable from arbitrary shell code).
// Pruning is conservative: when in doubt, keep the message.
func pruneStaleReads(history []backend.Message) []backend.Message {
	// Phase 1: walk history in order, tracking which tool-call IDs read which
	// paths and marking reads stale when a subsequent write touches the same path.
	//
	// reads accumulates ALL read call-IDs for a path since the last write so
	// that multiple reads of the same file are all pruned on a subsequent write.
	reads := make(map[string][]string) // path → []callID since last write
	stale := make(map[string]bool)     // callID → true when superseded

	for _, m := range history {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Name != "execute_code" {
				continue
			}
			readPaths, writePaths := extractExecCodePaths(tc.Arguments)
			// Mark all accumulated reads of written paths as stale before
			// recording new reads (a write within the same call supersedes
			// prior-call reads, not reads within this same call).
			for _, p := range writePaths {
				for _, id := range reads[p] {
					stale[id] = true
				}
				delete(reads, p)
			}
			for _, p := range readPaths {
				reads[p] = append(reads[p], tc.ID)
			}
		}
	}

	if len(stale) == 0 {
		return history
	}

	// Phase 2: rebuild history, dropping stale tool results AND their
	// corresponding tool calls from assistant messages.
	out := make([]backend.Message, 0, len(history))
	for _, m := range history {
		if m.Role == "tool" && stale[m.ToolCallID] {
			continue
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			// Filter out stale tool calls.
			var kept []backend.ToolCall
			for _, tc := range m.ToolCalls {
				if !stale[tc.ID] {
					kept = append(kept, tc)
				}
			}
			if len(kept) == 0 && m.Content == "" {
				// Assistant message had only stale tool calls and no text; drop it.
				continue
			}
			if len(kept) != len(m.ToolCalls) {
				// Some calls were pruned; create a modified copy.
				m = backend.Message{Role: m.Role, Content: m.Content, ToolCalls: kept}
			}
		}
		out = append(out, m)
	}
	return out
}

// execCodeStep mirrors the relevant subset of the execute_code "steps" schema.
type execCodeStep struct {
	Tool string   `json:"tool"`
	Args []string `json:"args"`
	// Code is intentionally omitted — paths are not extracted from inline code.
}

type execCodeArgs struct {
	Steps []execCodeStep `json:"steps"`
}

// extractExecCodePaths parses execute_code JSON arguments and returns the file
// paths read (via file_read steps) and written (via file_write/file_edit steps).
// Returns nil slices on parse failure.
func extractExecCodePaths(raw json.RawMessage) (reads, writes []string) {
	var a execCodeArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, nil
	}
	for _, step := range a.Steps {
		if len(step.Args) == 0 {
			continue
		}
		path := step.Args[0]
		switch step.Tool {
		case "file_read":
			reads = append(reads, path)
		case "file_write", "file_edit":
			writes = append(writes, path)
		}
	}
	return reads, writes
}
