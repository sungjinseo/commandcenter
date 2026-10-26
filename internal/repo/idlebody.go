package repo

import (
	"io"
	"net/http"
	"time"
)

// maxDrain bounds the read of what is left of a body closed before its end.
const maxDrain = 5 * time.Second

// IdleBody returns r.Body with an idle read deadline: while the handler reads
// the body the connection's read deadline is now+idle, set before each read
// and cleared after it, so only a body that stalls for idle is cut (its read
// then fails with a timeout error). Time the handler spends before its first
// read or between reads (a per-path lock, a rebuild, disk writes) does not
// count, and after the body is over no deadline is left for net/http's
// background read to hit; Close then clears it as well. Close before the end
// arms a short deadline instead (idle, at most 5 s): net/http reads what is
// left to reuse the connection, and a client that stalls there must not hold
// it (QA N-5).
//
// A body that has nothing to read (http.NoBody, Content-Length: 0) is
// returned unchanged. When the connection does not support deadlines the body
// is passed through unchanged. IdleBody applied to a body IdleBody returned
// replaces its idle value for every later read (npm's 5-minute publish inside
// the server's 60 s); the two share whether the body is over.
func IdleBody(w http.ResponseWriter, r *http.Request, idle time.Duration) io.ReadCloser {
	if idle <= 0 || r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return r.Body
	}
	if inner, ok := r.Body.(*idleBody); ok {
		return &idleBody{ReadCloser: inner.ReadCloser, rc: inner.rc, idle: idle, over: inner.over}
	}
	rc := http.NewResponseController(w)
	// Probe for support with the zero deadline, which leaves none armed.
	if rc.SetReadDeadline(time.Time{}) != nil {
		return r.Body
	}
	return &idleBody{ReadCloser: r.Body, rc: rc, idle: idle, over: new(bool)}
}

type idleBody struct {
	io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration
	over *bool // read to its end or closed
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.rc.SetReadDeadline(time.Now().Add(b.idle))
	n, err := b.ReadCloser.Read(p)
	// Disarm until the next read: the time the handler spends between reads
	// (writing to disk, waiting for a lock) is not the client's, and once
	// the body is over net/http's background read must not time out.
	b.rc.SetReadDeadline(time.Time{})
	if err == io.EOF {
		*b.over = true
	}
	return n, err
}

func (b *idleBody) Close() error {
	if *b.over {
		b.rc.SetReadDeadline(time.Time{})
	} else {
		*b.over = true
		b.rc.SetReadDeadline(time.Now().Add(min(b.idle, maxDrain)))
	}
	return b.ReadCloser.Close()
}
