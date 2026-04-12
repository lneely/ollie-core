package reasoning

import (
	"context"
	"encoding/json"
	"fmt"
	"ollie/pkg/tools"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// SetMemoryBackend implements tools.MemoryBackendSetter.
func (s *Server) SetMemoryBackend(b tools.MemoryBackend) { s.Memory = b }

var ToolRemember = tools.ToolInfo{
	Name: "memory_remember",
	Description: `Save a persistent memory across sessions.

Usage:
- Store key facts, decisions, preferences, or context
- Memories persist for future recall
- Use descriptive titles and specific tags
- Keep body concise and actionable

Title Requirements:
- Descriptive, stands alone
- Answers "what is this about?"
- Minimum 3 characters

Tag Guidelines:
- Specific, stable terms (not generic)
- Concrete topics, people, projects
- Influences recall accuracy
- Minimum 2 characters per tag
- Unique within memory

Body Content:
- Key facts and essential context
- Decisions made or preferences
- Not raw transcripts or chatter
- Minimum 8 characters

Examples:
- Project structure decisions
- User preferences or constraints
- Code patterns or conventions
- Important discoveries or insights`,
	InputSchema: json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"required": ["title", "tags", "body"],
		"properties": {
			"title": {
				"type": "string",
				"minLength": 3,
				"description": "Descriptive title that can stand alone and answer 'what is this about?'"
			},
			"tags": {
				"type": "array",
				"minItems": 1,
				"uniqueItems": true,
				"items": {
					"type": "string",
					"minLength": 2
				},
				"description": "Specific, stable recall tags you would search for later. Prefer concrete topics, people, projects, decisions, or preferences."
			},
			"body": {
				"type": "string",
				"minLength": 8,
				"description": "Concise memory content: key facts, decisions, preferences, and essential context. Not a transcript."
			}
		}
	}`),
}
var ToolRecall = tools.ToolInfo{
	Name: "memory_recall",
	Description: `Search previously saved memories.

Usage:
- Find memories by keyword search
- Searches titles and tags only (not body content)
- Use specific, literal terms from original tags/titles
- Returns matching memories with full details

Search Tips:
- Use exact tags or project names
- Prefer specific terms over vague descriptions
- Minimum query length: 2 characters
- Case-insensitive matching

Examples:
- Project name: "ollie" or "core"
- Topic: "authentication" or "database"
- Person: "user" or specific username
- Decision type: "architecture" or "api-design"

Note: Body content is not searched - only titles and tags.`,
	InputSchema: json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"required": ["query"],
		"properties": {
			"query": {
				"type": "string",
				"minLength": 2,
				"description": "Short keyword or phrase to search for in memory titles and tags. Prefer literal terms such as names, projects, topics, or exact tags."
			}
		}
	}`),
}

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
