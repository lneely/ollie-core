//go:build !windows

package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	gotty "github.com/mattn/go-tty"
)

const splitInputVisibleQueueRows = 3

type splitInputReadRuneFunc func() (rune, error)

// splitInput manages the persistent input line at the bottom of the terminal
// during agent turns. It uses ANSI scroll regions to pin a bottom band while
// output scrolls above.
type splitInput struct {
	mainTTY  *gotty.TTY
	out      io.Writer
	mu       sync.Mutex
	prompt   string
	complete func(fields []string) (completionSet []string, listingSet []string)

	width, height int
	scrollEnd     int
	bandStart     int

	active        bool
	inputTTY      *gotty.TTY
	buf           []rune
	queue         []string
	completions   []string
	outputStarted bool
	deferredLFs   int

	winchCh chan os.Signal
	done    chan struct{}

	pulseStart time.Time
}

func newSplitInput(t *gotty.TTY, out io.Writer, prompt string, complete func(fields []string) ([]string, []string)) *splitInput {
	return &splitInput{
		mainTTY:  t,
		out:      out,
		prompt:   prompt,
		complete: complete,
	}
}

func (s *splitInput) maxQueueRows() int {
	rows := max(s.height/3, 1)
	if rows > splitInputVisibleQueueRows {
		return splitInputVisibleQueueRows
	}
	return rows
}

func (s *splitInput) reservedRowsLocked() int {
	rows := 2
	bodyRows := s.promptBodyRowsLocked() + len(s.completions)
	if bodyRows > 0 {
		rows += 1 + bodyRows
	}
	if rows > s.height-2 {
		rows = s.height - 2
	}
	if rows < 2 {
		rows = 2
	}
	return rows
}

func (s *splitInput) recalcLayoutLocked() {
	s.scrollEnd = max(s.height-s.reservedRowsLocked(), 1)
	s.bandStart = s.scrollEnd + 1
}

func (s *splitInput) promptBodyRowsLocked() int {
	return len(s.queue)
}

func resizeRedrawStart(oldHeight, oldBandStart, newHeight, newBandStart int) int {
	if newBandStart < 1 {
		newBandStart = 1
	}
	if oldHeight <= 0 || oldBandStart <= 0 || newHeight <= 0 {
		return newBandStart
	}
	oldReserved := max(oldHeight-oldBandStart+1, 1)
	shiftedOldBandStart := min(max(newHeight-oldReserved+1, 1), newHeight)
	return min(shiftedOldBandStart, newBandStart)
}

func (s *splitInput) redrawLayoutLocked(clearFrom int) {
	if clearFrom < 1 {
		clearFrom = 1
	}
	fmt.Fprintf(s.out, "\033[1;%dr", s.scrollEnd)
	for row := clearFrom; row <= s.height; row++ {
		fmt.Fprintf(s.out, "\033[%d;1H\033[2K", row)
	}
	s.drawBottomAreaLocked()
	fmt.Fprintf(s.out, "\033[%d;1H", s.scrollEnd)
}

func (s *splitInput) prepareSubmittedPromptGapLocked() {
	if s.height <= 0 {
		return
	}
	fmt.Fprintf(s.out, "\033[%d;1H\n", s.height)
}

func (s *splitInput) Enter() io.Writer {
	w, h, err := s.mainTTY.Size()
	if err != nil || h < 6 {
		return nil
	}

	inputTTY, err := gotty.Open()
	if err != nil {
		return nil
	}

	s.mu.Lock()
	s.width = w
	s.height = h
	s.active = true
	s.inputTTY = inputTTY
	s.buf = s.buf[:0]
	s.queue = s.queue[:0]
	s.completions = nil
	s.outputStarted = false
	s.deferredLFs = 0
	s.pulseStart = time.Now()
	s.done = make(chan struct{})
	s.recalcLayoutLocked()

	fmt.Fprint(s.out, "\033[?25l")
	s.prepareSubmittedPromptGapLocked()
	s.redrawLayoutLocked(s.bandStart)
	s.mu.Unlock()

	s.winchCh = make(chan os.Signal, 1)
	signal.Notify(s.winchCh, syscall.SIGWINCH)
	go s.watchResize()
	go s.captureKeys(inputTTY)
	go s.pulseSeparator()

	return &splitWriter{s: s}
}

