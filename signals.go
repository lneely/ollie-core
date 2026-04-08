package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"
)

var ErrInterrupted = errors.New("interrupted")

const ctrlCExitWindow = 750 * time.Millisecond

type ActionCanceler func() context.CancelCauseFunc

func startSignalWatcher(appCancel context.CancelCauseFunc, getActionCancel ActionCanceler, errStream io.Writer) {
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
				if cancel := getActionCancel(); cancel != nil {
					fmt.Fprint(errStream, "\n^C\n")
					cancel(ErrInterrupted)
					continue
				}
			}
		}
	}()
}
