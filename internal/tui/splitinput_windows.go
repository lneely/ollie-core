//go:build windows

package tui

import "io"

type splitInput struct{}

func newSplitInput(_ interface{}, _ io.Writer, _ string, _ func([]string) ([]string, []string)) *splitInput {
	return &splitInput{}
}
func (s *splitInput) Enter() io.Writer         { return nil }
func (s *splitInput) Exit() ([]string, string) { return nil, "" }
func (s *splitInput) SetPrompt(_ string)       {}
func (s *splitInput) PopQueue() (string, bool) { return "", false }
func (s *splitInput) EchoQueuedInput(_ string) {}
func (s *splitInput) runQueuedCommand(cmd queuedCommand) string {
	_, msg := runQueuedCommand(nil, cmd)
	return msg
}
