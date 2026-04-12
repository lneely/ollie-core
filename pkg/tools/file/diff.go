package file

import (
	"strings"

	"github.com/aymanbagabas/go-udiff"
)

// unifiedDiff produces a unified diff of oldContent → newContent for display.
func unifiedDiff(path, oldContent, newContent string) string {
	return udiff.Unified(path, path, oldContent, newContent)
}

// plusLines formats content as diff "added" lines, each prefixed with "+".
// Used by file_write for new files.
func plusLines(content string) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	sb := strings.Builder{}
	for _, l := range lines {
		sb.WriteByte('+')
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	return sb.String()
}
