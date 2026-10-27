package cargo

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func (e *env) get(repoName, path string, hdr map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/repository/"+repoName+"/x", nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

func TestHostedIndexETag(t *testing.T) {
	e := newEnv(t)
	e.h.Now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	if w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(publishCrate(t, helloMeta, []byte("c1")))); w.Code != 200 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	w := e.get("cargo-hosted", "sm/ok/smoke-hello", nil)
	etag := w.Header().Get("ETag")
	if w.Code != 200 || !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || len(etag) < 10 {
		t.Fatalf("index: %d etag=%q", w.Code, etag)
	}
	body := w.Body.String()

	for _, inm := range []string{etag, "*", `"nope", ` + etag, "W/" + etag} {
		w := e.get("cargo-hosted", "sm/ok/smoke-hello", map[string]string{"If-None-Match": inm})
		if w.Code != 304 || w.Body.Len() != 0 || w.Header().Get("ETag") != etag {
			t.Errorf("If-None-Match %q: %d body=%q etag=%q", inm, w.Code, w.Body.String(), w.Header().Get("ETag"))
		}
	}
	if w := e.get("cargo-hosted", "sm/ok/smoke-hello", map[string]string{"If-None-Match": `"other"`}); w.Code != 200 || w.Body.String() != body {
		t.Fatalf("mismatched ETag: %d", w.Code)
	}

	// yank changes the ETag
	if w := e.do("DELETE", "cargo-hosted", "api/v1/crates/smoke-hello/1.0.0/yank", nil); w.Code != 200 {
		t.Fatalf("yank: %d", w.Code)
	}
	w = e.get("cargo-hosted", "sm/ok/smoke-hello", map[string]string{"If-None-Match": etag})
	if w.Code != 200 || w.Header().Get("ETag") == etag {
		t.Fatalf("yank must change ETag: %d %q", w.Code, w.Header().Get("ETag"))
	}
	etag2 := w.Header().Get("ETag")

	// a new version changes it too
	e.h.Now = func() time.Time { return time.Date(2026, 2, 2, 3, 4, 5, 0, time.UTC) }
	meta2 := strings.Replace(helloMeta, `"vers":"1.0.0"`, `"vers":"1.1.0"`, 1)
	if w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(publishCrate(t, meta2, []byte("c2")))); w.Code != 200 {
		t.Fatalf("publish 2: %d %s", w.Code, w.Body.String())
	}
	w = e.get("cargo-hosted", "sm/ok/smoke-hello", map[string]string{"If-None-Match": etag2})
	if w.Code != 200 || w.Header().Get("ETag") == etag2 {
		t.Fatalf("publish must change ETag: %d %q", w.Code, w.Header().Get("ETag"))
	}

	if w := e.get("cargo-hosted", "sm/ok/nonexistent", map[string]string{"If-None-Match": "*"}); w.Code != http.StatusNotFound {
		t.Fatalf("unknown crate with If-None-Match *: %d", w.Code)
	}
}

// TestIndexIgnoresIfModifiedSince: no repository type sends Last-Modified or
// answers If-Modified-Since (see index).
func TestIndexIgnoresIfModifiedSince(t *testing.T) {
	e := newEnv(t)
	e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(publishCrate(t, helloMeta, []byte("c1"))))
	for _, c := range [][2]string{{"cargo-hosted", "sm/ok/smoke-hello"}, {"cargo-proxy", "it/oa/itoa"}, {"cargo-all", "it/oa/itoa"}} {
		w := e.get(c[0], c[1], map[string]string{"If-Modified-Since": "Fri, 01 Jan 2100 00:00:00 GMT"})
		if w.Code != 200 || w.Header().Get("Last-Modified") != "" {
			t.Fatalf("%s: code=%d Last-Modified=%q", c[0], w.Code, w.Header().Get("Last-Modified"))
		}
	}
}

