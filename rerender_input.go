package main

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/mattn/go-runewidth"
)

var ansiCSIPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")
var ansiOSCPattern = regexp.MustCompile("\x1b\\][^\x07]*(\x07|\x1b\\\\)")

func visibleWidth(s string) int {
	s = ansiOSCPattern.ReplaceAllString(s, "")
	s = ansiCSIPattern.ReplaceAllString(s, "")
	return runewidth.StringWidth(s)
}

func shouldRerenderSubmittedSingleLine(prompt, input string, termWidth int) bool {
	if termWidth <= 0 {
		return false
	}
	return visibleWidth(prompt)+runewidth.StringWidth(input) > termWidth
}

func rerenderSubmittedSingleLine(w io.Writer, prompt, input string) {
	fmt.Fprint(w, "\x1b[1A\r\x1b[2K")
	fmt.Fprint(w, prompt)
	fmt.Fprint(w, input)
	fmt.Fprint(w, "\n")
}

func clearScreenAndMoveToBottom(w io.Writer, termHeight int) {
	if termHeight <= 0 {
		return
	}
	fmt.Fprint(w, strings.Repeat("\r\n", termHeight))
	fmt.Fprintf(w, "\x1b[%d;1H", termHeight)
}

func truncateToWidth(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	w := 0
	for i, r := range s {
		w += runewidth.RuneWidth(r)
		if w > maxWidth {
			return s[:i]
		}
	}
	return s
}

func truncateToWidthFromEnd(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	runes := []rune(s)
	w := 0
	start := len(runes)
	for start > 0 {
		nextWidth := runewidth.RuneWidth(runes[start-1])
		if w+nextWidth > maxWidth {
			break
		}
		start--
		w += nextWidth
	}
	return string(runes[start:])
}
