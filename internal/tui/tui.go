package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	multiline "github.com/hymkor/go-multiline-ny"
	gotty "github.com/mattn/go-tty"
	readline "github.com/nyaosorg/go-readline-ny"

	"ollie/pkg/core"
)

// recentHistory implements readline.IHistory for the multiline editor.
type recentHistory struct {
	entries []string
}

var _ readline.IHistory = (*recentHistory)(nil)

func (h *recentHistory) Len() int { return len(h.entries) }
func (h *recentHistory) At(i int) string {
	if i >= 0 && i < len(h.entries) {
		return h.entries[i]
	}
	return ""
}

// TUI is the terminal frontend. It owns all TUI state and drives a Core.
type TUI struct {
	core    core.Core
	split   *splitInput
	history recentHistory
}

// New creates a TUI backed by the given Core.
func New(c core.Core) *TUI {
	return &TUI{core: c}
}

// Run starts the interactive readline loop, blocking until the user exits.
func (t *TUI) Run(ctx context.Context) {
	tt, err := gotty.Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tty:", err)
		return
	}
	defer tt.Close()

	var ed multiline.Editor
	restorePaste, err := setupBracketedPaste(tt, &ed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bracketed paste:", err)
	} else {
		defer restorePaste()
	}

	ed.SetPrompt(func(w io.Writer, lnum int) (int, error) {
		if lnum == 0 {
			return fmt.Fprint(w, t.core.Prompt())
		}
		return fmt.Fprint(w, "... ")
	})

	ed.SubmitOnEnterWhen(func(lines []string, _ int) bool {
		if len(lines) <= 1 {
			return true
		}
		return !strings.HasSuffix(strings.TrimSpace(lines[len(lines)-1]), "\\")
	})

	ed.SetHistory(&t.history)
	ed.SetHistoryCycling(true)

	t.split = newSplitInput(tt, tt.Output(), t.core.Prompt(), nil)

	appCtx, appCancel := context.WithCancelCause(ctx)
	startSignalWatcher(appCancel, t.core, os.Stderr)

	var lastCtrlC time.Time
	firstRead := true

	for appCtx.Err() == nil {
		if firstRead {
			firstRead = false
			if _, h, err := tt.Size(); err == nil && h > 0 {
				clearScreenAndMoveToBottom(tt.Output(), h)
			}
		}

		lines, err := ed.Read(appCtx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			errs := err.Error()
			if errs == "interrupted" || errs == "^C" {
				now := time.Now()
				if !lastCtrlC.IsZero() && now.Sub(lastCtrlC) <= ctrlCExitWindow {
					break
				}
				lastCtrlC = now
				fmt.Fprint(os.Stderr, "^C (press Ctrl-C again to exit)\n")
				continue
			}
			fmt.Fprintln(os.Stderr, "input error:", err)
			break
		}

		input := strings.Join(lines, "\n")
		lastCtrlC = time.Time{}

		if strings.TrimSpace(input) == "" {
			continue
		}

		t.history.entries = append(t.history.entries, input)

		if len(lines) == 1 {
			if w, _, err := tt.Size(); err == nil {
				prompt := t.core.Prompt()
				if shouldRerenderSubmittedSingleLine(prompt, input, w) {
					rerenderSubmittedSingleLine(tt.Output(), prompt, input)
				}
			}
		}

		t.processInputWithSplit(appCtx, input, &ed)
	}
}

func (t *TUI) processInputWithSplit(ctx context.Context, input string, ed *multiline.Editor) {
	// /queued is TUI-specific: it manipulates the split input queue.
	if cmd, ok, err := parseQueuedCommandLine(input); ok {
		var msg string
		if err != nil {
			msg = err.Error()
		} else if t.split == nil {
			_, msg = runQueuedCommand(nil, cmd)
		} else {
			msg = t.split.runQueuedCommand(cmd)
		}
		fmt.Fprintln(os.Stdout, msg)
		return
	}

	if t.split == nil {
		t.core.Submit(ctx, input, MakeOutputFn(os.Stdout))
		return
	}

	t.split.SetPrompt(t.core.Prompt())
	wrapper := t.split.Enter()
	out := io.Writer(os.Stdout)
	if wrapper != nil {
		out = wrapper
	}

	handler := MakeOutputFn(out)
	t.core.Submit(ctx, input, handler)

	for ctx.Err() == nil {
		q, ok := t.split.PopQueue()
		if !ok {
			break
		}
		t.split.SetPrompt(t.core.Prompt())
		t.split.EchoQueuedInput(q)
		t.history.entries = append(t.history.entries, q)
		t.core.Submit(ctx, q, handler)
	}

	_, pending := t.split.Exit()
	if pending != "" {
		ed.SetDefault([]string{pending})
	}
}

// MakeOutputFn returns an EventHandler that renders events as text to out.
// This is the TUI's bridge from typed events to terminal output.
func MakeOutputFn(out io.Writer) core.EventHandler {
	return func(em core.Event) {
		switch em.Role {
		case "assistant":
			fmt.Fprint(out, em.Content)
		case "call":
			args := squashWhitespace(em.Content)
			if len(args) > 500 {
				args = args[:500] + "..."
			}
			fmt.Fprintf(out, "-> %s(%s)\n", em.Name, args)
		case "tool":
			s := strings.TrimRight(em.Content, "\n")
			if len(s) > 500 {
				s = s[:500] + "..."
			}
			fmt.Fprintf(out, "= %s\n", s)
		case "retry":
			fmt.Fprintf(out, "retrying in %ss...\n", em.Content)
		case "error":
			fmt.Fprintf(out, "error: %s\n", em.Content)
		case "stalled":
			fmt.Fprintln(out, "agent stalled")
		case "info":
			fmt.Fprint(out, em.Content)
		case "newline":
			fmt.Fprintln(out)
		}
	}
}

func squashWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
