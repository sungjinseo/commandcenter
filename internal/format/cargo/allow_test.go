package cargo

import (
	"net/http"
	"strings"
	"testing"

	"commandcenter/internal/repo"
)

// Final review (Task 7): a cargo 405 lists the methods the resource accepts.
func TestMethodNotAllowedListsTrueMethods(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		method, repo, path string
		allow              []string // nil: an empty Allow header
	}{
		{"GET", "cargo-hosted", "api/v1/crates/new", []string{"PUT"}},
		{"PUT", "cargo-hosted", "api/v1/crates/itoa/1.0.0/yank", []string{"DELETE"}},
		{"GET", "cargo-all", "api/v1/crates/itoa/1.0.0/unyank", []string{"PUT"}},
		{"POST", "cargo-hosted", "it/oa/itoa", []string{"GET, HEAD"}},
		{"DELETE", "cargo-proxy", "crates/itoa/1.0.0/download", []string{"GET, HEAD"}},
		{"PUT", "cargo-proxy", "api/v1/crates/new", []string{""}},
		{"GET", "cargo-proxy", "api/v1/crates/new", []string{""}},
	} {
		w := e.do(c.method, c.repo, c.path, nil)
		if got, ok := w.Header()["Allow"]; w.Code != http.StatusMethodNotAllowed || !ok || strings.Join(got, ",") != c.allow[0] {
			t.Errorf("%s %s/%s: %d Allow %q (set %v), want 405 and %q", c.method, c.repo, c.path, w.Code, got, ok, c.allow[0])
		}
	}
}

// A proxy member whose upstream answers 404 is "not found", not an error:
// ordered before the hosted member that has the crate, the group falls
// through to hosted (pins the fail-closed classification, Task 6 ruling).
func TestGroupProxyNotFoundAheadOfHostedFallsThrough(t *testing.T) {
	e := newEnv(t)
	e.seedHosted(t, "internal", "1.0.0", "internal crate")
	g := *e.repos["cargo-all"]
	g.MemberRepos = []*repo.Repository{e.repos["cargo-proxy"], e.repos["cargo-hosted"]}
	e.repos["cargo-px-first"] = &g
	l := lines(e.do("GET", "cargo-px-first", "in/te/internal", nil).Body.String())
	if len(l) != 1 || l[0]["vers"] != "1.0.0" || l[0]["cksum"] != sum("internal crate") {
		t.Fatalf("index: %v", l)
	}
	if w := e.do("GET", "cargo-px-first", "crates/internal/1.0.0/download", nil); w.Code != 200 || w.Body.String() != "internal crate" {
		t.Fatalf("download: %d %q", w.Code, w.Body.String())
	}
}
