package execute

import (
	"context"
	"fmt"
	"sync"
)

// runStage executes one stage of an execute_code pipeline, feeding stdinData as stdin.
// Parallel stages fan out concurrently and concatenate results in submission order.
func (e *Server) runStage(ctx context.Context, idx int, stage CodeStep, timeout int, sandboxName, stdinData string) (string, error) {
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
		var out string
		for i, r := range results {
			out += r
			if errs[i] != nil {
				return out, errs[i]
			}
		}
		return out, nil
	}

	if stage.Tool != "" || stage.Code != "" {
		code, lang, trusted, err := resolveCodeStep(stage)
		if err != nil {
			return "", err
		}
		return e.executeWithStdin(ctx, code, lang, timeout, sandboxName, trusted, stdinData)
	}

	return "", fmt.Errorf("stage %d: requires code, tool, or parallel", idx)
}
