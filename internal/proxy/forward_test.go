package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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

	resp, err := f.Post(ctx, rp, remote.URL+"/api", header, "a=b", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "answer" || auth != "Bearer secret" || ctype != header.Get("Content-Type") || body != "a=b" {
		t.Fatalf("own host: %q %q %q %q", got, auth, ctype, body)
	}

	if resp, err = f.Post(ctx, rp, elsewhere.URL+"/api", header, "a=b", 1<<20); err != nil {
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
		if _, err := f.Post(gone, rp, remote.URL+"/api", header, "a=b", 1<<20); err != ErrRemote {
			t.Fatalf("cancelled: %v", err)
		}
	}
	if err := f.blocked(rp, f.Now()); err != nil {
		t.Fatalf("clients that gave up blocked the remote: %v", err)
	}

	// A 404 is an answer for the caller; a 5xx is ErrRemote and blocks only its host.
	status = http.StatusNotFound
	if resp, err = f.Post(ctx, rp, remote.URL+"/api", header, "a=b", 1<<20); err != nil || resp.StatusCode != 404 {
		t.Fatalf("404: %v %v", resp, err)
	}
	resp.Body.Close()
	status = http.StatusBadGateway
	for range autoBlockAfter {
		if _, err := f.Post(ctx, rp, elsewhere.URL+"/api", header, "a=b", 1<<20); err != ErrRemote {
			t.Fatalf("5xx elsewhere: %v", err)
		}
	}
	if err := f.blocked(rp, f.Now()); err != nil {
		t.Fatalf("another host's failures blocked the remote: %v", err)
	}
	for range autoBlockAfter {
		if _, err := f.Post(ctx, rp, remote.URL+"/api", header, "a=b", 1<<20); err != ErrRemote {
			t.Fatalf("5xx: %v", err)
		}
	}
	before := asked
	if _, err := f.Post(ctx, rp, remote.URL+"/api", header, "a=b", 1<<20); err != ErrRemote || asked != before {
		t.Fatalf("auto-blocked: %v, asked %d times more", err, asked-before)
	}
}

// TestPostTarget: an API a remote's document names is asked only on the
// remote's host or its parent domain, over https unless it is the remote's own
// http origin; through a forward proxy no other name is checked, so an
// upstream's packages.json must not be able to point the POST at an internal
// host, directly or by a redirect.
func TestPostTarget(t *testing.T) {
	var proxied []string
	fwd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = append(proxied, r.Method+" "+r.Host)
		w.WriteHeader(http.StatusBadGateway) // a CONNECT that fails
	}))
	defer fwd.Close()
	viaProxy := func(r *http.Request) (*url.URL, error) {
		if r.URL.Hostname() == "127.0.0.1" {
			return nil, nil
		}
		return url.Parse(fwd.URL)
	}
	ctx := context.Background()
	header := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	f := New(nil, newClient(viaProxy, resolvesTo("203.0.113.10")), 2)
	packagist := &repo.Repository{Repository: config.Repository{Name: "p"}, RemoteURL: &url.URL{Scheme: "https", Host: "repo.packagist.org", Path: "/"}}
	private := &repo.Repository{Repository: config.Repository{Name: "pp"}, RemoteURL: &url.URL{Scheme: "https", Host: "repo.packagist.com", Path: "/acme/"}}
	for _, c := range []struct {
		rp     *repo.Repository
		target string
		ok     bool
	}{
		{packagist, "https://packagist.org/api/security-advisories/", true},
		{packagist, "https://repo.packagist.org/api/security-advisories/", true},
		{private, "https://repo.packagist.com/acme/api/security-advisories/", true},
		{private, "https://packagist.com/api/security-advisories/", true},
		{packagist, "http://internal-admin.corp/reset", false},
		{packagist, "https://internal-admin.corp/reset", false},
		{packagist, "http://packagist.org/api/security-advisories/", false},
		{packagist, "https://org/api/", false},
		{packagist, "https://evil.packagist.org/api/", false},
		{packagist, "https://packagist.org.evil.example/api/", false},
		{packagist, "https://packagist.org:8443/api/", true},
	} {
		proxied = nil
		resp, err := f.Post(ctx, c.rp, c.target, header, "a=b", 1<<20)
		if resp != nil {
			resp.Body.Close()
		}
		if asked := len(proxied) > 0; asked != c.ok {
			t.Errorf("%s for %s: asked %v (%v)", c.target, c.rp.RemoteURL, proxied, err)
		}
	}

	// A redirect is held to the same rule.
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://internal-admin.corp/reset", http.StatusTemporaryRedirect)
	}))
	defer remote.Close()
	u, _ := url.Parse(remote.URL + "/")
	direct := &repo.Repository{Repository: config.Repository{Name: "d"}, RemoteURL: u}
	proxied = nil
	resp, err := f.Post(ctx, direct, remote.URL+"/api", header, "a=b", 1<<20)
	if resp != nil {
		resp.Body.Close()
	}
	if len(proxied) > 0 || err == nil {
		t.Fatalf("redirect followed: %v %v", proxied, err)
	}
}

