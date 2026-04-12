package reasoning

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ollie/pkg/tools"
)

// Decl returns a factory for a reasoning Server.
func Decl() func() tools.Server { return func() tools.Server { return &Server{} } }

// Server implements tools.Server for reasoning tools.
type Server struct {
	// Plan is the optional task persistence backend. When nil, reasoning_plan
	// produces an in-context plan only. This in-context plan queues each step
	// of the plan, in order, as prompts for the model to execute when the
	// planning turn finishes.
	Plan tools.PlanBackend

	// Memory is the memory persistence backend. Always non-nil after
	// BuildAgentEnv wires it; defaults to a FlatDirBackend.
	Memory tools.MemoryBackend
}

// ListTools implements tools.Server.
func (s *Server) ListTools() ([]tools.ToolInfo, error) {
	return []tools.ToolInfo{
		ToolThink,
		ToolPlan,
		ToolRemember,
		ToolRecall,
	}, nil
}

// CallTool implements tools.Server.
func (s *Server) CallTool(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	var text string
	switch tool {
	case ToolThink.Name:
		var a struct {
			Thought string `json:"thought"`
		}
		if json.Unmarshal(args, &a) != nil || a.Thought == "" {
			text = "error: missing required field 'thought'"
		}
		// No-op: the thought is recorded in conversation history by the loop.
	case ToolPlan.Name:
		var err error
		text, err = s.plan(ctx, args)
		if err != nil {
			text = "error: " + err.Error()
		}
	case ToolRemember.Name:
		var err error
		text, err = s.remember(ctx, args)
		if err != nil {
			text = "error: " + err.Error()
		}
	case ToolRecall.Name:
		var err error
		text, err = s.recall(ctx, args)
		if err != nil {
			text = "error: " + err.Error()
		}
	default:
		text = "error: unknown tool: " + tool
	}
	return json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
}

// Close implements tools.Server (no-op).
func (s *Server) Close() {}

func (s *Server) plan(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Goal  string `json:"goal"`
		Steps []struct {
			Title string `json:"title"`
			Body  string `json:"body"`
			After []int  `json:"after"`
		} `json:"steps"`
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

	steps := make([]tools.PlanStep, len(a.Steps))
	for i, st := range a.Steps {
		steps[i] = tools.PlanStep{Title: st.Title, Body: st.Body, After: st.After}
	}

	var ids []string
	var backendMsg string
	var persistErr error
	if s.Plan != nil {
		ids, backendMsg, persistErr = s.Plan.CreatePlan(ctx, a.Goal, steps)
		if persistErr != nil {
			ids = nil // degrade to in-context plan
		}
	}

	// If the backend supplied a response message, use it directly — the backend
	// knows best how the agent should proceed (e.g. yield after queuing).
	if backendMsg != "" {
		return backendMsg, nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Plan: %s\n\n", a.Goal)
	for i, st := range a.Steps {
		id := ""
		if i < len(ids) && ids[i] != "" {
			id = fmt.Sprintf(" [%s]", ids[i])
		}
		fmt.Fprintf(&sb, "%d. %s%s\n", i+1, st.Title, id)
		if st.Body != "" {
			fmt.Fprintf(&sb, "   %s\n", st.Body)
		}
		if len(st.After) > 0 {
			deps := make([]string, len(st.After))
			for j, d := range st.After {
				deps[j] = fmt.Sprintf("step %d", d+1)
			}
			fmt.Fprintf(&sb, "   after: %s\n", strings.Join(deps, ", "))
		}
	}

	switch {
	case len(ids) > 0:
		sb.WriteString("\nTasks committed. Use task_list to see ready steps.")
	case persistErr != nil:
		fmt.Fprintf(&sb, "\nWarning: task backend error (%v). Plan exists in context only.", persistErr)
	case s.Plan != nil:
		sb.WriteString("\nTask backend present but returned no IDs. Plan exists in context only.")
	default:
		sb.WriteString("\nNo task backend configured. Plan exists in context only.")
	}

	return sb.String(), nil
}

func (s *Server) remember(ctx context.Context, args json.RawMessage) (string, error) {
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
	if s.Memory == nil {
		return "", fmt.Errorf("no memory backend configured")
	}
	return s.Memory.Remember(ctx, a.Title, a.Tags, a.Body)
}

func (s *Server) recall(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("bad args: %w", err)
	}
	if a.Query == "" {
		return "", fmt.Errorf("'query' is required")
	}
	if s.Memory == nil {
		return "", fmt.Errorf("no memory backend configured")
	}
	return s.Memory.Recall(ctx, a.Query)
}

// Compile-time interface checks
var _ tools.Server = (*Server)(nil)
var _ tools.PlanBackendSetter = (*Server)(nil)
var _ tools.MemoryBackendSetter = (*Server)(nil)
