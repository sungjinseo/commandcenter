package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

// TestPost: credentials go to the remote's own host only, a failure counts for
// the auto-block only there, and a blocked remote is not asked at all.
func TestPost(t *testing.T) {
	var auth, ctype, body string
	status := http.StatusOK
	asked := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		asked++
		auth, ctype, body = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b)
		if r.Method != http.MethodPost || r.ContentLength != int64(len(b)) {
			t.Errorf("%s with Content-Length %d for %d bytes", r.Method, r.ContentLength, len(b))
		}
		w.WriteHeader(status)
		io.WriteString(w, "answer")
	}))
	defer remote.Close()
	elsewhere := httptest.NewServer(remote.Config.Handler)
	defer elsewhere.Close()
	u, _ := url.Parse(remote.URL + "/")
	rp := &repo.Repository{Repository: config.Repository{Name: "p", RemoteAuth: &config.RemoteAuth{BearerToken: "secret"}}, RemoteURL: u}
	f := New(nil, remote.Client(), 1)
	ctx := context.Background()
	header := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}

	resp, err := f.Post(ctx, rp, remote.URL+"/api", header, "a=b")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "answer" || auth != "Bearer secret" || ctype != header.Get("Content-Type") || body != "a=b" {
		t.Fatalf("own host: %q %q %q %q", got, auth, ctype, body)
	}

	if resp, err = f.Post(ctx, rp, elsewhere.URL+"/api", header, "a=b"); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if auth != "" {
		t.Fatalf("credentials sent to another host: %q", auth)
	}

	// A client that gave up is no failure of the remote (Composer waits 10 seconds).
	gone, cancel := context.WithCancel(ctx)
	cancel()
	for range autoBlockAfter + 1 {
		if _, err := f.Post(gone, rp, remote.URL+"/api", header, "a=b"); err != ErrRemote {
			t.Fatalf("cancelled: %v", err)
		}
	}
	if err := f.blocked(rp, f.Now()); err != nil {
		t.Fatalf("clients that gave up blocked the remote: %v", err)
	}

	// A 404 is an answer for the caller; a 5xx is ErrRemote and blocks only its host.
	status = http.StatusNotFound
	if resp, err = f.Post(ctx, rp, remote.URL+"/api", header, "a=b"); err != nil || resp.StatusCode != 404 {
		t.Fatalf("404: %v %v", resp, err)
	}
	resp.Body.Close()
	status = http.StatusBadGateway
	for range autoBlockAfter {
		if _, err := f.Post(ctx, rp, elsewhere.URL+"/api", header, "a=b"); err != ErrRemote {
			t.Fatalf("5xx elsewhere: %v", err)
		}
	}
	if err := f.blocked(rp, f.Now()); err != nil {
		t.Fatalf("another host's failures blocked the remote: %v", err)
	}
	for range autoBlockAfter {
		if _, err := f.Post(ctx, rp, remote.URL+"/api", header, "a=b"); err != ErrRemote {
			t.Fatalf("5xx: %v", err)
		}
	}
	before := asked
	if _, err := f.Post(ctx, rp, remote.URL+"/api", header, "a=b"); err != ErrRemote || asked != before {
		t.Fatalf("auto-blocked: %v, asked %d times more", err, asked-before)
	}
}
