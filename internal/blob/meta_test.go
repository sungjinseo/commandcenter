package blob_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/blob/s3test"
)

// checkMeta runs the meta object contract on s: put, get, a lexical listing
// by prefix, remove, and walk never reporting a meta object.
func checkMeta(t *testing.T, s *blob.Store) {
	t.Helper()
	keys := []string{"journal/2026/10/30/09/b.json", "journal/2026/10/30/09/a.json", "journal/2026/10/30/10/c.json", "journal.x", "other/z"}
	for _, k := range keys {
		if err := s.PutMeta(k, []byte("data of "+k)); err != nil {
			t.Fatalf("PutMeta(%s): %v", k, err)
		}
	}
	if err := s.PutMeta("journal/2026/10/30/09/a.json", []byte("replaced")); err != nil {
		t.Fatal(err)
	}
	if b, err := s.GetMeta("journal/2026/10/30/09/a.json"); err != nil || string(b) != "replaced" {
		t.Fatalf("GetMeta = %q, %v", b, err)
	}
	if _, err := s.GetMeta("journal/missing.json"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("GetMeta(missing) = %v, want ErrNotFound", err)
	}
	list := func(prefix string) []string {
		var out []string
		if err := s.ListMeta(prefix, func(k string) error { out = append(out, k); return nil }); err != nil {
			t.Fatalf("ListMeta(%q): %v", prefix, err)
		}
		return out
	}
	want := []string{"journal.x", "journal/2026/10/30/09/a.json", "journal/2026/10/30/09/b.json", "journal/2026/10/30/10/c.json", "other/z"}
	if got := list(""); !slices.Equal(got, want) {
		t.Fatalf("ListMeta(\"\") = %v, want %v", got, want)
	}
	if got := list("journal/2026/10/30/09/"); !slices.Equal(got, want[1:3]) {
		t.Fatalf("ListMeta(hour) = %v", got)
	}
	if got := list("journal/2026/10/30/1"); !slices.Equal(got, want[3:4]) {
		t.Fatalf("ListMeta(partial segment) = %v", got)
	}
	stop := errors.New("stop")
	n := 0
	if err := s.ListMeta("", func(string) error { n++; return stop }); !errors.Is(err, stop) || n != 1 {
		t.Fatalf("ListMeta stops on fn's error: %v after %d", err, n)
	}
	sums, err := s.Write(strings.NewReader("a blob"))
	if err != nil {
		t.Fatal(err)
	}
	var walked []string
	if err := s.Walk(func(sha string, _ time.Time) error { walked = append(walked, sha); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(walked, []string{sums.SHA256}) {
		t.Fatalf("Walk = %v, want only the blob %s", walked, sums.SHA256)
	}
	if blobs, _, err := s.Usage(t.Context()); err != nil || blobs != 1 {
		t.Fatalf("Usage = %d, %v: meta objects must not count", blobs, err)
	}
	for _, k := range append(keys, "journal/never.json") {
		if err := s.RemoveMeta(k); err != nil {
			t.Fatalf("RemoveMeta(%s): %v", k, err)
		}
	}
	if got := list(""); len(got) != 0 {
		t.Fatalf("after RemoveMeta: %v", got)
	}
	for _, bad := range []string{"", "/abs", "a/../b", "a//b", "a/", "sp ace", "ü", "./a"} {
		if err := s.PutMeta(bad, nil); err == nil {
			t.Errorf("PutMeta(%q) accepted", bad)
		}
	}
	if err := s.ListMeta("../", func(string) error { return nil }); err == nil {
		t.Error("ListMeta(../) accepted")
	}
}

func TestMetaFile(t *testing.T) {
	s, err := blob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkMeta(t, s)
}

func TestMetaS3(t *testing.T) {
	srv := s3test.New(t)
	s := openS3(t, srv, func(c *blob.S3Config) { c.Prefix = "cc/store" })
	checkMeta(t, s)
	srv.Put("cc/store/meta/journal/x.json", []byte("{}"), time.Now())
	if b, err := s.GetMeta("journal/x.json"); err != nil || string(b) != "{}" {
		t.Fatalf("the object lives at <prefix>/meta/<key>: %q, %v", b, err)
	}
	// Pages of the listing follow the continuation token.
	srv.MaxKeys = 2
	for _, k := range []string{"p/1", "p/2", "p/3", "p/4", "p/5"} {
		if err := s.PutMeta(k, []byte(k)); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	if err := s.ListMeta("p/", func(k string) error { got = append(got, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"p/1", "p/2", "p/3", "p/4", "p/5"}) {
		t.Fatalf("paged ListMeta = %v", got)
	}
}

func TestMetaGroup(t *testing.T) {
	a, _ := blob.Open(t.TempDir())
	b, _ := blob.Open(t.TempDir())
	c, _ := blob.Open(t.TempDir())
	stores := map[string]*blob.Store{"a": a, "b": b, "c": c}
	g, err := blob.OpenGroup(t.TempDir(), blob.GroupConfig{Members: []blob.GroupMember{{Name: "a"}, {Name: "b"}}}, func(n string) *blob.Store { return stores[n] })
	if err != nil {
		t.Fatal(err)
	}
	checkMeta(t, g)
	// Every writable member gets a copy; a read-only member is read, not written.
	if err := g.SetGroup(blob.GroupConfig{Members: []blob.GroupMember{{Name: "a"}, {Name: "b", ReadOnly: true}, {Name: "c"}}}, func(n string) *blob.Store { return stores[n] }); err != nil {
		t.Fatal(err)
	}
	if err := b.PutMeta("j/old", []byte("from b")); err != nil {
		t.Fatal(err)
	}
	if err := g.PutMeta("j/new", []byte("new")); err != nil {
		t.Fatal(err)
	}
	for name, s := range stores {
		_, err := s.GetMeta("j/new")
		if (name == "b") != errors.Is(err, blob.ErrNotFound) {
			t.Errorf("member %s: GetMeta(j/new) = %v", name, err)
		}
	}
	var got []string
	if err := g.ListMeta("j/", func(k string) error { got = append(got, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"j/new", "j/old"}) {
		t.Fatalf("group ListMeta = %v, want the union", got)
	}
	if d, err := g.GetMeta("j/old"); err != nil || string(d) != "from b" {
		t.Fatalf("group GetMeta from a read-only member = %q, %v", d, err)
	}
	if err := g.RemoveMeta("j/new"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetMeta("j/new"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("RemoveMeta left member c's copy: %v", err)
	}
}
