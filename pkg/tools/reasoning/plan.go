package reasoning

import (
	"encoding/json"
	"ollie/pkg/tools"
)

var ToolPlan = tools.ToolInfo{
	Name: "reasoning_plan",
	Description: `Executive planning tool.

Use this tool before taking action whenever the task is non-trivial: specifically, when it requires multiple steps, has dependencies, may produce side effects, benefits from persistence/resumption, or the correct order is not obvious.

First, create a complete ordered plan for the current goal. The plan must:
- Break the goal into concrete, outcome-oriented steps
- Order steps by execution sequence
- Record explicit dependencies
- Allow dependencies only on earlier steps (lower index)
- Keep steps small enough to execute or verify independently
- Avoid redundant or purely decorative steps

Each step should include:
- index
- short title
- description
- depends_on (list of earlier step indexes only)
- acceptance_criteria

If a task backend is available via ` + "`task_create`" + `, persist the plan and steps there and use returned task IDs in subsequent updates. If no backend is available, maintain the same ordered plan in memory and execute it sequentially.

Use ` + "`reasoning_plan`" + `only to create or revise the overall work breakdown. Use ` + "`reasoning_think`" + ` for short, local reflection during execution. Do not substitute moment-to-moment reasoning for a full plan when planning is required.

Re-plan only if:
- a step fails,
- new information changes the goal or constraints,
- dependencies were incorrect or incomplete,
- or a better decomposition becomes necessary.

Do not begin executing dependent work until its dependencies are complete.
`,
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
							"items": {
								"type": "integer",
								"minimum": 0
							},
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

// SetPlanBackend implements tools.PlanBackendSetter.
func (s *Server) SetPlanBackend(b tools.PlanBackend) { s.Plan = b }
