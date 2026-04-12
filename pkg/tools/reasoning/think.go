package reasoning

import (
	"encoding/json"
	"ollie/pkg/tools"
)

// think is a no-op tool for reasoning models. it functions like an in-context "scratchpad".

var ToolThink = tools.ToolInfo{
	Name:        "reasoning_think",
	Description: "Internal reasoning tool for breaking down problems, planning approaches, analyzing constraints, and reasoning through multi-step solutions before acting. The thought content is not shown to the user.",
	InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["thought"],
				"properties": {
					"thought": {"type": "string", "description": "A reflective note or intermediate reasoning step."}
				}
			}`),
}
