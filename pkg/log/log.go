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

func init() {
	defaultLevel = parseLevel(os.Getenv("OLLIE_LOG"), LevelWarn)
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
	msg := fmt.Sprintf(format, args...)
	if lvl >= LevelWarn {
		fmt.Fprintf(os.Stderr, "%s: %s\n", l.tag, msg)
	} else {
		fmt.Fprintf(os.Stdout, "%s: %s\n", l.tag, msg)
	}
}

func (l *Logger) Debug(format string, args ...any) { l.emit(LevelDebug, format, args...) }
func (l *Logger) Info(format string, args ...any)  { l.emit(LevelInfo, format, args...) }
func (l *Logger) Warn(format string, args ...any)  { l.emit(LevelWarn, format, args...) }
func (l *Logger) Error(format string, args ...any) { l.emit(LevelError, format, args...) }