func (s *splitInput) Exit() (queued []string, pending string) {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return nil, ""
	}
	s.active = false
	close(s.done)

	inputTTY := s.inputTTY
	s.inputTTY = nil

	queued = s.queue
	s.queue = nil
	s.completions = nil
	if len(s.buf) > 0 {
		pending = string(s.buf)
		s.buf = s.buf[:0]
	}
	s.flushDeferredLFsForExitLocked()

	fmt.Fprint(s.out, "\033[r")
	s.clearBandLocked()
	fmt.Fprint(s.out, "\033[?25h")
	fmt.Fprintf(s.out, "\033[%d;1H", s.height)

	s.mu.Unlock()

	if inputTTY != nil {
		inputTTY.Close()
	}
	signal.Stop(s.winchCh)

	return queued, pending
}

func (s *splitInput) SetPrompt(prompt string) {
	s.mu.Lock()
	s.prompt = prompt
	if s.active {
		fmt.Fprint(s.out, "\033[s")
		s.clearBandLocked()
		s.drawBottomAreaLocked()
		s.restoreOverlayCursorLocked(s.height)
	}
	s.mu.Unlock()
}

func (s *splitInput) PopQueue() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return "", false
	}

	item := s.queue[0]
	s.queue = s.queue[1:]
	s.completions = nil
	oldBandStart := s.bandStart
	s.recalcLayoutLocked()
	s.redrawLayoutLocked(min(oldBandStart, s.bandStart))

	return item, true
}

func (s *splitInput) runQueuedCommand(cmd queuedCommand) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cmdMsg string
	s.queue, cmdMsg = runQueuedCommand(s.queue, cmd)
	if s.active {
		oldBandStart := s.bandStart
		s.completions = nil
		s.recalcLayoutLocked()
		s.redrawLayoutLocked(min(oldBandStart, s.bandStart))
	}
	return cmdMsg
}

func (s *splitInput) EchoQueuedInput(input string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}
	s.flushDeferredLFsLocked()
	fmt.Fprintf(s.out, "%s%s\n", s.prompt, input)
	s.completions = nil
	s.outputStarted = false
}

func (s *splitInput) clearBandLocked() {
	for row := s.bandStart; row <= s.height; row++ {
		fmt.Fprintf(s.out, "\033[%d;1H\033[2K", row)
	}
}

func (s *splitInput) renderPulseSepLocked() string {
	const (
		baseGray = 238
		peakGray = 252
		sigma    = 3.0
	)
	period := 14.0 * float64(s.width-1) / 139.0
	phase := time.Since(s.pulseStart).Seconds() * 2 * math.Pi / period
	pos := (math.Sin(phase) + 1) / 2 * float64(s.width-1)

	var b strings.Builder
	for i := range s.width {
		d := float64(i) - pos
		gray := baseGray + int(float64(peakGray-baseGray)*math.Exp(-d*d/(2*sigma*sigma)))
		fmt.Fprintf(&b, "\033[38;5;%dm─", gray)
	}
	b.WriteString("\033[0m")
	return b.String()
}

func (s *splitInput) redrawBottomSepLocked() {
	sepRow := s.height - 1
	if sepRow < s.bandStart {
		return
	}
	fmt.Fprintf(s.out, "\033[s\033[%d;1H\033[2K%s", sepRow, s.renderPulseSepLocked())
	s.restoreOverlayCursorLocked(sepRow)
}

