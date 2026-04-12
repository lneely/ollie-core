package reasoning

import (
	"encoding/json"
	"ollie/pkg/tools"
)

var ToolPlan = tools.ToolInfo{
	Name: "reasoning_plan",
	Description: `Create an execution plan for multi-step tasks.

Usage:
- Use for non-trivial tasks requiring multiple steps
- Plan before acting when order matters or there are dependencies
- Creates task records if task backend available
- Execute steps sequentially based on dependencies

Plan Structure:
- goal: top-level objective
- steps: array of ordered steps
- Each step: title, description, depends_on, acceptance_criteria
- depends_on: earlier step indexes (0-based)
- Execute steps only after dependencies complete

When to Plan:
- Multiple steps required
- Steps have dependencies
- Task benefits from persistence
- Execution order not obvious

Re-plan If:
- Step fails
- New information changes constraints
- Dependencies incorrect
- Better decomposition possible

Examples:
- Code modification: read → analyze → edit → test
- Project exploration: list files → examine structure → read key files`,
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
