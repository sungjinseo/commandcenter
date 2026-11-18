package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/repo"
)

// Forward passes request r through to rp's remote at remotePath (relative to
// the remote URL) and copies the answer (status, Content-Type, body) to w,
// caching nothing: npm audit. Both bodies are bounded by maxBody: an answer
// found longer only after its headers were written is an error, and the caller
// must cut the response (http.ErrAbortHandler), not end it. It holds a fetch
// slot and stops on a stalled answer as a fetch does. The remote's failure is a
// 502 (ErrRemote); a blocked remote is never asked.
func (f *Fetcher) Forward(ctx context.Context, rp *repo.Repository, w http.ResponseWriter, r *http.Request, remotePath string, maxBody int64) error {
	if err := f.blocked(rp, f.Now()); err != nil {
		slog.Warn("remote request skipped", "repo", rp.Name, "path", remotePath, "err", logsafe.Err(err))
		return ErrRemote
	}
	ctx, release, err := f.slot(ctx)
	if err != nil {
		return err
	}
	defer release()
	u := *rp.RemoteURL
	u.Path, u.RawPath, u.RawQuery = rp.RemoteURL.Path+remotePath, "", r.URL.RawQuery
	body := &capReader{Reader: http.MaxBytesReader(w, r.Body, maxBody)}
	req, err := http.NewRequestWithContext(repoContext(ctx, rp), r.Method, u.String(), body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set(HopHeader, hopValue(ctx))
	for _, k := range []string{"Content-Type", "Content-Encoding", "Accept", "Accept-Encoding"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	if ra := rp.RemoteAuth; ra != nil && sendsCredentials(rp, req.URL) {
		if ra.BearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+ra.BearerToken)
		} else {
			req.SetBasicAuth(ra.Username, ra.Password)
		}
	}
	resp, err := f.client(rp).Do(req)
	if err != nil {
		if body.tooBig { // the cap, not the remote
			return &repo.Error{Status: http.StatusRequestEntityTooLarge, Msg: fmt.Sprintf("request body larger than %d bytes", maxBody)}
		}
		f.noteFailure(rp, err)
		slog.Warn("remote request failed", "repo", rp.Name, "path", remotePath, "err", logsafe.Err(err))
		return ErrRemote
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		f.noteFailure(rp, fmt.Errorf("remote status %d", resp.StatusCode))
		return ErrRemote
	}
	f.noteSuccess(rp)
	if resp.ContentLength > maxBody {
		slog.Warn("remote answer too large", "repo", rp.Name, "path", remotePath, "bytes", resp.ContentLength)
		return ErrRemote
	}
	for _, k := range []string{"Content-Type", "Content-Encoding"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, capBody(rp, resp.Body, maxBody, release))
	return err
}