func (s *splitInput) drawBottomAreaLocked() {
	h := s.height
	inputRow := h
	queueRows := len(s.queue)
	bodyRows := queueRows + len(s.completions)

	pulseSep := s.renderPulseSepLocked()
	if bodyRows == 0 {
		sepRow := inputRow - 1
		if sepRow >= s.bandStart {
			fmt.Fprintf(s.out, "\033[%d;1H\033[2K%s", sepRow, pulseSep)
		}
	} else {
		topSepRow := inputRow - bodyRows - 2
		bottomSepRow := inputRow - 1
		if topSepRow >= s.bandStart {
			sep := strings.Repeat("─", s.width)
			fmt.Fprintf(s.out, "\033[%d;1H\033[2K\033[2m%s\033[0m", topSepRow, sep)
		}

		row := inputRow - bodyRows - 1
		for i, q := range s.queue {
			currentRow := row + i
			if currentRow >= s.bandStart {
				display := truncateToWidth(q, s.width-4)
				fmt.Fprintf(s.out, "\033[%d;1H\033[2K\033[2m ◆ %s\033[0m", currentRow, display)
			}
		}
		row += queueRows
		for i, item := range s.completions {
			currentRow := row + i
			if currentRow >= s.bandStart {
				display := truncateToWidth(item, s.width)
				fmt.Fprintf(s.out, "\033[%d;1H\033[2K%s", currentRow, display)
			}
		}
		if bottomSepRow >= s.bandStart {
			fmt.Fprintf(s.out, "\033[%d;1H\033[2K%s", bottomSepRow, pulseSep)
		}
	}

	fmt.Fprintf(s.out, "\033[%d;1H\033[2K%s", inputRow, s.renderInputBufferLocked())
}

func (s *splitInput) captureKeys(inputTTY *gotty.TTY) {
	if inputTTY == nil {
		return
	}
	var pendingTokens []string
	for {
		token, rest, err := readSplitInputToken(inputTTY.ReadRune, pendingTokens)
		pendingTokens = rest
		if err != nil {
			return
		}
		if token == "" {
			continue
		}

		select {
		case <-s.done:
			return
		default:
		}

		s.mu.Lock()
		if !s.active {
			s.mu.Unlock()
			return
		}

		switch {
		case token == "\t":
			s.completeLocked()

		case token == "\r" || token == "\n":
			if len(s.buf) > 0 && s.handleLocalCommandLocked(strings.TrimSpace(string(s.buf))) {
				break
			}
			if len(s.buf) > 0 && len(s.queue) < s.maxQueueRows() {
				oldBandStart := s.bandStart
				s.completions = nil
				s.queue = append(s.queue, string(s.buf))
				s.buf = s.buf[:0]
				s.recalcLayoutLocked()
				s.redrawLayoutLocked(min(oldBandStart, s.bandStart))
			}

		case token == "\x7f" || token == "\b":
			if len(s.buf) > 0 {
				oldBandStart := s.bandStart
				cleared := s.clearCompletionsLocked()
				s.buf = s.buf[:len(s.buf)-1]
				if cleared {
					s.recalcLayoutLocked()
					s.redrawLayoutLocked(min(oldBandStart, s.bandStart))
				} else {
					s.redrawInputLineLocked()
				}
			}

		case token == "\x03": // Ctrl-C — signal handler deals with it

		case token == "\x15":
			if len(s.buf) > 0 {
				oldBandStart := s.bandStart
				cleared := s.clearCompletionsLocked()
				s.buf = s.buf[:0]
				if cleared {
					s.recalcLayoutLocked()
					s.redrawLayoutLocked(min(oldBandStart, s.bandStart))
				} else {
					s.redrawInputLineLocked()
				}
			}

		case token == "\x17":
			oldBandStart := s.bandStart
			cleared := s.clearCompletionsLocked()
			s.deleteWordLocked()
			if cleared {
				s.recalcLayoutLocked()
				s.redrawLayoutLocked(min(oldBandStart, s.bandStart))
			} else {
				s.redrawInputLineLocked()
			}

		default:
			if isPrintableSplitInputToken(token) {
				oldBandStart := s.bandStart
				cleared := s.clearCompletionsLocked()
				s.buf = append(s.buf, []rune(token)...)
				if cleared {
					s.recalcLayoutLocked()
					s.redrawLayoutLocked(min(oldBandStart, s.bandStart))
				} else {
					s.redrawInputLineLocked()
				}
			}
		}
		s.mu.Unlock()
	}
}

