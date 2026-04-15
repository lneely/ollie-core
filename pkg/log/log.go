package log

import (
	"fmt"
	"os"
	"strings"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var defaultLevel Level

type entry struct {
	stderr bool
	msg    string
}

var entries = make(chan entry, 4096)
var done = make(chan struct{})

func init() {
	defaultLevel = parseLevel(os.Getenv("OLLIE_LOG"), LevelWarn)
	go writer()
}

func writer() {
	for e := range entries {
		if e.stderr {
			fmt.Fprint(os.Stderr, e.msg)
		} else {
			fmt.Fprint(os.Stdout, e.msg)
		}
	}
	close(done)
}

// Flush closes the log channel and waits for all pending entries to be written.
func Flush() {
	close(entries)
	<-done
}

func parseLevel(s string, fallback Level) Level {
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

type Logger struct {
	tag   string
	level Level
}

func New(tag string) *Logger {
	l := defaultLevel
	if env := os.Getenv("OLLIE_" + strings.ToUpper(tag) + "_LOG"); env != "" {
		l = parseLevel(env, l)
	}
	return &Logger{tag: tag, level: l}
}

func (l *Logger) emit(lvl Level, format string, args ...any) {
	if lvl < l.level {
		return
	}
	msg := fmt.Sprintf("%s: %s\n", l.tag, fmt.Sprintf(format, args...))
	entries <- entry{stderr: lvl >= LevelWarn, msg: msg}
}

func (l *Logger) Debug(format string, args ...any) { l.emit(LevelDebug, format, args...) }
func (l *Logger) Info(format string, args ...any)  { l.emit(LevelInfo, format, args...) }
func (l *Logger) Warn(format string, args ...any)  { l.emit(LevelWarn, format, args...) }
func (l *Logger) Error(format string, args ...any) { l.emit(LevelError, format, args...) }
