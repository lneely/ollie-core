package execute

// execute_pipe constructs a pipeline of execute_code and execute_tool calls
// to enable the composition of tools and arbitrary code.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PipeStep is one stage in a tool pipeline.
// Exactly one of Tool or Code must be set.
type PipeStep struct {
	Tool string // named tool read from 9P (trusted)
	Code string // inline bash code (untrusted, validated)
	Args []string
}

// BuildPipeline constructs a single bash pipeline string from the given steps.
// Tool steps are trusted (sourced from 9P); inline code steps are validated
// individually so the combined string is always returned as trusted.
func BuildPipeline(steps []PipeStep) (string, bool, error) {
	if len(steps) == 0 {
		return "", false, fmt.Errorf("pipe requires at least one step")
	}
	validator := &Server{} // used only for ValidateCode; no shared rate-limit state
	parts := make([]string, 0, len(steps))
	for _, step := range steps {
		var code string
		if step.Tool != "" {
			var err error
			code, err = ReadTool(step.Tool)
			if err != nil {
				return "", false, fmt.Errorf("pipe step %q: %v", step.Tool, err)
			}
		} else if step.Code != "" {
			if err := validator.ValidateCode(step.Code); err != nil {
				return "", false, fmt.Errorf("pipe step code: %v", err)
			}
			code = step.Code
		} else {
			return "", false, fmt.Errorf("each pipe step requires either 'tool' or 'code'")
		}
		language := "bash"
		if step.Tool != "" {
			language = detectLanguage(code)
		}
		if len(step.Args) > 0 {
			code = injectArgs(language, step.Tool, step.Args, code)
		}
		switch language {
		case "python3":
			parts = append(parts, fmt.Sprintf("( python3 -c $'%s' )", ansiCEscape(code)))
		default:
			parts = append(parts, fmt.Sprintf("(\n%s\n)", code))
		}
	}
	return strings.Join(parts, " |\n"), true, nil

}

func dispatchExecutePipe(ctx context.Context, e *Server, args json.RawMessage) (string, error) {
	var a struct {
		Pipe    []PipeStep `json:"pipe"`
		Timeout int        `json:"timeout"`
		Sandbox string     `json:"sandbox"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("execute_pipe: bad args: %w", err)
	}
	if len(a.Pipe) == 0 {
		return "", fmt.Errorf("execute_pipe: 'pipe' is required")
	}
	code, _, err := BuildPipeline(a.Pipe)
	if err != nil {
		return "", err
	}
	if e.Confirm != nil && !e.Confirm(fmt.Sprintf("execute_pipe: %s", code)) {
		return "", fmt.Errorf("execute_pipe: denied by user")
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	sandbox := a.Sandbox
	if sandbox == "" {
		sandbox = "default"
	}
	return e.Execute(ctx, code, "bash", timeout, sandbox, true)
}
