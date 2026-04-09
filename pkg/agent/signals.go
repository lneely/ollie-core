package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"
)

const CtrlCExitWindow = 750 * time.Millisecond

// WatchSignals installs OS signal handlers for the lifetime of the program.
// SIGTERM cancels the app context; SIGINT interrupts the current agent turn.
// It is safe to call from any frontend (TUI, HTTP, CLI one-shot, etc.).
func WatchSignals(appCancel context.CancelCauseFunc, c Core, errStream io.Writer) {
	ch := make(chan os.Signal, 16)
	signals := []os.Signal{os.Interrupt}
	if haveSIGTERM {
		signals = append(signals, sigTERM)
	}
	signal.Notify(ch, signals...)

	go func() {
		for sig := range ch {
			switch sig {
			case sigTERM:
				signal.Stop(ch)
				close(ch)
				fmt.Fprint(errStream, "\n(terminated)\n")
				appCancel(context.Canceled)
				continue
			case os.Interrupt:
				if c.Interrupt(ErrInterrupted) {
					fmt.Fprint(errStream, "\n^C\n")
				}
			}
		}
	}()
}
