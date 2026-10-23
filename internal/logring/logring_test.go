package logring

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/slogtest"
	"unicode/utf8"
)

func newLogger(ring *Ring, out io.Writer, level slog.Level) *slog.Logger {
	return slog.New(NewHandler(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level}), ring))
}

func TestHandlerForwardsAndCopies(t *testing.T) {
	ring := NewRing(10, 1<<20)
	var out bytes.Buffer
	log := newLogger(ring, &out, slog.LevelInfo)
	log.Info("hello", "a", 1, "b", "two words")
	if !strings.Contains(out.String(), `"msg":"hello"`) {
		t.Fatalf("stdout handler got nothing: %q", out.String())
	}
	recs := ring.Snapshot(slog.LevelDebug, "", 10)
	if len(recs) != 1 {
		t.Fatalf("ring has %d records", len(recs))
	}
	r := recs[0]
	if r.Msg != "hello" || r.Level != slog.LevelInfo || r.Attrs != `a=1 b="two words"` || r.Time.IsZero() {
		t.Fatalf("record %+v", r)
	}
}

func TestDisabledLevelIsNotCopied(t *testing.T) {
	ring := NewRing(10, 1<<20)
	var out bytes.Buffer
	log := newLogger(ring, &out, slog.LevelWarn)
	log.Info("quiet")
	log.Warn("loud")
	recs := ring.Snapshot(slog.LevelDebug, "", 10)
	if len(recs) != 1 || recs[0].Msg != "loud" {
		t.Fatalf("records %+v", recs)
	}
}

func TestWithAttrsAndWithGroup(t *testing.T) {
	ring := NewRing(10, 1<<20)
	var out bytes.Buffer
	log := newLogger(ring, &out, slog.LevelInfo)
	l2 := log.With("svc", "x").WithGroup("req").With("id", 7)
	l2.Info("m", "path", "/p", slog.Group("g", slog.Int("n", 3)))
	got := ring.Snapshot(slog.LevelDebug, "", 1)[0].Attrs
	want := `svc=x req.id=7 req.path=/p req.g.n=3`
	if got != want {
		t.Fatalf("attrs %q, want %q", got, want)
	}
	// The stdout handler saw the same structure.
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["svc"] != "x" || m["req"].(map[string]any)["id"] != float64(7) {
		t.Fatalf("stdout %v", m)
	}
	// The parent logger is unaffected by the derived one.
	log.Info("plain", "k", "v")
	if got := ring.Snapshot(slog.LevelDebug, "", 1)[0].Attrs; got != "k=v" {
		t.Fatalf("parent attrs %q", got)
	}
}

func TestEmptyGroupAndEmptyAttrs(t *testing.T) {
	ring := NewRing(10, 1<<20)
	log := newLogger(ring, &bytes.Buffer{}, slog.LevelInfo)
	log.WithGroup("empty").Info("m")
	log.Info("m2", slog.Group("g"), slog.Attr{})
	for _, r := range ring.Snapshot(slog.LevelDebug, "", 10) {
		if r.Attrs != "" {
			t.Fatalf("attrs %q for %q", r.Attrs, r.Msg)
		}
	}
}

