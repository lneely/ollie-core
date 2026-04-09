package tui

import (
	"fmt"
	"strings"

	multiline "github.com/hymkor/go-multiline-ny"
	"github.com/mattn/go-tty"
	readline "github.com/nyaosorg/go-readline-ny"
	"github.com/nyaosorg/go-readline-ny/keys"
	ttyadapter "github.com/nyaosorg/go-ttyadapter"
	"github.com/nyaosorg/go-ttyadapter/tty8"
)

const (
	bracketedPasteEnable   = "\x1b[?2004h"
	bracketedPasteDisable  = "\x1b[?2004l"
	BracketedPasteStart    = "\x1b[200~"
	BracketedPasteEnd      = "\x1b[201~"
	maxBracketedPasteBytes = 10 << 20
)

const GoqPasteNewlineKey = keys.Code("\x1b[goq-paste-newline]")

func setupBracketedPaste(t *tty.TTY, ed *multiline.Editor) (cleanup func(), err error) {
	fmt.Fprint(t.Output(), bracketedPasteEnable)
	cleanup = func() { fmt.Fprint(t.Output(), bracketedPasteDisable) }
	ed.SetTty(NewBracketedPasteTty(&tty8.Tty{TTY: t}))
	if err = ed.BindKey(GoqPasteNewlineKey, readline.AnonymousCommand(ed.NewLine)); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}

type adaptedTty ttyadapter.Tty

type BracketedPasteTty struct {
	adaptedTty
	buf        []string
	pendingErr error
}

var _ ttyadapter.Tty = (*BracketedPasteTty)(nil)

func NewBracketedPasteTty(inner ttyadapter.Tty) *BracketedPasteTty {
	return &BracketedPasteTty{adaptedTty: inner}
}

func (t *BracketedPasteTty) GetKey() (string, error) {
	if t.pendingErr != nil && len(t.buf) == 0 {
		err := t.pendingErr
		t.pendingErr = nil
		return "", err
	}
	if len(t.buf) > 0 {
		k := t.buf[0]
		t.buf = t.buf[1:]
		return k, nil
	}

	key, err := t.adaptedTty.GetKey()
	if err != nil {
		return "", err
	}

	if !strings.Contains(key, BracketedPasteStart) {
		return key, nil
	}

	raw := key
	for !strings.Contains(raw, BracketedPasteEnd) {
		next, nextErr := t.adaptedTty.GetKey()
		if nextErr != nil {
			t.buf = append(t.buf, raw)
			t.pendingErr = nextErr
			break
		}
		if len(raw)+len(next) > maxBracketedPasteBytes {
			return raw, nil
		}
		raw += next
	}

	if t.pendingErr == nil {
		t.buf = append(t.buf, TokenizeBracketedPaste(raw)...)
	}
	if len(t.buf) > 0 {
		k := t.buf[0]
		t.buf = t.buf[1:]
		return k, nil
	}
	return t.adaptedTty.GetKey()
}

func TokenizeBracketedPaste(raw string) []string {
	var out []string
	for {
		start := strings.Index(raw, BracketedPasteStart)
		if start < 0 {
			if raw != "" {
				out = append(out, raw)
			}
			return out
		}
		if start > 0 {
			out = append(out, raw[:start])
		}
		raw = raw[start+len(BracketedPasteStart):]

		end := strings.Index(raw, BracketedPasteEnd)
		if end < 0 {
			out = append(out, BracketedPasteStart+raw)
			return out
		}

		content := raw[:end]
		out = append(out, tokenizePasteContent(content)...)
		raw = raw[end+len(BracketedPasteEnd):]
	}
}

func tokenizePasteContent(s string) []string {
	var out []string
	var buf strings.Builder

	flush := func() {
		if buf.Len() == 0 {
			return
		}
		out = append(out, buf.String())
		buf.Reset()
	}

	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\r':
			flush()
			out = append(out, string(GoqPasteNewlineKey))
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
		case '\n':
			flush()
			out = append(out, string(GoqPasteNewlineKey))
		default:
			buf.WriteByte(s[i])
		}
	}
	flush()
	return out
}
