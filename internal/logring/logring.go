// Package logring keeps the most recent log records in memory for the web UI's
// log viewer. A slog.Handler tees every record into a bounded ring while the
// wrapped handler still writes to stdout.
package logring

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"commandcenter/internal/logsafe"
)

const (
	// DefaultRecords and DefaultBytes bound the ring, whichever is hit first.
	DefaultRecords = 2000
	DefaultBytes   = 1 << 20

	maxMsg   = 2048 // a single record never costs more than about maxMsg+maxAttrs+overhead
	maxAttrs = 4096
	overhead = 64 // fixed cost counted per record besides its text
)

// Record is one captured log record.
type Record struct {
	Time  time.Time
	Level slog.Level
	Msg   string
	Attrs string // flat "key=value" pairs; group keys are dotted
}

// oneLine escapes line breaks so a record stays one line in the download.
var oneLiner = strings.NewReplacer("\r", `\r`, "\n", `\n`)

func oneLine(s string) string { return oneLiner.Replace(s) }

func (r Record) size() int { return len(r.Msg) + len(r.Attrs) + overhead }

// String renders the record as one log line.
func (r Record) String() string {
	var b strings.Builder
	b.WriteString(r.Time.UTC().Format("2006-01-02T15:04:05.000Z"))
	b.WriteByte(' ')
	b.WriteString(r.Level.String())
	b.WriteByte(' ')
	b.WriteString(oneLine(r.Msg))
	if r.Attrs != "" {
		b.WriteByte(' ')
		b.WriteString(oneLine(r.Attrs))
	}
	return b.String()
}

// Ring holds at most maxRecords records and maxBytes bytes; the oldest are
// dropped first. It is safe for concurrent use.
type Ring struct {
	maxRecords, maxBytes int

	mu    sync.Mutex
	recs  []Record // recs[head:] are live, oldest first
	head  int
	bytes int
}

// NewRing returns an empty ring; non-positive limits select the defaults.
func NewRing(maxRecords, maxBytes int) *Ring {
	if maxRecords <= 0 {
		maxRecords = DefaultRecords
	}
	if maxBytes <= 0 {
		maxBytes = DefaultBytes
	}
	return &Ring{maxRecords: maxRecords, maxBytes: maxBytes}
}

// Add appends a record, evicting the oldest ones to stay within the bounds.
func (g *Ring) Add(r Record) {
	// Bound first, then hide credentials of any URL in the text (defense in depth).
	r.Msg = logsafe.URLs(clip(r.Msg, maxMsg))
	r.Attrs = logsafe.URLs(clip(r.Attrs, maxAttrs))
	g.mu.Lock()
	defer g.mu.Unlock()
	g.recs = append(g.recs, r)
	g.bytes += r.size()
	for len(g.recs)-g.head > 0 && (len(g.recs)-g.head > g.maxRecords || g.bytes > g.maxBytes) {
		g.bytes -= g.recs[g.head].size()
		g.recs[g.head] = Record{} // release the strings
		g.head++
	}
	if g.head > 0 && g.head >= len(g.recs)/2 { // compact so the slice does not grow forever
		g.recs = append([]Record(nil), g.recs[g.head:]...)
		g.head = 0
	}
}

// Bytes is the memory the ring accounts for.
func (g *Ring) Bytes() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.bytes
}

// Snapshot returns up to limit records at or above min whose message or
// attributes contain q (case-insensitive), newest first. A non-positive limit
// returns all matches.
func (g *Ring) Snapshot(min slog.Level, q string, limit int) []Record {
	q = strings.ToLower(q)
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Record
	for i := len(g.recs) - 1; i >= g.head; i-- {
		r := g.recs[i]
		if r.Level < min {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(r.Msg), q) && !strings.Contains(strings.ToLower(r.Attrs), q) {
			continue
		}
		out = append(out, r)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// ParseLevel maps the API's level names; "" means no filter (DEBUG).
func ParseLevel(s string) (slog.Level, bool) {
	switch strings.ToUpper(s) {
	case "", "DEBUG":
		return slog.LevelDebug, true
	case "INFO":
		return slog.LevelInfo, true
	case "WARN":
		return slog.LevelWarn, true
	case "ERROR":
		return slog.LevelError, true
	}
	return 0, false
}

// Handler wraps a handler and copies every record it handles into a Ring.
type Handler struct {
	next   slog.Handler
	ring   *Ring
	groups string // current group prefix, "a.b."
	pre    string // formatted attributes added with WithAttrs, already prefixed
}

// NewHandler returns a handler that passes records to next and into ring.
func NewHandler(next slog.Handler, ring *Ring) *Handler {
	return &Handler{next: next, ring: ring}
}

func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(h.pre)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.groups, a)
		return b.Len() < maxAttrs
	})
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	h.ring.Add(Record{Time: t, Level: r.Level, Msg: r.Message, Attrs: b.String()})
	return h.next.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	var b strings.Builder
	b.WriteString(h.pre)
	for _, a := range attrs {
		appendAttr(&b, h.groups, a)
	}
	return &Handler{next: h.next.WithAttrs(attrs), ring: h.ring, groups: h.groups, pre: b.String()}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &Handler{next: h.next.WithGroup(name), ring: h.ring, groups: h.groups + name + ".", pre: h.pre}
}

// appendAttr writes a as "prefix+key=value", flattening groups; empty
// attributes and empty groups write nothing.
func appendAttr(b *strings.Builder, prefix string, a slog.Attr) {
	if b.Len() >= maxAttrs { // the record is clipped anyway: skip the work, in particular String() of huge values
		return
	}
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		sub := prefix
		if a.Key != "" {
			sub += a.Key + "."
		}
		for _, g := range a.Value.Group() {
			if b.Len() >= maxAttrs {
				return
			}
			appendAttr(b, sub, g)
		}
		return
	}
	if b.Len() > 0 {
		b.WriteByte(' ')
	}
	b.WriteString(prefix)
	b.WriteString(a.Key)
	b.WriteByte('=')
	v := a.Value.String()
	if len(v) > maxAttrs {
		v = v[:maxAttrs] // never quote more than can be kept
	}
	if v == "" || strings.ContainsAny(v, " \t\r\n\"=") {
		v = strconv.Quote(v)
	}
	b.WriteString(v)
}

// clip cuts s to at most n bytes, backing up at most utf8.UTFMax bytes to a rune
// start, and marks the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for i := 0; i < utf8.UTFMax && cut > 0 && !utf8.RuneStart(s[cut]); i++ {
		cut--
	}
	return s[:cut] + "…"
}