func TestSlogtest(t *testing.T) {
	ring := NewRing(100, 1<<20)
	var out bytes.Buffer
	h := NewHandler(slog.NewJSONHandler(&out, nil), ring)
	err := slogtest.TestHandler(h, func() []map[string]any {
		var ms []map[string]any
		for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte{'\n'}) {
			var m map[string]any
			if err := json.Unmarshal(line, &m); err != nil {
				t.Fatal(err)
			}
			ms = append(ms, m)
		}
		return ms
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRingBoundedByCount(t *testing.T) {
	ring := NewRing(5, 1<<20)
	log := newLogger(ring, &bytes.Buffer{}, slog.LevelInfo)
	for i := 0; i < 12; i++ {
		log.Info(fmt.Sprint("m", i))
	}
	recs := ring.Snapshot(slog.LevelDebug, "", 100)
	if len(recs) != 5 || recs[0].Msg != "m11" || recs[4].Msg != "m7" { // newest first
		t.Fatalf("records %+v", recs)
	}
}

func TestRingBoundedByBytes(t *testing.T) {
	// Each record is about len(msg) + overhead; a 1 KiB message and a 3 KiB cap hold at most 2-3.
	ring := NewRing(1000, 3<<10)
	log := newLogger(ring, &bytes.Buffer{}, slog.LevelInfo)
	big := strings.Repeat("x", 1024)
	for i := 0; i < 20; i++ {
		log.Info(big, "i", i)
	}
	recs := ring.Snapshot(slog.LevelDebug, "", 100)
	if len(recs) < 1 || len(recs) > 3 {
		t.Fatalf("%d records kept", len(recs))
	}
	if ring.Bytes() > 3<<10 {
		t.Fatalf("ring holds %d bytes", ring.Bytes())
	}
	if recs[0].Attrs != "i=19" {
		t.Fatalf("newest %+v", recs[0])
	}
}

func TestOversizedRecordIsClipped(t *testing.T) {
	ring := NewRing(10, 1<<20)
	log := newLogger(ring, &bytes.Buffer{}, slog.LevelInfo)
	log.Info(strings.Repeat("m", 100_000), "k", strings.Repeat("v", 100_000))
	r := ring.Snapshot(slog.LevelDebug, "", 1)[0]
	if len(r.Msg) > maxMsg+len("…") || len(r.Attrs) > maxAttrs+len("…") {
		t.Fatalf("lengths %d %d", len(r.Msg), len(r.Attrs))
	}
}

func TestSnapshotFilters(t *testing.T) {
	ring := NewRing(100, 1<<20)
	log := newLogger(ring, &bytes.Buffer{}, slog.LevelDebug)
	log.Debug("d one")
	log.Info("i one")
	log.Warn("w one", "user", "Alice")
	log.Error("e one")
	names := func(rs []Record) string {
		var s []string
		for _, r := range rs {
			s = append(s, r.Msg)
		}
		return strings.Join(s, ",")
	}
	if got := names(ring.Snapshot(slog.LevelInfo, "", 100)); got != "e one,w one,i one" {
		t.Fatalf("INFO: %s", got)
	}
	if got := names(ring.Snapshot(slog.LevelWarn, "", 100)); got != "e one,w one" {
		t.Fatalf("WARN: %s", got)
	}
	if got := names(ring.Snapshot(slog.LevelError, "", 100)); got != "e one" {
		t.Fatalf("ERROR: %s", got)
	}
	if got := names(ring.Snapshot(slog.LevelDebug, "alice", 100)); got != "w one" { // q: case-insensitive, over attrs
		t.Fatalf("q: %s", got)
	}
	if got := names(ring.Snapshot(slog.LevelDebug, "", 2)); got != "e one,w one" {
		t.Fatalf("limit: %s", got)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"": slog.LevelDebug, "debug": slog.LevelDebug, "INFO": slog.LevelInfo,
		"warn": slog.LevelWarn, "ERROR": slog.LevelError} {
		got, ok := ParseLevel(in)
		if !ok || got != want {
			t.Fatalf("%q: %v %v", in, got, ok)
		}
	}
	if _, ok := ParseLevel("loud"); ok {
		t.Fatal("accepted loud")
	}
}

func TestConcurrentUse(t *testing.T) {
	ring := NewRing(50, 1<<16)
	log := newLogger(ring, &lockedBuf{}, slog.LevelInfo)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := log.With("g", g)
			for i := 0; i < 200; i++ {
				l.Info("m", "i", i)
				ring.Snapshot(slog.LevelDebug, "", 10)
			}
		}()
	}
	wg.Wait()
	if n := len(ring.Snapshot(slog.LevelDebug, "", 1000)); n != 50 {
		t.Fatalf("%d records", n)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func TestRedactsURLCredentials(t *testing.T) {
	ring := NewRing(10, 1<<20)
	log := newLogger(ring, &bytes.Buffer{}, slog.LevelInfo)
	log.Info("fetching https://user:pw@host/p?token=x failed", "err", `Get "http://u2:pw2@h2/q?key=sekret&a=b": EOF`)
	r := ring.Snapshot(slog.LevelDebug, "", 1)[0]
	all := r.Msg + " " + r.Attrs
	for _, bad := range []string{"user", "pw", "token=x", "u2", "pw2", "sekret", "a=b"} {
		if strings.Contains(all, bad) {
			t.Fatalf("%q survived in %q", bad, all)
		}
	}
	if !strings.Contains(all, "https://host/p?token=REDACTED") {
		t.Fatalf("got %q", all)
	}
}

func TestClipInvalidByteEarly(t *testing.T) {
	got := clip("\xff"+strings.Repeat("é", 5000), 100)
	if len(got) > 100+len("…") || len(got) < 100-utf8.UTFMax {
		t.Fatalf("len %d", len(got))
	}
}

func TestNewlinesEscapedInLines(t *testing.T) {
	ring := NewRing(10, 1<<20)
	log := newLogger(ring, &bytes.Buffer{}, slog.LevelInfo)
	log.Info("line one\nline two\r", "k", "a\nb")
	s := ring.Snapshot(slog.LevelDebug, "", 1)[0].String()
	if strings.ContainsAny(s, "\r\n") || !strings.Contains(s, `line one\nline two\r`) {
		t.Fatalf("%q", s)
	}
	// An attribute key can hold a newline too.
	log.Info("m", "bad\nkey", 1)
	if s := ring.Snapshot(slog.LevelDebug, "", 1)[0].String(); strings.ContainsAny(s, "\r\n") {
		t.Fatalf("%q", s)
	}
}

type huge struct{ called *bool }

func (h huge) String() string { *h.called = true; return strings.Repeat("z", 10<<20) }

func TestAttrBuilderIsCapped(t *testing.T) {
	ring := NewRing(10, 1<<20)
	log := newLogger(ring, &bytes.Buffer{}, slog.LevelInfo)
	called := false
	log.Info("m", "big", strings.Repeat("a", 50<<20), "later", huge{&called})
	r := ring.Snapshot(slog.LevelDebug, "", 1)[0]
	if len(r.Attrs) > maxAttrs+len("…")+16 { // redaction never grows it much
		t.Fatalf("attrs %d bytes", len(r.Attrs))
	}
	if called {
		t.Fatal("String() of a value after the cap was called")
	}
}
