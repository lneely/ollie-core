package reasoning

import (
	"encoding/json"
	"ollie/pkg/tools"
)

// think is a no-op tool for reasoning models. it functions like an in-context "scratchpad".

var ToolThink = tools.ToolInfo{
	Name: "reasoning_think",
	Description: `Internal reasoning scratchpad (thoughts not shown to user).

Usage:
- Break down complex problems
- Analyze constraints and requirements  
- Reason through multi-step solutions
- Plan approaches before acting
- Work through logic and edge cases

Notes:
- Thought content is internal only (not visible to user)
- Use for local reasoning during execution
- Use reasoning_plan for structured multi-step planning
- Keep thoughts focused and actionable

Examples:
- Analyzing code structure before modification
- Considering alternative approaches
- Working through error scenarios
- Validating assumptions before acting`,
	InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["thought"],
				"properties": {
					"thought": {"type": "string", "description": "A reflective note or intermediate reasoning step."}
				}
			}`),
}
