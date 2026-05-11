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
				if s.Elevated {
					e.wdMu.RLock()
					dir := e.cwd
					e.wdMu.RUnlock()
					results[j], errs[j] = e.executeElevated(ctx, s.Code, dir, timeout)
					return
				}
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

	if stage.Elevated {
		e.wdMu.RLock()
		dir := e.cwd
		e.wdMu.RUnlock()
		return e.executeElevated(ctx, stage.Code, dir, timeout)
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

// runReadBatch runs a contiguous slice of read-safe stages concurrently,
// concatenating their outputs in submission order. All stages receive the
// same stdinData (read-safe tools don't depend on stdin chaining).
// If e.lockDir is set, each goroutine acquires LOCK_SH before running.
func (e *Server) runReadBatch(ctx context.Context, startIdx int, stages []CodeStep, timeout int, sandboxName, stdinData string) (string, error) {
	if len(stages) == 1 {
		lf, err := acquireFlock(e.lockDir, "rw", false)
		if err != nil {
			return "", fmt.Errorf("step %d: lock: %w", startIdx, err)
		}
		if lf != nil {
			defer lf.Close()
		}
		out, err := e.runStage(ctx, startIdx, stages[0], timeout, sandboxName, stdinData)
		if err != nil {
			return out, fmt.Errorf("step %d: %w", startIdx, err)
		}
		return out, nil
	}

	results := make([]string, len(stages))
	errs := make([]error, len(stages))
	var wg sync.WaitGroup
	for i, stage := range stages {
		wg.Add(1)
		go func(j int, s CodeStep) {
			defer wg.Done()
			lf, lerr := acquireFlock(e.lockDir, "rw", false)
			if lerr != nil {
				errs[j] = fmt.Errorf("step %d: lock: %w", startIdx+j, lerr)
				return
			}
			if lf != nil {
				defer lf.Close()
			}
			results[j], errs[j] = e.runStage(ctx, startIdx+j, s, timeout, sandboxName, stdinData)
		}(i, stage)
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
