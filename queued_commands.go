package main

import (
	"fmt"
	"strconv"
	"strings"
)

const queuedCommandUsage = "/queued [pop [N] | clear]"

type queuedCommandAction int

const (
	queuedCommandHelp queuedCommandAction = iota
	queuedCommandPop
	queuedCommandClear
)

type queuedCommand struct {
	action queuedCommandAction
	count  int
}

func parseQueuedCommandLine(input string) (cmd queuedCommand, ok bool, err error) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return queuedCommand{}, false, nil
	}
	name, args, _ := strings.Cut(input[1:], " ")
	if name != "queued" {
		return queuedCommand{}, false, nil
	}
	cmd, err = parseQueuedCommandArgs(args)
	return cmd, true, err
}

func parseQueuedCommandArgs(args string) (queuedCommand, error) {
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 {
		return queuedCommand{action: queuedCommandHelp}, nil
	}
	switch fields[0] {
	case "pop":
		if len(fields) == 1 {
			return queuedCommand{action: queuedCommandPop, count: 1}, nil
		}
		if len(fields) != 2 {
			return queuedCommand{}, fmt.Errorf("usage: %s", queuedCommandUsage)
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil || n <= 0 {
			return queuedCommand{}, fmt.Errorf("pop count must be a positive integer")
		}
		return queuedCommand{action: queuedCommandPop, count: n}, nil
	case "clear":
		if len(fields) != 1 {
			return queuedCommand{}, fmt.Errorf("usage: %s", queuedCommandUsage)
		}
		return queuedCommand{action: queuedCommandClear}, nil
	default:
		return queuedCommand{}, fmt.Errorf("usage: %s", queuedCommandUsage)
	}
}

func runQueuedCommand(queue []string, cmd queuedCommand) ([]string, string) {
	switch cmd.action {
	case queuedCommandHelp:
		return queue, fmt.Sprintf("Usage: %s", queuedCommandUsage)
	case queuedCommandPop:
		if len(queue) == 0 {
			return queue, "No queued prompts."
		}
		n := min(cmd.count, len(queue))
		return queue[:len(queue)-n], fmt.Sprintf("Removed %d queued prompt(s).", n)
	case queuedCommandClear:
		if len(queue) == 0 {
			return queue, "No queued prompts."
		}
		n := len(queue)
		return nil, fmt.Sprintf("Cleared %d queued prompt(s).", n)
	default:
		return queue, "No queued prompts."
	}
}