// TestProxyAndGroupIndexETag: proxy and group index files carry an ETag and
// answer If-None-Match like hosted ones (QA L-21). A group's ETag names the
// member that answers, so it changes when another member takes over.
func TestProxyAndGroupIndexETag(t *testing.T) {
	e := newEnv(t)
	for _, rp := range []string{"cargo-proxy", "cargo-all"} {
		w := e.get(rp, "it/oa/itoa", nil)
		etag, body := w.Header().Get("ETag"), w.Body.String()
		if w.Code != 200 || !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || len(etag) < 10 || len(lines(body)) != 2 {
			t.Fatalf("%s: %d etag=%q %s", rp, w.Code, etag, body)
		}
		for _, inm := range []string{etag, "*", `"nope", W/` + etag} {
			if w := e.get(rp, "it/oa/itoa", map[string]string{"If-None-Match": inm}); w.Code != 304 || w.Body.Len() != 0 || w.Header().Get("ETag") != etag {
				t.Errorf("%s If-None-Match %q: %d body=%q etag=%q", rp, inm, w.Code, w.Body.String(), w.Header().Get("ETag"))
			}
		}
		if w := e.get(rp, "it/oa/itoa", map[string]string{"If-None-Match": `"other"`}); w.Code != 200 || w.Body.String() != body || w.Header().Get("ETag") != etag {
			t.Errorf("%s mismatched ETag: %d etag=%q", rp, w.Code, w.Header().Get("ETag"))
		}
		if w := e.get(rp, "3/n/nop", map[string]string{"If-None-Match": "*"}); w.Code != http.StatusNotFound || w.Header().Get("ETag") != "" {
			t.Errorf("%s unknown crate with If-None-Match *: %d etag=%q", rp, w.Code, w.Header().Get("ETag"))
		}
	}
	old := e.get("cargo-all", "it/oa/itoa", nil).Header().Get("ETag")
	if old == e.get("cargo-proxy", "it/oa/itoa", nil).Header().Get("ETag") {
		t.Error("a group ETag must name the answering member")
	}
	e.seedHosted(t, "itoa", "1.0.0", "internal itoa")
	w := e.get("cargo-all", "it/oa/itoa", map[string]string{"If-None-Match": old})
	if l := lines(w.Body.String()); w.Code != 200 || w.Header().Get("ETag") == old || len(l) != 1 || l[0]["cksum"] != sum("internal itoa") {
		t.Fatalf("group after hosted took over: %d etag=%q %s", w.Code, w.Header().Get("ETag"), w.Body.String())
	}
}

// TestProxyIndexETagFollowsUpstream: a proxy's ETag is its cached file's
// checksum, so it stays while the upstream file is the same and changes with it.
func TestProxyIndexETagFollowsUpstream(t *testing.T) {
	e := newEnv(t)
	var body atomic.Value
	body.Store(`{"name":"itoa","vers":"1.0.0","deps":[],"cksum":"` + sum("a") + `","features":{},"yanked":false}` + "\n")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body.Load().(string)) }))
	defer ts.Close()
	rp := *e.repos["cargo-proxy"]
	rp.Name, rp.MetadataMaxAge = "cargo-live", 0 // revalidate on every request
	rp.RemoteURL, _ = url.Parse(ts.URL + "/")
	e.repos[rp.Name] = &rp
	etag := e.get(rp.Name, "it/oa/itoa", nil).Header().Get("ETag")
	if w := e.get(rp.Name, "it/oa/itoa", map[string]string{"If-None-Match": etag}); etag == "" || w.Code != 304 {
		t.Fatalf("unchanged upstream: %d etag=%q", w.Code, etag)
	}
	body.Store(body.Load().(string) + `{"name":"itoa","vers":"1.0.1","deps":[],"cksum":"` + sum("b") + `","features":{},"yanked":false}` + "\n")
	w := e.get(rp.Name, "it/oa/itoa", map[string]string{"If-None-Match": etag})
	if w.Code != 200 || w.Header().Get("ETag") == etag || len(lines(w.Body.String())) != 2 {
		t.Fatalf("changed upstream: %d etag=%q %s", w.Code, w.Header().Get("ETag"), w.Body.String())
	}
}
