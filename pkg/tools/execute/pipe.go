package execute

// execute_pipe constructs a sequential pipeline of code and tool steps,
// enabling composition of named scripts and arbitrary inline code.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// PipeStep is one stage in a tool pipeline.
// Exactly one of Tool, Code, or Parallel must be set.
type PipeStep struct {
	Tool     string     // named tool read from 9P (trusted)
	Code     string     // inline bash code (untrusted, validated)
	Args     []string
	Parallel []CodeStep // parallel fan-out; outputs concatenated in submission order
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
			if err := validator.ValidateCode(step.Code, "bash"); err != nil {
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
		case "perl":
			parts = append(parts, fmt.Sprintf("( perl -e $'%s' )", ansiCEscape(code)))
		case "awk":
			parts = append(parts, fmt.Sprintf("( gawk -e $'%s' )", ansiCEscape(code)))
		case "sed":
			parts = append(parts, fmt.Sprintf("( sed -e $'%s' )", ansiCEscape(code)))
		case "jq":
			parts = append(parts, fmt.Sprintf("( jq $'%s' )", ansiCEscape(code)))
		case "bc":
			parts = append(parts, fmt.Sprintf("( bc -ql <<< $'%s' )", ansiCEscape(code)))
		case "lua":
			parts = append(parts, fmt.Sprintf("( lua -e $'%s' )", ansiCEscape(code)))
		default:
			parts = append(parts, fmt.Sprintf("(\n%s\n)", code))
		}
	}
	return strings.Join(parts, " |\n"), true, nil

}

func dispatchExecutePipe(ctx context.Context, e *Server, args json.RawMessage) (string, error) {
	var a struct {
		Pipe []struct {
			Tool     string     `json:"tool"`
			Args     []string   `json:"args"`
			Code     string     `json:"code"`
			Parallel []CodeStep `json:"parallel"`
		} `json:"pipe"`
		Timeout int    `json:"timeout"`
		Sandbox string `json:"sandbox"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("execute_pipe: bad args: %w", err)
	}
	if len(a.Pipe) == 0 {
		return "", fmt.Errorf("execute_pipe: 'pipe' is required")
	}

	stages := make([]PipeStep, len(a.Pipe))
	for i, s := range a.Pipe {
		stages[i] = PipeStep{Tool: s.Tool, Code: s.Code, Args: s.Args, Parallel: s.Parallel}
	}

	if !e.allowed("execute_pipe", fmt.Sprintf("execute_pipe: %d stage(s)", len(stages))) {
		return "", fmt.Errorf("execute_pipe: denied by user")
	}

	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	sandboxName := a.Sandbox
	if sandboxName == "" {
		sandboxName = "default"
	}

	// Execute stages sequentially, feeding each stage's stdout as the next stage's stdin.
	var input string
	for i, stage := range stages {
		out, err := e.runPipeStage(ctx, i, stage, timeout, sandboxName, input)
		if err != nil {
			return out, fmt.Errorf("pipe stage %d: %w", i, err)
		}
		input = out
	}
	return input, nil
}

// runPipeStage executes one stage of an execute_pipe pipeline, feeding stdinData
// as stdin. Parallel stages fan out concurrently and concatenate in submission order.
func (e *Server) runPipeStage(ctx context.Context, idx int, stage PipeStep, timeout int, sandboxName, stdinData string) (string, error) {
	// Parallel stage: fan out, collect in submission order, concatenate.
	if len(stage.Parallel) > 0 {
		results := make([]string, len(stage.Parallel))
		errs := make([]error, len(stage.Parallel))
		var wg sync.WaitGroup
		for i, step := range stage.Parallel {
			wg.Add(1)
			go func(j int, s CodeStep) {
				defer wg.Done()
				code, lang, trusted, err := resolveCodeStep(s)
				if err != nil {
					errs[j] = err
					return
				}
				results[j], errs[j] = e.executeWithStdin(ctx, code, lang, timeout, sandboxName, trusted, stdinData)
			}(i, step)
		}
		wg.Wait()
		var sb strings.Builder
		for i, out := range results {
			sb.WriteString(out)
			if errs[i] != nil {
				return sb.String(), errs[i]
			}
		}
		return sb.String(), nil
	}

	// Tool stage.
	if stage.Tool != "" {
		toolCode, err := ReadTool(stage.Tool)
		if err != nil {
			return "", err
		}
		language := detectLanguage(toolCode)
		code := toolCode
		if len(stage.Args) > 0 {
			code = injectArgs(language, stage.Tool, stage.Args, toolCode)
			switch language {
			case "awk", "sed", "jq", "ed", "expect", "bc":
				language = "bash"
			}
		}
		return e.executeWithStdin(ctx, code, language, timeout, sandboxName, true, stdinData)
	}

	// Inline code stage (bash).
	if stage.Code != "" {
		validator := &Server{}
		if err := validator.ValidateCode(stage.Code, "bash"); err != nil {
			return "", err
		}
		return e.executeWithStdin(ctx, stage.Code, "bash", timeout, sandboxName, false, stdinData)
	}

	return "", fmt.Errorf("stage %d: requires code, tool, or parallel", idx)
}
