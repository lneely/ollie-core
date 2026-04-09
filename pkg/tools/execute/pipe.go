package execute

import (
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
		if len(step.Args) > 0 {
			var escaped []string
			for _, arg := range step.Args {
				escaped = append(escaped, "'"+strings.ReplaceAll(arg, "'", "'\\''")+"'")
			}
			parts = append(parts, fmt.Sprintf("( set -- %s\n%s )", strings.Join(escaped, " "), code))
		} else {
			parts = append(parts, fmt.Sprintf("(\n%s\n)", code))
		}
	}
	return strings.Join(parts, " |\n"), true, nil
}