// TestForwardTooLarge: an answer over the limit is refused before the headers
// when its length is known, and fails the copy when it is found later (chunked):
// the caller must not end it as a complete answer. Post's body fails likewise.
func TestForwardTooLarge(t *testing.T) {
	chunked := false
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if !chunked {
			w.Header().Set("Content-Length", "11")
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // no Content-Length when chunked
		io.WriteString(w, "01234567890")
	}))
	defer remote.Close()
	u, _ := url.Parse(remote.URL + "/")
	rp := &repo.Repository{Repository: config.Repository{Name: "p"}, RemoteURL: u}
	f := New(nil, remote.Client(), 1)
	ctx := context.Background()
	for _, chunked = range []bool{false, true} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}"))
		err := f.Forward(ctx, rp, w, r, "api", 10)
		if err == nil {
			t.Errorf("chunked %v: a cut answer ended cleanly: %d %q", chunked, w.Code, w.Body)
		}
		if !chunked && w.Body.Len() > 0 {
			t.Errorf("known length: written %q", w.Body)
		}

		resp, err := f.Post(ctx, rp, remote.URL+"/api", nil, "a=b", 10)
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		if err == nil {
			t.Errorf("chunked %v: Post's answer over the limit read without an error", chunked)
		}
	}
}

// TestPostAndForwardTakeASlot: both hold a fetch slot until their answer is
// read, a stalled answer is cut after bodyIdle, and a client that gives up
// while queued or reading gives the slot back.
func TestPostAndForwardTakeASlot(t *testing.T) {
	defer func(d time.Duration) { bodyIdle = d }(bodyIdle)
	bodyIdle = 200 * time.Millisecond
	release := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "{")
		w.(http.Flusher).Flush()
		select { // stall
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer remote.Close()
	defer close(release)
	u, _ := url.Parse(remote.URL + "/")
	rp := &repo.Repository{Repository: config.Repository{Name: "p"}, RemoteURL: u}
	f := New(nil, remote.Client(), 1)
	ctx := context.Background()

	resp, err := f.Post(ctx, rp, remote.URL+"/api", nil, "a=b", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := f.Slots(); n != 1 {
		t.Fatalf("Post holds %d slots while its answer is open", n)
	}
	// The slot is taken: a second request waits, and gives up with its client.
	queued, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := f.Post(queued, rp, remote.URL+"/api", nil, "a=b", 1<<20); err == nil {
		t.Fatal("a second Post ran past the only slot")
	}
	t0 := time.Now()
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil || time.Since(t0) > 5*time.Second {
		t.Fatalf("stalled answer: %v after %v", err, time.Since(t0))
	}
	if n, _ := f.Slots(); n != 0 {
		t.Fatalf("%d slots held after Close", n)
	}

	t0 = time.Now()
	w := httptest.NewRecorder()
	if err := f.Forward(ctx, rp, w, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}")), "api", 1<<20); err == nil || time.Since(t0) > 5*time.Second {
		t.Fatalf("stalled Forward: %v after %v", err, time.Since(t0))
	}
	if n, _ := f.Slots(); n != 0 {
		t.Fatalf("%d slots held after Forward", n)
	}

	// A client that gives up while reading gives the slot back.
	gone, stop := context.WithCancel(ctx)
	resp, err = f.Post(gone, rp, remote.URL+"/api", nil, "a=b", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if n, _ := f.Slots(); n != 0 {
		t.Fatalf("%d slots held after the client gave up", n)
	}
}
