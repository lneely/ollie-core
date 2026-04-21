package log

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want Level
	}{
		{"debug", LevelDebug},
		{"DEBUG", LevelDebug},
		{"info", LevelInfo},
		{"warn", LevelWarn},
		{"error", LevelError},
		{"", LevelWarn},
		{"bogus", LevelInfo},
	}
	for _, c := range cases {
		fb := LevelWarn
		if c.in == "bogus" {
			fb = LevelInfo
		}
		if got := ParseLevel(c.in, fb); got != c.want {
			t.Errorf("ParseLevel(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestEmitLevelFiltering(t *testing.T) {
	var out, errout bytes.Buffer
	l := NewWriter("test", LevelWarn, &out, &errout)

	l.Debug("should not appear")
	l.Info("should not appear")
	if out.Len() != 0 {
		t.Errorf("debug/info should be filtered, got: %s", out.String())
	}

	l.Warn("warning %d", 1)
	if errout.Len() == 0 {
		t.Error("warn should write to errout")
	}
}

func TestEmitRouting(t *testing.T) {
	var out, errout bytes.Buffer
	l := NewWriter("tag", LevelDebug, &out, &errout)

	l.Debug("d")
	l.Info("i")
	l.Warn("w")
	l.Error("e")

	if got := out.String(); got != "tag: d\ntag: i\n" {
		t.Errorf("out = %q, want debug+info", got)
	}
	if got := errout.String(); got != "tag: w\ntag: e\n" {
		t.Errorf("errout = %q, want warn+error", got)
	}
}

func TestEmitFormat(t *testing.T) {
	var out bytes.Buffer
	l := NewWriter("app", LevelDebug, &out, &out)
	l.Info("hello %s %d", "world", 42)
	if got := out.String(); got != "app: hello world 42\n" {
		t.Errorf("got %q", got)
	}
}

func TestNewWriterLevel(t *testing.T) {
	var out bytes.Buffer
	l := NewWriter("x", LevelError, &out, &out)
	l.Warn("skip")
	if out.Len() != 0 {
		t.Error("warn should be filtered at LevelError")
	}
	l.Error("show")
	if out.Len() == 0 {
		t.Error("error should appear at LevelError")
	}
}

func TestAsyncWriter(t *testing.T) {
	var buf bytes.Buffer
	aw := newAsyncWriter(&buf, 16)
	aw.Write([]byte("hello"))
	aw.Write([]byte(" world"))
	aw.flush()
	if got := buf.String(); got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestSinkFlush(t *testing.T) {
	var out, errout bytes.Buffer
	s := NewSink(&out, &errout, LevelDebug)
	l := s.Logger("t", LevelDebug)
	l.Info("hi")
	l.Error("bad")
	s.Flush()
	if !strings.Contains(out.String(), "hi") {
		t.Errorf("out = %q, want 'hi'", out.String())
	}
	if !strings.Contains(errout.String(), "bad") {
		t.Errorf("errout = %q, want 'bad'", errout.String())
	}
}

func TestSinkNewLogger(t *testing.T) {
	var out bytes.Buffer
	s := NewSink(&out, &out, LevelWarn)
	t.Setenv("OLLIE_MYTEST_LOG", "debug")
	l := s.NewLogger("mytest")
	l.Debug("visible")
	s.Flush()
	if !strings.Contains(out.String(), "visible") {
		t.Errorf("expected debug output with env override, got: %s", out.String())
	}
}

func TestSinkNewLoggerDefault(t *testing.T) {
	var out bytes.Buffer
	s := NewSink(&out, &out, LevelError)
	t.Setenv("OLLIE_NOTSET_LOG", "")
	l := s.NewLogger("notset")
	l.Warn("skip")
	s.Flush()
	if out.Len() != 0 {
		t.Errorf("expected no output at LevelError, got: %s", out.String())
	}
}
