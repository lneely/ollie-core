package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"ollie/pkg/tools"
)

// dispatchPlanBackend implements tools.PlanBackend by dispatching task_create
// calls through the agent's dispatcher to a registered task MCP server.
type dispatchPlanBackend struct {
	d      tools.Dispatcher
	server string // name of the task server (e.g. "task")
}

// CreatePlan creates a top-level goal bead and one child bead per step,
// wiring dependency edges from the After indices.
func (b *dispatchPlanBackend) CreatePlan(ctx context.Context, goal string, steps []tools.PlanStep) ([]string, string, error) {
	goalID, err := b.create(ctx, goal, "", "")
	if err != nil {
		return nil, "", fmt.Errorf("create goal bead: %w", err)
	}

	ids := make([]string, len(steps))
	for i, step := range steps {
		var blockers []string
		for _, idx := range step.After {
			if idx >= 0 && idx < i && ids[idx] != "" {
				blockers = append(blockers, ids[idx])
			}
		}
		id, err := b.create(ctx, step.Title, step.Body, goalID, blockers...)
		if err != nil {
			return nil, "", fmt.Errorf("create step %d: %w", i, err)
		}
		ids[i] = id
	}
	return ids, "", nil
}

func (b *dispatchPlanBackend) create(ctx context.Context, title, body, parent string, blockers ...string) (string, error) {
	args := map[string]any{"title": title}
	if body != "" {
		args["body"] = body
	}
	if parent != "" {
		args["parent"] = parent
	}
	if len(blockers) > 0 {
		args["blockers"] = blockers
	}

	raw, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	result, err := b.d.Dispatch(ctx, b.server, "task_create", raw)
	if err != nil {
		return "", err
	}

	// Expected: {"content": [{"type": "text", "text": "<id>"}]}
	var resp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &resp); err != nil || len(resp.Content) == 0 {
		return "", fmt.Errorf("unexpected task_create response: %s", string(result))
	}
	return resp.Content[0].Text, nil
}