func readSplitInputToken(readRune splitInputReadRuneFunc, pending []string) (string, []string, error) {
	if len(pending) > 0 {
		return pending[0], pending[1:], nil
	}

	token, err := readSplitInputRawToken(readRune)
	if err != nil {
		return "", nil, err
	}
	if token != BracketedPasteStart {
		return token, nil, nil
	}

	raw := token
	for !strings.Contains(raw, BracketedPasteEnd) {
		next, err := readSplitInputRawToken(readRune)
		if err != nil {
			return "", nil, err
		}
		if len(raw)+len(next) > maxBracketedPasteBytes {
			return raw, nil, nil
		}
		raw += next
	}

	tokens := tokenizeSplitInputPaste(raw)
	if len(tokens) == 0 {
		return "", nil, nil
	}
	return tokens[0], tokens[1:], nil
}

func readSplitInputRawToken(readRune splitInputReadRuneFunc) (string, error) {
	r, err := readRune()
	if err != nil {
		return "", err
	}
	if r != '\x1b' {
		return string(r), nil
	}

	var b strings.Builder
	b.WriteRune(r)

	next, err := readRune()
	if err != nil {
		return b.String(), nil
	}
	b.WriteRune(next)
	if next != '[' && next != 'O' {
		return b.String(), nil
	}

	for {
		r, err := readRune()
		if err != nil {
			return b.String(), nil
		}
		b.WriteRune(r)
		if r >= 0x40 && r <= 0x7e {
			return b.String(), nil
		}
	}
}

func tokenizeSplitInputPaste(raw string) []string {
	tokens := TokenizeBracketedPaste(raw)
	out := make([]string, 0, len(tokens))
	needSpace := false
	for _, token := range tokens {
		if token == string(GoqPasteNewlineKey) {
			needSpace = true
			continue
		}
		token = normalizeSplitInputPasteText(token)
		if token == "" {
			continue
		}
		if needSpace {
			if len(out) > 0 && !strings.HasSuffix(out[len(out)-1], " ") && !strings.HasPrefix(token, " ") {
				out = append(out, " ")
			}
			needSpace = false
		}
		out = append(out, token)
	}
	return out
}

