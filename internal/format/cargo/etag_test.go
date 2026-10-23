package cargo

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestIndexIgnoresIfModifiedSince(t *testing.T) {
	e := newEnv(t)
	e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(publishCrate(t, helloMeta, []byte("c1"))))
	w := e.get("cargo-hosted", "sm/ok/smoke-hello", map[string]string{"If-Modified-Since": "Fri, 01 Jan 2100 00:00:00 GMT"})
	if w.Code != 200 || w.Header().Get("Last-Modified") != "" {
		t.Fatalf("code=%d Last-Modified=%q", w.Code, w.Header().Get("Last-Modified"))
	}
}
