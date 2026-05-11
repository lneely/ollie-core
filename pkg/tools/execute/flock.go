package execute

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// lockClass classifies a CodeStep by its concurrency profile.
type lockClass int

const (
	lockClassRead   lockClass = iota // file_read, file_glob, file_grep — safe to parallelize
	lockClassWrite                   // file_write, file_edit — exclusive per path
	lockClassGlobal                  // inline code, bash, unknown tools — global serialize
)

// classifyStep returns the lock class for a single CodeStep.
// Inline code, parallel groups, and elevated steps are always lockClassGlobal.
// Tool steps are classified by reading the script and checking for an
// ollie:parallel annotation in the first 10 lines (see detectParallelClass).
// Unknown or unreadable tools default to lockClassGlobal.
func classifyStep(step CodeStep) lockClass {
	if step.Code != "" || len(step.Parallel) > 0 || step.Elevated || step.Tool == "" {
		return lockClassGlobal
	}
	code, err := ReadTool(step.Tool)
	if err != nil {
		return lockClassGlobal
	}
	return detectParallelClass(code)
}

// detectParallelClass scans the first 10 lines of a tool script for an
// ollie:parallel annotation. The annotation is comment-syntax-agnostic:
// it matches "ollie:parallel read" or "ollie:parallel write" anywhere in
// the line, so it works with # (bash/python), -- (lua), // (go), etc.
//
// Absence of the annotation → lockClassGlobal (serialize).
func detectParallelClass(code string) lockClass {
	lines := strings.SplitN(code, "\n", 11)
	if len(lines) > 10 {
		lines = lines[:10]
	}
	for _, line := range lines {
		idx := strings.Index(line, "ollie:parallel")
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(line[idx+len("ollie:parallel"):])
		switch {
		case rest == "read" || strings.HasPrefix(rest, "read "):
			return lockClassRead
		case rest == "write" || strings.HasPrefix(rest, "write "):
			return lockClassWrite
		}
	}
	return lockClassGlobal
}

// acquireFlock opens (or creates) a lock file in dir named name and acquires
// LOCK_SH (exclusive=false) or LOCK_EX (exclusive=true).
// Returns nil, nil when dir is empty (locking disabled).
// Caller must Close the returned file to release the lock.
func acquireFlock(dir, name string, exclusive bool) (*os.File, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	path := filepath.Join(dir, sanitizeLockName(name)+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// IsParallelRead implements tools.ParallelClassifier. Returns true when the
// named tool script carries an "ollie:parallel read" annotation.
func (e *Server) IsParallelRead(name string) bool {
	code, err := ReadTool(name)
	if err != nil {
		return false
	}
	return detectParallelClass(code) == lockClassRead
}

func sanitizeLockName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '<', '>', '|', '"', ' ':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	name := b.String()
	if len(name) > 64 {
		name = name[:64]
	}
	if name == "" {
		return "unnamed"
	}
	return name
}
