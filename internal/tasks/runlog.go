package tasks

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"commandcenter/internal/logsafe"
)

// Run log caps (plan 11 Decision 7): the stored last_log holds at most
// MaxLogLines lines and MaxLogBytes bytes, the omission marker included.
// Past the cap it keeps the head and the tail, so the final lines (failed,
// finished) are always there.
const (
	MaxLogLines = 200
	MaxLogBytes = 64 << 10
	headLines   = 100
	tailLines   = MaxLogLines - headLines - 1 // one line for the marker
	maxLineLen  = 2048
	markerBytes = 128
	halfBytes   = (MaxLogBytes - markerBytes) / 2
)

// RunLog collects the log lines of one run, capped, and copies every line
// to slog (and so to the log ring), uncapped there because the ring bounds
// itself. URLs are redacted. It is used by one goroutine.
type RunLog struct {
	log   *slog.Logger
	attrs []any
	now   func() time.Time

	head      []string
	headBytes int
	headFull  bool
	tail      []string // the newest lines after the head, oldest first
	tailBytes int
	omitted   int // lines dropped between head and tail
}

func newRunLog(log *slog.Logger, now func() time.Time, attrs ...any) *RunLog {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &RunLog{log: log, attrs: attrs, now: now}
}

// Infof, Warnf and Errorf add a line at that level.
func (l *RunLog) Infof(format string, args ...any)  { l.add(slog.LevelInfo, format, args...) }
func (l *RunLog) Warnf(format string, args ...any)  { l.add(slog.LevelWarn, format, args...) }
func (l *RunLog) Errorf(format string, args ...any) { l.add(slog.LevelError, format, args...) }

func (l *RunLog) add(level slog.Level, format string, args ...any) {
	msg := logsafe.URLs(fmt.Sprintf(format, args...))
	l.log.Log(context.Background(), level, msg, l.attrs...)
	msg = strings.ToValidUTF8(strings.ReplaceAll(msg, "\n", " "), "?")
	if len(msg) > maxLineLen {
		msg = strings.ToValidUTF8(msg[:maxLineLen], "") + "…"
	}
	line := l.now().UTC().Format(time.RFC3339) + " " + level.String() + " " + msg
	n := len(line) + 1
	if !l.headFull {
		if len(l.head) < headLines && l.headBytes+n <= halfBytes {
			l.head = append(l.head, line)
			l.headBytes += n
			return
		}
		l.headFull = true
	}
	l.tail = append(l.tail, line)
	l.tailBytes += n
	for len(l.tail) > tailLines || l.tailBytes > halfBytes {
		l.tailBytes -= len(l.tail[0]) + 1
		l.tail = l.tail[1:]
		l.omitted++
	}
}

// String returns the kept lines, with a marker where lines were omitted.
func (l *RunLog) String() string {
	lines := append([]string{}, l.head...)
	if l.omitted > 0 {
		lines = append(lines, fmt.Sprintf("… %d lines omitted (see the server log) …", l.omitted))
	}
	return strings.Join(append(lines, l.tail...), "\n")
}
