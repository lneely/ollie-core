package main

import (
	"fmt"
	"strconv"
	"strings"
)

const interruptCommandUsage = "/interrupt <prompt>"

func parseInterruptCommandLine(input string) (prompt string, ok bool, err error) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return "", false, nil
	}
	name, args, _ := strings.Cut(input[1:], " ")
	if name != "interrupt" {
		return "", false, nil
	}
	prompt, err = parseInterruptCommandArgs(args)
	return prompt, true, err
}

func parseInterruptCommandArgs(args string) (string, error) {
	prompt := strings.TrimSpace(args)
	if prompt == "" {
		return "", fmt.Errorf("usage: %s", interruptCommandUsage)
	}
	if unquoted, err := strconv.Unquote(prompt); err == nil {
		prompt = strings.TrimSpace(unquoted)
	} else if len(prompt) >= 2 && prompt[0] == '\'' && prompt[len(prompt)-1] == '\'' {
		prompt = strings.TrimSpace(prompt[1 : len(prompt)-1])
	}
	if prompt == "" {
		return "", fmt.Errorf("usage: %s", interruptCommandUsage)
	}
	return prompt, nil
}

func interruptQueuedMessage(prompt string) string {
	return "◇ interrupt queued: " + prompt
}

func interruptExpiredMessage(count int) string {
	return fmt.Sprintf("◇ %d interrupt(s) expired (turn ended)", count)
}

func interruptQueueFullMessage() string {
	return "Interrupt queue is full."
}