// Post sends the form body to target, caching nothing, for an API the remote's
// documents name, which may lie outside the remote URL (Composer's security
// advisories), and returns the answer for the caller to read and close. target,
// and where a redirect leads, must be one APIAllowed. Egress policy and
// credentials are a fetch's; as in Forward a blocked remote is never asked, a
// network failure or a 5xx is ErrRemote, which counts for the auto-block only
// on the remote's own host, an answer longer than maxBody is ErrRemote or an
// error reading its body, and the request holds a fetch slot until the body is
// closed.
func (f *Fetcher) Post(ctx context.Context, rp *repo.Repository, target string, header http.Header, body string, maxBody int64) (*http.Response, error) {
	if err := f.blocked(rp, f.Now()); err != nil {
		slog.Warn("remote request skipped", "repo", rp.Name, "url", logsafe.Path(target), "err", logsafe.Err(err))
		return nil, ErrRemote
	}
	if u, err := url.Parse(target); err != nil || !APIAllowed(rp, u) {
		slog.Warn("remote API refused: not on the remote's host or its parent domain", "repo", rp.Name, "url", logsafe.Path(target))
		return nil, ErrRemote
	}
	ctx, release, err := f.slot(ctx)
	if err != nil {
		return nil, err
	}
	req, err := f.newRequest(ctx, rp, target, header)
	if err != nil {
		release()
		return nil, err
	}
	req.Method = http.MethodPost
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
	req.Body, _ = req.GetBody()
	own := sendsCredentials(rp, req.URL)
	c := *f.client(rp)
	check := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !APIAllowed(rp, req.URL) {
			return errors.New("redirect off the remote's host or its parent domain")
		}
		if check != nil {
			return check(req, via)
		}
		return nil
	}
	resp, err := c.Do(req)
	if err == nil && resp.StatusCode >= 500 {
		resp.Body.Close()
		err = fmt.Errorf("remote status %d", resp.StatusCode)
	}
	if err != nil {
		if own && ctx.Err() == nil { // a client that gave up says nothing about the remote
			f.noteFailure(rp, err)
		}
		release()
		slog.Warn("remote request failed", "repo", rp.Name, "url", logsafe.Path(target), "err", logsafe.Err(err))
		return nil, ErrRemote
	}
	if own {
		f.noteSuccess(rp)
	}
	if resp.ContentLength > maxBody {
		resp.Body.Close()
		release()
		slog.Warn("remote answer too large", "repo", rp.Name, "url", logsafe.Path(target), "bytes", resp.ContentLength)
		return nil, ErrRemote
	}
	resp.Body = capBody(rp, resp.Body, maxBody, release)
	return resp, nil
}

// APIAllowed reports whether u, an API a remote's document names (Composer's
// security advisories), may be asked for rp: on the remote's host name or its
// parent domain (repo.packagist.org's is packagist.org), over https, or over
// http on the host of a remote that is http itself. A forward proxy resolves
// host names the egress policy cannot check, so a public upstream could
// otherwise have the server POST to any internal name.
func APIAllowed(rp *repo.Repository, u *url.URL) bool {
	host, remote := u.Hostname(), rp.RemoteURL.Hostname()
	if strings.EqualFold(host, remote) {
		return u.Scheme == "https" || u.Scheme == "http" && rp.RemoteURL.Scheme == "http"
	}
	_, parent, _ := strings.Cut(remote, ".")
	if _, err := netip.ParseAddr(remote); err == nil || !strings.Contains(parent, ".") {
		return false
	}
	return u.Scheme == "https" && strings.EqualFold(host, parent)
}

// slot waits for a fetch slot for a request under ctx and returns the
// request's context, bounded as a fetch's, and what ends it and gives the
// slot back (once).
func (f *Fetcher) slot(ctx context.Context) (context.Context, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	select {
	case f.sem <- struct{}{}:
	case <-ctx.Done():
		cancel()
		return nil, nil, ErrRemote
	}
	var once sync.Once
	return ctx, func() { once.Do(func() { cancel(); <-f.sem }) }, nil
}

// capBody is a remote answer read as a fetch reads one: release (which ends the
// request) when a read waits a stall, and an error past max bytes rather than
// an end that looks complete. Closing it releases too.
func capBody(rp *repo.Repository, body io.ReadCloser, max int64, release func()) io.ReadCloser {
	return &cappedBody{r: newStallReader(body, idleFor(rp), release), body: body, left: max, max: max, release: release}
}

type cappedBody struct {
	r         io.Reader
	body      io.Closer
	left, max int64
	release   func()
}

func (b *cappedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if b.left -= int64(n); b.left < 0 {
		return 0, fmt.Errorf("remote answer larger than %d bytes", b.max)
	}
	return n, err
}

func (b *cappedBody) Close() error {
	err := b.body.Close()
	b.release()
	return err
}

// capReader remembers whether its MaxBytesReader refused the body.
type capReader struct {
	io.Reader
	tooBig bool
}

func (c *capReader) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	if err != nil && errors.As(err, new(*http.MaxBytesError)) {
		c.tooBig = true
	}
	return n, err
}
