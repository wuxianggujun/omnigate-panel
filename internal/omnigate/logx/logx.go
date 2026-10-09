// Package logx provides a tiny logger with an in-memory ring buffer so the
// gateway can expose "recent activity" over /healthz.
package logx

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Logger writes to stdout and keeps the most recent max lines.
type Logger struct {
	mu   sync.Mutex
	ring []string
	max  int
	out  func(string)
}

// New returns a Logger retaining up to max recent lines.
func New(max int) *Logger {
	if max <= 0 {
		max = 200
	}
	return &Logger{max: max, out: func(s string) { fmt.Println(s) }}
}

// SetOutput overrides where lines are written (defaults to stdout).
func (l *Logger) SetOutput(fn func(string)) { l.out = fn }

func (l *Logger) log(level, format string, args ...any) {
	line := fmt.Sprintf("%s  %-5s %s", time.Now().Format("15:04:05"), level, fmt.Sprintf(format, args...))
	l.mu.Lock()
	l.ring = append(l.ring, line)
	if len(l.ring) > l.max {
		l.ring = l.ring[len(l.ring)-l.max:]
	}
	out := l.out
	l.mu.Unlock()
	if out != nil {
		out(line)
	}
}

// Info logs an informational line.
func (l *Logger) Info(format string, args ...any) { l.log("INFO", format, args...) }

// Warn logs a warning line.
func (l *Logger) Warn(format string, args ...any) { l.log("WARN", format, args...) }

// Error logs an error line.
func (l *Logger) Error(format string, args ...any) { l.log("ERROR", format, args...) }

// Recent returns the buffered lines, newest first.
func (l *Logger) Recent() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.ring) == 0 {
		return "（暂无记录）"
	}
	var sb strings.Builder
	for i := len(l.ring) - 1; i >= 0; i-- {
		sb.WriteString(l.ring[i])
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}
