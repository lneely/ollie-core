package log

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Level controls which messages are emitted.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// ParseLevel converts a string to a Level, returning fallback if unrecognized.
func ParseLevel(s string, fallback Level) Level {
	switch strings.ToLower(s) {
	case "debug":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return fallback
	}
}

// asyncWriter wraps an io.Writer with a buffered channel so writes never block.
type asyncWriter struct {
	ch   chan []byte
	done chan struct{}
}

func newAsyncWriter(w io.Writer, bufSize int) *asyncWriter {
	aw := &asyncWriter{
		ch:   make(chan []byte, bufSize),
		done: make(chan struct{}),
	}
	go func() {
		for b := range aw.ch {
			w.Write(b) //nolint:errcheck
		}
		close(aw.done)
	}()
	return aw
}

func (aw *asyncWriter) Write(p []byte) (int, error) {
	cp := make([]byte, len(p))
	copy(cp, p)
	aw.ch <- cp
	return len(p), nil
}

func (aw *asyncWriter) flush() {
	close(aw.ch)
	<-aw.done
}

// Sink owns a pair of async writers and can flush them.
type Sink struct {
	out    *asyncWriter
	errout *asyncWriter
	level  Level
}

// NewSink creates a Sink that asynchronously writes to out and errout.
func NewSink(out, errout io.Writer, level Level) *Sink {
	return &Sink{
		out:    newAsyncWriter(out, 4096),
		errout: newAsyncWriter(errout, 4096),
		level:  level,
	}
}

// Flush drains all pending log output. Call once at shutdown.
func (s *Sink) Flush() {
	s.out.flush()
	s.errout.flush()
}

// Logger returns a Logger attached to this Sink.
func (s *Sink) Logger(tag string, level Level) *Logger {
	return &Logger{tag: tag, level: level, out: s.out, errout: s.errout}
}

// NewLogger creates a Logger from this Sink, reading the level from
// OLLIE_{TAG}_LOG and falling back to the Sink's default level.
func (s *Sink) NewLogger(tag string) *Logger {
	l := s.level
	if env := os.Getenv("OLLIE_" + strings.ToUpper(tag) + "_LOG"); env != "" {
		l = ParseLevel(env, l)
	}
	return s.Logger(tag, l)
}

// Logger emits tagged, leveled log messages.
type Logger struct {
	tag    string
	level  Level
	out    io.Writer
	errout io.Writer
}

// NewWriter creates a Logger that writes directly to the given writers.
func NewWriter(tag string, level Level, out, errout io.Writer) *Logger {
	return &Logger{tag: tag, level: level, out: out, errout: errout}
}

func (l *Logger) emit(lvl Level, format string, args ...any) {
	if lvl < l.level {
		return
	}
	msg := fmt.Sprintf("%s: %s\n", l.tag, fmt.Sprintf(format, args...))
	if lvl >= LevelWarn {
		fmt.Fprint(l.errout, msg)
	} else {
		fmt.Fprint(l.out, msg)
	}
}

func (l *Logger) Debug(format string, args ...any) { l.emit(LevelDebug, format, args...) }
func (l *Logger) Info(format string, args ...any)  { l.emit(LevelInfo, format, args...) }
func (l *Logger) Warn(format string, args ...any)  { l.emit(LevelWarn, format, args...) }
func (l *Logger) Error(format string, args ...any) { l.emit(LevelError, format, args...) }
