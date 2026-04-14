package task

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"ollie/pkg/tools"
)

// Decl returns a factory for a task Server rooted at planDir.
func Decl(planDir string) func() tools.Server {
	return func() tools.Server { return &Server{planDir: planDir} }
}

// Server implements tools.Server for task_plan and task_complete.
type Server struct {
	planDir string
}

// ListTools implements tools.Server.
func (s *Server) ListTools() ([]tools.ToolInfo, error) {
	return []tools.ToolInfo{ToolPlan, ToolComplete}, nil
}

// CallTool implements tools.Server.
func (s *Server) CallTool(_ context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	var text string
	var err error
	switch tool {
	case ToolPlan.Name:
		text, err = s.plan(args)
	case ToolComplete.Name:
		text, err = s.complete(args)
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

type planStep struct {
	Title string `json:"title"`
	Body  string `json:"description"`
	After []int  `json:"depends_on"`
}

func (s *Server) plan(args json.RawMessage) (string, error) {
	var a struct {
		Goal  string     `json:"goal"`
		Steps []planStep `json:"steps"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("bad args: %w", err)
	}
	if a.Goal == "" {
		return "", fmt.Errorf("'goal' is required")
	}
	if len(a.Steps) == 0 {
		return "", fmt.Errorf("'steps' must not be empty")
	}

	order, err := topoSort(a.Steps)
	if err != nil {
		return "", err
	}

	slug := slugify(a.Goal)
	uid := make([]byte, 4)
	rand.Read(uid) //nolint:errcheck
	ts := time.Now().Format("20060102T150405")
	filename := ts + "_" + fmt.Sprintf("%08x", uid) + "--" + slug + "__todo.md"

	var md strings.Builder
	fmt.Fprintf(&md, "# %s\n\n", a.Goal)
	for _, idx := range order {
		step := a.Steps[idx]
		fmt.Fprintf(&md, "- [ ] %s\n", step.Title)
		if step.Body != "" {
			for line := range strings.SplitSeq(step.Body, "\n") {
				fmt.Fprintf(&md, "  %s\n", line)
			}
		}
	}

	if err := os.MkdirAll(s.planDir, 0755); err != nil {
		return "", fmt.Errorf("plan dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.planDir, filename), []byte(md.String()), 0644); err != nil {
		return "", fmt.Errorf("write plan: %w", err)
	}

	return fmt.Sprintf(
		"Plan saved to pl/%s (%d steps). Rename __todo → __wip when you start work, use task_complete to mark steps done, rename __wip → __done when the goal is realized.",
		filename, len(a.Steps),
	), nil
}

func (s *Server) complete(args json.RawMessage) (string, error) {
	var a struct {
		File string `json:"file"`
		Step string `json:"step"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("bad args: %w", err)
	}
	if a.File == "" {
		return "", fmt.Errorf("'file' is required")
	}
	if a.Step == "" {
		return "", fmt.Errorf("'step' is required")
	}

	path := filepath.Join(s.planDir, a.File)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read plan: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	needle := strings.ToLower(strings.TrimSpace(a.Step))
	matched := false
	for i, line := range lines {
		if !strings.HasPrefix(line, "- [ ] ") {
			continue
		}
		title := strings.ToLower(strings.TrimPrefix(line, "- [ ] "))
		if strings.Contains(title, needle) {
			lines[i] = "- [x] " + strings.TrimPrefix(line, "- [ ] ")
			matched = true
			break
		}
	}
	if !matched {
		return "", fmt.Errorf("no unchecked step matching %q in %s", a.Step, a.File)
	}

	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		return "", fmt.Errorf("write plan: %w", err)
	}

	var remaining []string
	for _, line := range lines {
		if strings.HasPrefix(line, "- [ ] ") {
			remaining = append(remaining, strings.TrimPrefix(line, "- [ ] "))
		}
	}

	if len(remaining) == 0 {
		return "step marked complete. all steps done.", nil
	}
	return fmt.Sprintf("step marked complete. remaining steps:\n- %s", strings.Join(remaining, "\n- ")), nil
}

func topoSort(steps []planStep) ([]int, error) {
	n := len(steps)
	visited := make([]bool, n)
	result := make([]int, 0, n)

	var visit func(i int, path []bool) error
	visit = func(i int, path []bool) error {
		if path[i] {
			return fmt.Errorf("dependency cycle at step %d", i)
		}
		if visited[i] {
			return nil
		}
		path[i] = true
		for _, dep := range steps[i].After {
			if dep < 0 || dep >= n {
				continue
			}
			if err := visit(dep, path); err != nil {
				return err
			}
		}
		path[i] = false
		visited[i] = true
		result = append(result, i)
		return nil
	}

	for i := range steps {
		if err := visit(i, make([]bool, n)); err != nil {
			return nil, err
		}
	}
	return result, nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = s[:60]
		if i := strings.LastIndex(s, "-"); i > 20 {
			s = s[:i]
		}
	}
	return s
}

var ToolPlan = tools.ToolInfo{
	Name: "task_plan",
	Description: `Create an execution plan for multi-step tasks.

Usage:
- Use for non-trivial tasks requiring multiple steps
- Plan before acting when order matters or there are dependencies
- Saves a markdown checklist to the plan directory (pl/)
- Execute steps sequentially based on dependencies

Plan Structure:
- goal: top-level objective
- steps: array of ordered steps
- Each step: title, description, depends_on, acceptance_criteria
- depends_on: earlier step indexes (0-based)
- Execute steps only after dependencies complete

Plan Lifecycle:
- Plans are saved as __todo.md files in pl/
- Rename __todo → __wip when you begin work
- Use task_complete after each step finishes
- Rename __wip → __done when the goal is realized

When to Plan:
- Multiple steps required
- Steps have dependencies
- Execution order is not obvious

Re-plan If:
- A step fails
- New information changes constraints
- Better decomposition is possible`,
	InputSchema: json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"required": ["goal", "steps"],
		"properties": {
			"goal": {
				"type": "string",
				"description": "Top-level goal or objective."
			},
			"steps": {
				"type": "array",
				"description": "Ordered plan steps. Dependencies may only reference earlier 0-based step indexes.",
				"items": {
					"type": "object",
					"additionalProperties": false,
					"required": ["title"],
					"properties": {
						"title": {
							"type": "string",
							"description": "Short, outcome-oriented step title."
						},
						"description": {
							"type": "string",
							"description": "Optional implementation detail for the step."
						},
						"depends_on": {
							"type": "array",
							"description": "0-based indexes of earlier steps that must complete before this one.",
							"items": {"type": "integer", "minimum": 0},
							"default": []
						},
						"acceptance_criteria": {
							"type": "string",
							"description": "How to tell the step is complete or successful."
						}
					}
				}
			}
		}
	}`),
}

var ToolComplete = tools.ToolInfo{
	Name: "task_complete",
	Description: `Mark a plan step as complete.

Finds the first unchecked step whose title contains the given string
(case-insensitive) and marks it [x]. Returns the remaining unchecked steps
so you always know what's left.

Call this immediately after finishing each step — before starting the next.`,
	InputSchema: json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"required": ["file", "step"],
		"properties": {
			"file": {
				"type": "string",
				"description": "Plan filename as returned by task_plan (e.g. '20240101T120000_ab12cd34--my-goal__wip.md')."
			},
			"step": {
				"type": "string",
				"description": "Title (or substring) of the step to mark complete. Matched case-insensitively against unchecked steps."
			}
		}
	}`),
}
