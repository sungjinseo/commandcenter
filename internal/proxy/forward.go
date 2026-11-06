package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/repo"
)

// Forward passes request r through to rp's remote at remotePath (relative to
// the remote URL) and copies the answer (status, Content-Type, body) to w,
// caching nothing: npm audit. Both bodies are bounded by maxBody. The
// remote's failure is a 502 (ErrRemote); a blocked remote is never asked.
func (f *Fetcher) Forward(ctx context.Context, rp *repo.Repository, w http.ResponseWriter, r *http.Request, remotePath string, maxBody int64) error {
	if err := f.blocked(rp, f.Now()); err != nil {
		slog.Warn("remote request skipped", "repo", rp.Name, "path", remotePath, "err", logsafe.Err(err))
		return ErrRemote
	}
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
	// ponytail: an answer without Content-Length is cut at maxBody, not refused.
	_, err = io.Copy(w, io.LimitReader(resp.Body, maxBody))
	return err
}

// Post sends the form body to target, caching nothing, for an API the remote's
// documents name, which may lie outside the remote URL (Composer's security
// advisories), and returns the answer for the caller to read and close. Egress
// policy and credentials are a fetch's; as in Forward a blocked remote is never
// asked and a network failure or a 5xx is ErrRemote, which counts for the
// auto-block only on the remote's own host.
func (f *Fetcher) Post(ctx context.Context, rp *repo.Repository, target string, header http.Header, body string) (*http.Response, error) {
	if err := f.blocked(rp, f.Now()); err != nil {
		slog.Warn("remote request skipped", "repo", rp.Name, "url", logsafe.Path(target), "err", logsafe.Err(err))
		return nil, ErrRemote
	}
	req, err := f.newRequest(ctx, rp, target, header)
	if err != nil {
		return nil, err
	}
	req.Method = http.MethodPost
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
	req.Body, _ = req.GetBody()
	own := sendsCredentials(rp, req.URL)
	resp, err := f.client(rp).Do(req)
	if err == nil && resp.StatusCode >= 500 {
		resp.Body.Close()
		err = fmt.Errorf("remote status %d", resp.StatusCode)
	}
	if err != nil {
		if own && ctx.Err() == nil { // a client that gave up says nothing about the remote
			f.noteFailure(rp, err)
		}
		slog.Warn("remote request failed", "repo", rp.Name, "url", logsafe.Path(target), "err", logsafe.Err(err))
		return nil, ErrRemote
	}
	if own {
		f.noteSuccess(rp)
	}
	return resp, nil
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
