package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"ollie/pkg/tools"
)

// Decl returns a factory for a memory Server.
func Decl() func() tools.Server {
	return func() tools.Server { return &Server{dir: flatDirPath()} }
}

// Server implements tools.Server for memory_remember and memory_recall.
type Server struct {
	dir string
}

// ListTools implements tools.Server.
func (s *Server) ListTools() ([]tools.ToolInfo, error) {
	return []tools.ToolInfo{ToolRemember, ToolRecall}, nil
}

// CallTool implements tools.Server.
func (s *Server) CallTool(_ context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	var text string
	var err error
	switch tool {
	case ToolRemember.Name:
		text, err = s.remember(args)
	case ToolRecall.Name:
		text, err = s.recall(args)
	default:
		text = "error: unknown tool: " + tool
	}
	if err != nil {
		text = "error: " + err.Error()
	}
	result, _ := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
	return result, nil
}

// Close implements tools.Server (no-op).
func (s *Server) Close() {}

func (s *Server) remember(args json.RawMessage) (string, error) {
	var a struct {
		Title string   `json:"title"`
		Tags  []string `json:"tags"`
		Body  string   `json:"body"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("bad args: %w", err)
	}
	if a.Title == "" {
		return "", fmt.Errorf("'title' is required")
	}
	if len(a.Tags) == 0 {
		return "", fmt.Errorf("'tags' must be non-empty")
	}
	if a.Body == "" {
		return "", fmt.Errorf("'body' is required")
	}

	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return "", fmt.Errorf("memory dir: %w", err)
	}

	slug := slugify(a.Title)
	ts := time.Now().Format("20060102T150405")
	name := ts + "--" + slug + "__" + strings.Join(a.Tags, "_") + ".md"

	content := buildNote(a.Title, a.Tags, a.Body)
	if err := os.WriteFile(filepath.Join(s.dir, name), []byte(content), 0600); err != nil {
		return "", fmt.Errorf("write memory: %w", err)
	}
	return name, nil
}

func (s *Server) recall(args json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("bad args: %w", err)
	}
	if a.Query == "" {
		return "", fmt.Errorf("'query' is required")
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "no memories found matching: " + a.Query, nil
		}
		return "", fmt.Errorf("read memory dir: %w", err)
	}

	q := strings.ToLower(a.Query)
	var results []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if !strings.Contains(strings.ToLower(e.Name()), q) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		results = append(results, fmt.Sprintf("file: %s\n\n%s", e.Name(), strings.TrimSpace(string(data))))
	}

	if len(results) == 0 {
		return "no memories found matching: " + a.Query, nil
	}
	return strings.Join(results, "\n\n---\n\n"), nil
}

// flatDirPath resolves the memory directory: OLLIE_MEMORY_PATH env var or
// ~/.local/share/ollie/memory/.
func flatDirPath() string {
	if d := os.Getenv("OLLIE_MEMORY_PATH"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "ollie", "memory")
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlnum.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func buildNote(title string, tags []string, body string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n", title)
	if len(tags) > 0 {
		fmt.Fprintf(&sb, "tags: %s\n\n", strings.Join(tags, ", "))
	}
	sb.WriteString(strings.TrimSpace(body))
	sb.WriteByte('\n')
	return sb.String()
}

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
