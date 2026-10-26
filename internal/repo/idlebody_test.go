package repo

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIdleBodyStalledReadFails(t *testing.T) {
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := IdleBody(w, r, 150*time.Millisecond)
		defer b.Close()
		_, err := io.ReadAll(b)
		done <- err
	}))
	defer srv.Close()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "PUT /x HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\nabc")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled body read succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled body was not cut")
	}
}

func TestIdleBodyPassesDataThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := IdleBody(w, r, time.Second)
		data, err := io.ReadAll(b)
		b.Close()
		if err != nil || string(data) != "hello" {
			t.Errorf("data=%q err=%v", data, err)
		}
	}))
	defer srv.Close()
	req, _ := http.NewRequest("PUT", srv.URL, strings.NewReader("hello"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

type deadlineRec struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineRec) SetReadDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

// QA N-5: closed before its end, the body leaves a short deadline (idle, at
// most maxDrain) for net/http's read of the rest; read to its end, also
// through a nested IdleBody (npm publish), Close clears the deadline.
func TestIdleBodyCloseDeadline(t *testing.T) {
	for _, idle := range []time.Duration{time.Minute, 50 * time.Millisecond} {
		w := &deadlineRec{ResponseRecorder: httptest.NewRecorder()}
		r := httptest.NewRequest("PUT", "/x", strings.NewReader("abc"))
		b := IdleBody(w, r, idle)
		for _, d := range w.deadlines {
			if !d.IsZero() {
				t.Fatalf("deadline armed at create (final review M1): %v", w.deadlines)
			}
		}
		b.Read(make([]byte, 1))
		b.Close()
		if last := w.deadlines[len(w.deadlines)-1]; last.IsZero() || time.Until(last) > min(idle, maxDrain) {
			t.Fatalf("idle %v: deadlines after an early Close = %v, want a trailing one within %v", idle, w.deadlines, min(idle, maxDrain))
		}
	}
	w := &deadlineRec{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest("PUT", "/x", strings.NewReader("abc"))
	r.Body = IdleBody(w, r, time.Minute)
	io.ReadAll(IdleBody(w, r, time.Hour))
	r.Body.Close()
	if n := len(w.deadlines); n < 2 || !w.deadlines[n-1].IsZero() {
		t.Fatalf("deadlines after Close at the end = %v, want a trailing zero", w.deadlines)
	}
}

func TestIdleBodyEOFClearsDeadline(t *testing.T) {
	w := &deadlineRec{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest("PUT", "/x", strings.NewReader("abc"))
	b := IdleBody(w, r, time.Minute)
	io.ReadAll(b)
	if n := len(w.deadlines); n < 2 || !w.deadlines[n-1].IsZero() {
		t.Fatalf("deadlines after EOF = %v, want a trailing zero", w.deadlines)
	}
}

// Final review M1: no deadline is armed until the first read, each read is
// preceded by one, and a nested IdleBody (npm's 5-minute publish inside the
// server's 60 s) uses the outer, inner-most-called value for every read.
func TestIdleBodyArmsOnReadAndNestedOuterWins(t *testing.T) {
	w := &deadlineRec{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest("PUT", "/x", strings.NewReader("abc"))
	r.Body = IdleBody(w, r, time.Second)
	for _, d := range w.deadlines {
		if !d.IsZero() {
			t.Fatalf("deadline armed before any read: %v", w.deadlines)
		}
	}
	outer := IdleBody(w, r, time.Hour)
	w.deadlines = nil
	buf := make([]byte, 1)
	if _, err := outer.Read(buf); err != nil {
		t.Fatal(err)
	}
	if len(w.deadlines) == 0 || w.deadlines[0].IsZero() {
		t.Fatalf("no deadline set before the read: %v", w.deadlines)
	}
	for _, d := range w.deadlines {
		if !d.IsZero() && time.Until(d) < 30*time.Minute {
			t.Fatalf("deadlines during a nested read = %v, want only the outer (1h) one", w.deadlines)
		}
	}
	io.ReadAll(outer)
	if n := len(w.deadlines); !w.deadlines[n-1].IsZero() {
		t.Fatalf("deadline not cleared at EOF: %v", w.deadlines)
	}
}

func TestIdleBodySkipsEmptyBodies(t *testing.T) {
	w := &deadlineRec{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest("DELETE", "/x", nil)
	r.Body = http.NoBody
	if b := IdleBody(w, r, time.Second); b != http.NoBody {
		t.Fatalf("NoBody wrapped: %T", b)
	}
	r = httptest.NewRequest("PUT", "/x", strings.NewReader(""))
	if b := IdleBody(w, r, time.Second); b != r.Body {
		t.Fatalf("empty body wrapped: %T", b)
	}
	for _, d := range w.deadlines {
		if !d.IsZero() {
			t.Fatalf("deadline armed for an empty body: %v", w.deadlines)
		}
	}
}
