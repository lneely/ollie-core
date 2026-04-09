package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"ollie/pkg/core"
)

const ctrlCExitWindow = 750 * time.Millisecond

func startSignalWatcher(appCancel context.CancelCauseFunc, c core.Core, errStream io.Writer) {
	ch := make(chan os.Signal, 16)
	signals := []os.Signal{os.Interrupt}
	if haveSIGTERM {
		signals = append(signals, SIGTERM)
	}
	signal.Notify(ch, signals...)

	go func() {
		for sig := range ch {
			switch sig {
			case SIGTERM:
				signal.Stop(ch)
				close(ch)
				fmt.Fprint(errStream, "\n(terminated)\n")
				appCancel(context.Canceled)
				continue
			case os.Interrupt:
				if c.Interrupt(core.ErrInterrupted) {
					fmt.Fprint(errStream, "\n^C\n")
				}
			}
		}
	}()
}