func normalizeSplitInputPasteText(token string) string {
	var b strings.Builder
	for _, r := range token {
		switch r {
		case '\t', '\v', '\f':
			b.WriteByte(' ')
		default:
			if r < 32 || r == utf8.RuneError {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isPrintableSplitInputToken(token string) bool {
	if token == "" {
		return false
	}
	for _, r := range token {
		if r < 32 || r == utf8.RuneError {
			return false
		}
	}
	return true
}

func (s *splitInput) deleteWordLocked() {
	for len(s.buf) > 0 && s.buf[len(s.buf)-1] == ' ' {
		s.buf = s.buf[:len(s.buf)-1]
	}
	for len(s.buf) > 0 && s.buf[len(s.buf)-1] != ' ' {
		s.buf = s.buf[:len(s.buf)-1]
	}
}

func (s *splitInput) redrawInputLineLocked() {
	fmt.Fprintf(s.out, "\033[s\033[%d;1H\033[2K%s", s.height, s.renderInputBufferLocked())
	s.restoreOverlayCursorLocked(s.height)
}

func (s *splitInput) restoreOverlayCursorLocked(row int) {
	if row < 1 {
		row = 1
	}
	fmt.Fprintf(s.out, "\033[%d;1H\033[u", row)
}

func (s *splitInput) clearCompletionsLocked() bool {
	if len(s.completions) == 0 {
		return false
	}
	s.completions = nil
	return true
}

func (s *splitInput) completeLocked() {
	if s.complete == nil {
		return
	}

	buf := string(s.buf)
	fields := completionFieldsForBuffer(buf)
	if len(fields) == 0 {
		return
	}

	completionSet, listingSet := s.complete(fields)
	prefixStart := lastTokenStart(buf)
	prefix := buf[prefixStart:]
	matches := filterCompletionMatches(completionSet, prefix)
	if len(matches) == 0 {
		return
	}

	if len(matches) == 1 {
		s.setCompletionTokenLocked(prefixStart, matches[0], true)
		return
	}

	common := longestCommonPrefix(matches)
	if len(common) > len(prefix) {
		s.setCompletionTokenLocked(prefixStart, common, false)
		return
	}

	listing := filterCompletionMatches(listingSet, prefix)
	if len(listing) == 0 {
		listing = matches
	}
	s.showCompletionListingLocked(listing)
}

func (s *splitInput) setCompletionTokenLocked(prefixStart int, replacement string, appendSpace bool) {
	oldBandStart := s.bandStart
	s.completions = nil
	if appendSpace && replacement != "" && !strings.HasSuffix(replacement, "/") && !strings.HasSuffix(replacement, " ") {
		replacement += " "
	}
	s.buf = []rune(string(s.buf)[:prefixStart] + replacement)
	s.recalcLayoutLocked()
	s.redrawLayoutLocked(min(oldBandStart, s.bandStart))
}

func (s *splitInput) showCompletionListingLocked(listing []string) {
	if len(listing) == 0 {
		return
	}
	oldBandStart := s.bandStart
	s.completions = s.limitCompletionListingLocked(listing)
	s.recalcLayoutLocked()
	s.redrawLayoutLocked(min(oldBandStart, s.bandStart))
}

func (s *splitInput) limitCompletionListingLocked(listing []string) []string {
	maxRows := s.height - 5 - s.promptBodyRowsLocked()
	if maxRows <= 0 {
		return nil
	}
	if maxRows > 10 {
		maxRows = 10
	}
	if len(listing) <= maxRows {
		return append([]string(nil), listing...)
	}
	if maxRows == 1 {
		return []string{"..."}
	}
	limited := append([]string(nil), listing[:maxRows-1]...)
	return append(limited, "...")
}

func (s *splitInput) watchResize() {
	for {
		select {
		case <-s.done:
			return
		case _, ok := <-s.winchCh:
			if !ok {
				return
			}
			s.handleResize()
		}
	}
}

func (s *splitInput) pulseSeparator() {
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.mu.Lock()
			if !s.active {
				s.mu.Unlock()
				return
			}
			s.redrawBottomSepLocked()
			s.mu.Unlock()
		}
	}
}

func (s *splitInput) handleResize() {
	w, h, err := s.mainTTY.Size()
	if err != nil || h < 6 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}

	oldHeight := s.height
	oldBandStart := s.bandStart
	s.width = w
	s.height = h

	s.recalcLayoutLocked()
	s.redrawLayoutLocked(resizeRedrawStart(oldHeight, oldBandStart, s.height, s.bandStart))
}

type splitWriter struct {
	s *splitInput
}

func (w *splitWriter) Write(p []byte) (int, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	if len(w.s.completions) > 0 {
		oldBandStart := w.s.bandStart
		w.s.completions = nil
		w.s.recalcLayoutLocked()
		w.s.redrawLayoutLocked(min(oldBandStart, w.s.bandStart))
	}
	if len(p) > 0 && !w.s.outputStarted {
		w.s.writeScrollbackSeparatorLocked()
		w.s.outputStarted = true
	}
	w.s.flushDeferredLFsLocked()
	trailingLFs := trailingLFCount(p)
	body := p[:len(p)-trailingLFs]
	if len(body) > 0 {
		if _, err := w.s.out.Write(body); err != nil {
			return 0, err
		}
	}
	w.s.deferredLFs += trailingLFs
	return len(p), nil
}

func (s *splitInput) writeScrollbackSeparatorLocked() {
	sep := strings.Repeat("─", s.width)
	fmt.Fprintf(s.out, "\n\033[2m%s\033[0m\n", sep)
}

func (s *splitInput) flushDeferredLFsLocked() {
	if s.deferredLFs == 0 {
		return
	}
	fmt.Fprint(s.out, strings.Repeat("\n", s.deferredLFs))
	s.deferredLFs = 0
}

func (s *splitInput) flushDeferredLFsForExitLocked() {
	if s.deferredLFs <= 1 {
		s.deferredLFs = 0
		return
	}
	fmt.Fprint(s.out, strings.Repeat("\n", s.deferredLFs-1))
	s.deferredLFs = 0
}

func trailingLFCount(p []byte) int {
	n := 0
	for i := len(p) - 1; i >= 0 && p[i] == '\n'; i-- {
		n++
	}
	return n
}

func (s *splitInput) renderInputBufferLocked() string {
	promptWidth := visibleWidth(s.prompt)
	maxInputWidth := s.width - promptWidth - 1
	display := truncateToWidthFromEnd(string(s.buf), maxInputWidth)
	return s.prompt + display + "\033[7m \033[0m"
}

func (s *splitInput) handleLocalCommandLocked(input string) bool {
	return s.handleQueuedCommandLocked(input)
}

func (s *splitInput) handleQueuedCommandLocked(input string) bool {
	cmd, ok, err := parseQueuedCommandLine(input)
	if !ok {
		return false
	}

	oldBandStart := s.bandStart
	s.buf = s.buf[:0]
	s.completions = nil
	if err != nil {
		s.recalcLayoutLocked()
		s.redrawLayoutLocked(min(oldBandStart, s.bandStart))
		s.writeLocalStatusMessageLocked(err.Error())
		return true
	}

	s.queue, input = runQueuedCommand(s.queue, cmd)
	s.recalcLayoutLocked()
	s.redrawLayoutLocked(min(oldBandStart, s.bandStart))
	s.writeLocalStatusMessageLocked(input)
	return true
}

func (s *splitInput) writeLocalStatusMessageLocked(msg string) {
	if msg == "" {
		return
	}
	s.flushDeferredLFsLocked()
	for line := range strings.SplitSeq(msg, "\n") {
		fmt.Fprintf(s.out, "\033[2m%s\033[0m\n", line)
	}
}


// --- completion helpers ---

func completionFieldsForBuffer(buf string) []string {
	fields := splitFields(buf)
	if len(buf) == 0 {
		return fields
	}
	if containsByte(" \t\r\n\v\f", buf[len(buf)-1]) {
		return append(fields, "")
	}
	return fields
}

func lastTokenStart(buf string) int {
	for i := len(buf) - 1; i >= 0; i-- {
		if containsByte(" \t\r\n\v\f", buf[i]) {
			return i + 1
		}
	}
	return 0
}

func filterCompletionMatches(candidates []string, prefix string) []string {
	if len(candidates) == 0 {
		return nil
	}
	if prefix == "" {
		return append([]string(nil), candidates...)
	}
	matches := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, prefix) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) > 0 {
		return matches
	}
	lowerPrefix := strings.ToLower(prefix)
	for _, candidate := range candidates {
		if strings.HasPrefix(strings.ToLower(candidate), lowerPrefix) {
			matches = append(matches, candidate)
		}
	}
	return matches
}

func longestCommonPrefix(values []string) string {
	if len(values) == 0 {
		return ""
	}
	prefix := values[0]
	for _, value := range values[1:] {
		for !strings.HasPrefix(value, prefix) && prefix != "" {
			prefix = prefix[:len(prefix)-1]
		}
		if prefix == "" {
			return ""
		}
	}
	return prefix
}

func splitFields(line string) []string {
	const spaces = " \t\r\n\v\f"
	var fields []string
	for len(line) > 0 {
		i := 0
		for i < len(line) && containsByte(spaces, line[i]) {
			i++
		}
		line = line[i:]
		if len(line) == 0 {
			break
		}
		i = 0
		for i < len(line) && !containsByte(spaces, line[i]) {
			i++
		}
		fields = append(fields, line[:i])
		line = line[i:]
	}
	return fields
}

func containsByte(set string, b byte) bool {
	return strings.IndexByte(set, b) >= 0
}
