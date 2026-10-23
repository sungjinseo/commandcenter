package cargo

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

func TestPublishYankUnyank(t *testing.T) {
	e := newEnv(t)
	body := publishCrate(t, helloMeta, []byte("hello crate"))
	w := e.do("PUT", "cargo-all", "api/v1/crates/new", bytes.NewReader(body))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"warnings"`) {
		t.Fatalf("publish via group: %d %s", w.Code, w.Body.String())
	}
	l := lines(e.do("GET", "cargo-all", "sm/ok/smoke-hello", nil).Body.String())
	if len(l) != 1 || l[0]["cksum"] != sum(string(testCrate("smoke-hello", "1.0.0", "hello crate"))) || l[0]["yanked"] != false {
		t.Fatalf("index after publish: %v", l)
	}
	if w := e.do("GET", "cargo-all", "crates/smoke-hello/1.0.0/download", nil); w.Body.String() != string(testCrate("smoke-hello", "1.0.0", "hello crate")) {
		t.Fatalf("download: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(body)); w.Code != 409 {
		t.Fatalf("republish: %d %s", w.Code, w.Body.String())
	}

	if w := e.do("DELETE", "cargo-all", "api/v1/crates/smoke-hello/1.0.0/yank", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("yank: %d %s", w.Code, w.Body.String())
	}
	if l := lines(e.do("GET", "cargo-hosted", "sm/ok/smoke-hello", nil).Body.String()); l[0]["yanked"] != true {
		t.Fatalf("after yank: %v", l)
	}
	if w := e.do("PUT", "cargo-hosted", "api/v1/crates/smoke-hello/1.0.0/unyank", nil); w.Code != 200 {
		t.Fatalf("unyank: %d", w.Code)
	}
	if l := lines(e.do("GET", "cargo-hosted", "sm/ok/smoke-hello", nil).Body.String()); l[0]["yanked"] != false {
		t.Fatalf("after unyank: %v", l)
	}
	if w := e.do("DELETE", "cargo-hosted", "api/v1/crates/smoke-hello/9.9.9/yank", nil); w.Code != 404 {
		t.Fatalf("yank unknown: %d", w.Code)
	}
	if w := e.do("PUT", "cargo-hosted", "api/v1/crates/smoke-hello/1.0.0/yank", nil); w.Code != 405 {
		t.Fatalf("yank with PUT: %d", w.Code)
	}
}

func TestPublishRejects(t *testing.T) {
	e := newEnv(t)
	full := publishCrate(t, helloMeta, []byte("hello crate"))
	cases := map[string][]byte{
		"truncated crate": full[:len(full)-3],
		"bad json":        publishCrate(t, "nope", []byte("x")),
		"empty crate":     publishCrate(t, helloMeta, nil),
	}
	for name, body := range cases {
		if w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(body)); w.Code != 400 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := e.do("GET", "cargo-hosted", "sm/ok/smoke-hello", nil); w.Code != 404 {
		t.Fatalf("rejected publishes must leave nothing: %d", w.Code)
	}
	old := maxCrate
	maxCrate = 5
	defer func() { maxCrate = old }()
	if w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(full)); w.Code != 413 {
		t.Fatalf("oversized crate: %d %s", w.Code, w.Body.String())
	}
	maxCrate = old
	e.repos["cargo-hosted"].WritePolicy = "deny"
	if w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(full)); w.Code != 400 {
		t.Fatalf("deny: %d", w.Code)
	}
}

func TestPublishRejectsOversizedIndexLine(t *testing.T) {
	e := newEnv(t)
	old := maxLine
	maxLine = 200
	defer func() { maxLine = old }()
	body := publishCrate(t, helloMeta, []byte("hello crate"))
	if w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(body)); w.Code != 413 {
		t.Fatalf("oversized index line: %d %s", w.Code, w.Body.String())
	}
	maxLine = old
	if w := e.do("GET", "cargo-hosted", "sm/ok/smoke-hello", nil); w.Code != 404 {
		t.Fatalf("rejected publish must leave nothing: %d", w.Code)
	}
}

func TestPublishCratesIODuplicateRules(t *testing.T) {
	e := newEnv(t)
	meta := func(name, vers string) string {
		return `{"name":"` + name + `","vers":"` + vers + `","deps":[],"features":{}}`
	}
	if w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(publishCrate(t, meta("foo-bar", "1.0.0+a"), []byte("c1")))); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	for name, body := range map[string]string{
		"underscore variant": meta("foo_bar", "2.0.0"),
		"case variant":       meta("Foo-Bar", "2.0.0"),
		"build metadata":     meta("foo-bar", "1.0.0+b"),
	} {
		if w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(publishCrate(t, body, []byte("c2")))); w.Code != 409 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	// A rejected publish must not block later valid ones.
	if w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(publishCrate(t, meta("foo-bar", "2.0.0"), []byte("c3")))); w.Code != 200 {
		t.Fatalf("foo-bar 2.0.0 after conflicts: %d %s", w.Code, w.Body.String())
	}
	e.do("DELETE", "cargo-hosted", "api/v1/crates/foo-bar/1.0.0+a/yank", nil)
	pkgs, _ := e.h.Store.DB.ListPackages(context.Background(), "cargo-hosted", "foo-bar")
	if len(pkgs) != 2 || !pkgs[0].Yanked {
		t.Fatalf("yanked column not synced: %+v", pkgs)
	}
}

func TestPublishMixedCaseCrateKeepsPublishing(t *testing.T) {
	e := newEnv(t)
	for _, vers := range []string{"1.0.0", "1.1.0"} {
		body := publishCrate(t, `{"name":"Foo-Bar","vers":"`+vers+`","deps":[],"features":{}}`, []byte("c"+vers))
		if w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(body)); w.Code != 200 {
			t.Fatalf("Foo-Bar %s: %d %s", vers, w.Code, w.Body.String())
		}
	}
	yanked := func() bool {
		pkgs, _ := e.h.Store.DB.ListPackages(context.Background(), "cargo-hosted", "foo-bar")
		for _, p := range pkgs {
			if p.Version == "1.0.0" {
				return p.Yanked
			}
		}
		t.Fatal("1.0.0 missing")
		return false
	}
	e.do("DELETE", "cargo-hosted", "api/v1/crates/foo-bar/1.0.0/yank", nil)
	if !yanked() {
		t.Fatal("yank did not set the column")
	}
	e.do("PUT", "cargo-hosted", "api/v1/crates/foo-bar/1.0.0/unyank", nil)
	if yanked() {
		t.Fatal("unyank did not clear the column")
	}
}

func TestCheckDuplicateWithManyCrates(t *testing.T) {
	e := newEnv(t)
	rp := e.repos["cargo-hosted"]
	ctx := context.Background()
	err := e.h.Store.DB.Tx(ctx, func(tx *meta.Tx) error {
		for i := 0; i < 500; i++ {
			name := fmt.Sprintf("crate-%03d", i)
			attrs := fmt.Sprintf(`{"name":%q,"vers":"1.0.0"}`, name)
			if err := tx.InsertPackage(ctx, meta.Package{Repo: rp.Name, Format: "cargo", Name: name, Version: "1.0.0", Attrs: attrs}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, vers string
		want       int
	}{
		{"crate-250", "1.0.0", 409},   // same version
		{"crate-250", "1.0.0+b", 409}, // build metadata ignored
		{"crate-250", "2.0.0", 0},     // new version of an existing crate
		{"Crate-250", "2.0.0", 409},   // different spelling
		{"crate_250", "2.0.0", 409},   // - vs _
		{"crate-500", "1.0.0", 0},     // new crate
		{"crate", "1.0.0", 0},         // prefix of other names is not a match
	}
	for _, c := range cases {
		err := e.h.checkDuplicate(ctx, rp, publishMeta{Name: c.name, Vers: c.vers})
		got := 0
		if err != nil {
			got = repo.StatusOf(err)
		}
		if got != c.want {
			t.Errorf("%s@%s: status %d (%v), want %d", c.name, c.vers, got, err, c.want)
		}
	}
}
