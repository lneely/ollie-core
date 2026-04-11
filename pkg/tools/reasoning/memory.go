package reasoning

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// FlatDirBackend implements tools.MemoryBackend using a plain directory of
// markdown files. Filenames follow a denote-like convention:
//
//	YYYYMMddTHHmmss--title-slug__tag1_tag2.md
//
// Recall searches filenames only — no body reads. Tag and title are both
// encoded in the filename, making it the sole search surface.
type FlatDirBackend struct {
	dir string
}

// NewFlatDirBackend returns a FlatDirBackend rooted at dir. If dir is empty,
// OLLIE_MEMORY_PATH is used, falling back to ~/.local/share/ollie/memory/.
func NewFlatDirBackend(dir string) *FlatDirBackend {
	if dir == "" {
		dir = os.Getenv("OLLIE_MEMORY_PATH")
	}
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "share", "ollie", "memory")
	}
	return &FlatDirBackend{dir: dir}
}

// Remember writes a new memory file and returns its filename.
func (b *FlatDirBackend) Remember(_ context.Context, title string, tags []string, body string) (string, error) {
	if err := os.MkdirAll(b.dir, 0700); err != nil {
		return "", fmt.Errorf("memory dir: %w", err)
	}

	slug := slugify(title)
	ts := time.Now().Format("20060102T150405")

	name := ts + "--" + slug
	if len(tags) > 0 {
		name += "__" + strings.Join(tags, "_")
	}
	name += ".md"

	content := buildFlatNote(title, tags, body)
	path := filepath.Join(b.dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return "", fmt.Errorf("write memory: %w", err)
	}
	return name, nil
}

// Recall searches memory filenames for the query and returns matching file
// contents. Body content is not searched.
func (b *FlatDirBackend) Recall(_ context.Context, query string) (string, error) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "no memories found matching: " + query, nil
		}
		return "", fmt.Errorf("read memory dir: %w", err)
	}

	q := strings.ToLower(query)
	var results []string

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if !strings.Contains(strings.ToLower(e.Name()), q) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(b.dir, e.Name()))
		if err != nil {
			continue
		}
		results = append(results, fmt.Sprintf("file: %s\n\n%s", e.Name(), strings.TrimSpace(string(data))))
	}

	if len(results) == 0 {
		return "no memories found matching: " + query, nil
	}
	return strings.Join(results, "\n\n---\n\n"), nil
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// slugify converts a title to a lowercase hyphen-separated slug.
func slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

// buildFlatNote formats a markdown note with a minimal frontmatter header.
func buildFlatNote(title string, tags []string, body string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n", title)
	if len(tags) > 0 {
		fmt.Fprintf(&sb, "tags: %s\n\n", strings.Join(tags, ", "))
	}
	sb.WriteString(strings.TrimSpace(body))
	sb.WriteByte('\n')
	return sb.String()
}
