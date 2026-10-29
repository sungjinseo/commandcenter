package blob_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/blob/s3test"
)

// groupOf opens a group over the named stores at a fresh root.
func groupOf(t *testing.T, cfg blob.GroupConfig, stores map[string]*blob.Store) *blob.Store {
	t.Helper()
	g, err := blob.OpenGroup(t.TempDir(), cfg, func(n string) *blob.Store { return stores[n] })
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func fileStore(t *testing.T) *blob.Store {
	t.Helper()
	s, err := blob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func has(s *blob.Store, sha string) bool {
	_, err := s.Stat(sha)
	return err == nil
}

func readAll(t *testing.T, s *blob.Store, sha string) string {
	t.Helper()
	f, err := s.Open(sha)
	if err != nil {
		t.Fatalf("open %s: %v", sha, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func members(names ...string) []blob.GroupMember {
	var out []blob.GroupMember
	for _, n := range names {
		ro := strings.HasSuffix(n, "!")
		out = append(out, blob.GroupMember{Name: strings.TrimSuffix(n, "!"), ReadOnly: ro})
	}
	return out
}

// Round robin spreads distinct blobs over the writable members, the same
// content goes back to the member that holds it, and every blob reads back
// through the group; writeToFirst fills the first writable member only.
func TestGroupFillPolicies(t *testing.T) {
	a, b := fileStore(t), fileStore(t)
	stores := map[string]*blob.Store{"a": a, "b": b}
	g := groupOf(t, blob.GroupConfig{Members: members("a", "b")}, stores)
	if cfg, ok := g.Group(); !ok || cfg.FillPolicy != blob.FillRoundRobin {
		t.Fatalf("Group() = %+v, %v; want the default round robin", cfg, ok)
	}
	var shas []string
	for _, s := range []string{"one", "two", "three", "four"} {
		sums, err := g.Write(strings.NewReader(s))
		if err != nil {
			t.Fatal(err)
		}
		shas = append(shas, sums.SHA256)
	}
	inA, inB := 0, 0
	for i, sha := range shas {
		if has(a, sha) {
			inA++
		}
		if has(b, sha) {
			inB++
		}
		if got := readAll(t, g, sha); got != []string{"one", "two", "three", "four"}[i] {
			t.Fatalf("read back %q", got)
		}
	}
	if inA != 2 || inB != 2 {
		t.Fatalf("round robin: %d in a, %d in b; want 2 and 2", inA, inB)
	}
	// The same content again is refreshed where it is, not copied.
	if _, err := g.Write(strings.NewReader("one")); err != nil {
		t.Fatal(err)
	}
	if has(a, shas[0]) == has(b, shas[0]) {
		t.Fatal("a re-upload was stored in a second member")
	}

	c, d := fileStore(t), fileStore(t)
	w := groupOf(t, blob.GroupConfig{Members: members("c", "d"), FillPolicy: blob.FillWriteToFirst}, map[string]*blob.Store{"c": c, "d": d})
	for _, s := range []string{"x", "y", "z"} {
		sums, err := w.Write(strings.NewReader(s))
		if err != nil {
			t.Fatal(err)
		}
		if !has(c, sums.SHA256) || has(d, sums.SHA256) {
			t.Fatalf("writeToFirst put %q elsewhere than the first member", s)
		}
	}
}

// A read-only member is read but never written; a blob only one member holds
// (written there directly) is found without a hint; Walk and Usage cover
// every member, copies included; Remove deletes every copy.
func TestGroupReadOnlyAndReads(t *testing.T) {
	a, b := fileStore(t), fileStore(t)
	old, err := a.Write(strings.NewReader("drained"))
	if err != nil {
		t.Fatal(err)
	}
	g := groupOf(t, blob.GroupConfig{Members: members("a!", "b")}, map[string]*blob.Store{"a": a, "b": b})
	if got := readAll(t, g, old.SHA256); got != "drained" {
		t.Fatalf("read from the read-only member: %q", got)
	}
	for _, s := range []string{"p", "q", "r"} {
		sums, err := g.Write(strings.NewReader(s))
		if err != nil {
			t.Fatal(err)
		}
		if has(a, sums.SHA256) || !has(b, sums.SHA256) {
			t.Fatalf("%q went to the read-only member", s)
		}
	}
	// The drained blob written through the group lands in b too: a holds a
	// read-only copy, so the group now has two.
	if _, err := g.Write(strings.NewReader("drained")); err != nil {
		t.Fatal(err)
	}
	if !has(a, old.SHA256) || !has(b, old.SHA256) {
		t.Fatal("want a copy in each member")
	}
	n := 0
	if err := g.Walk(func(sha string, _ time.Time) error {
		if sha == old.SHA256 {
			n++
		}
		return nil
	}); err != nil || n != 2 {
		t.Fatalf("Walk saw the copied blob %d times (%v), want 2", n, err)
	}
	blobs, bytes, err := g.Usage(context.Background())
	ua, ba, _ := a.Usage(context.Background())
	ub, bb, _ := b.Usage(context.Background())
	if err != nil || blobs != ua+ub || bytes != ba+bb || blobs != 5 || bytes != int64(3+2*len("drained")) {
		t.Fatalf("Usage = %d blobs, %d bytes, %v; members %d+%d, %d+%d", blobs, bytes, err, ua, ub, ba, bb)
	}
	if err := g.Remove(old.SHA256); err != nil {
		t.Fatal(err)
	}
	if has(a, old.SHA256) || has(b, old.SHA256) {
		t.Fatal("Remove left a copy")
	}
	if _, err := g.Open(old.SHA256); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Open after Remove: %v", err)
	}
}

// SetGroup returns only when the writes that picked a member under the old
// configuration have committed: after it marks a member read-only, nothing
// more lands there.
func TestGroupSetWaitsForWrites(t *testing.T) {
	a, b := fileStore(t), fileStore(t)
	stores := map[string]*blob.Store{"a": a, "b": b}
	g := groupOf(t, blob.GroupConfig{Members: members("a", "b"), FillPolicy: blob.FillWriteToFirst}, stores)
	entered, release := make(chan struct{}), make(chan struct{})
	orig := *blob.Rename
	*blob.Rename = func(from, to string) error {
		close(entered)
		<-release
		return orig(from, to)
	}
	t.Cleanup(func() { *blob.Rename = orig })
	wrote := make(chan blob.Sums, 1)
	go func() {
		sums, err := g.Write(strings.NewReader("in flight"))
		if err != nil {
			t.Error(err)
		}
		wrote <- sums
	}()
	<-entered
	set := make(chan error, 1)
	go func() {
		set <- g.SetGroup(blob.GroupConfig{Members: members("a!", "b")}, func(n string) *blob.Store { return stores[n] })
	}()
	select {
	case err := <-set:
		t.Fatalf("SetGroup returned (%v) while a write to a was committing", err)
	case <-time.After(100 * time.Millisecond):
	}
	*blob.Rename = orig
	close(release)
	if err := <-set; err != nil {
		t.Fatal(err)
	}
	if sums := <-wrote; !has(a, sums.SHA256) {
		t.Fatal("the in-flight write did not land in a")
	}
	sums, err := g.Write(strings.NewReader("after"))
	if err != nil || has(a, sums.SHA256) || !has(b, sums.SHA256) {
		t.Fatalf("a write after SetGroup: %v; in a %v", err, has(a, sums.SHA256))
	}
}

// A temp file on another filesystem than the member is copied in, through
// the member's own tmp, which is left empty.
func TestGroupCommitAcrossFilesystems(t *testing.T) {
	root := t.TempDir()
	a, err := blob.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	g := groupOf(t, blob.GroupConfig{Members: members("a")}, map[string]*blob.Store{"a": a})
	orig := *blob.Rename
	calls := 0
	*blob.Rename = func(from, to string) error {
		calls++
		if calls == 1 {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EXDEV}
		}
		return orig(from, to)
	}
	t.Cleanup(func() { *blob.Rename = orig })
	sums, err := g.Write(strings.NewReader("copied in"))
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, a, sums.SHA256); got != "copied in" || calls != 1 {
		t.Fatalf("member holds %q after %d renames", got, calls)
	}
	if left, _ := os.ReadDir(filepath.Join(root, "tmp")); len(left) != 0 {
		t.Fatalf("member tmp holds %v", left)
	}
}

// A group over a file and an S3 member spreads writes over both, reads each
// back, and its health covers both members.
func TestGroupFileAndS3Members(t *testing.T) {
	srv := s3test.New(t)
	f, s := fileStore(t), openS3(t, srv, nil)
	g := groupOf(t, blob.GroupConfig{Members: members("file", "cloud")}, map[string]*blob.Store{"file": f, "cloud": s})
	one, err := g.Write(strings.NewReader("first"))
	if err != nil {
		t.Fatal(err)
	}
	two, err := g.Write(strings.NewReader("second"))
	if err != nil {
		t.Fatal(err)
	}
	if !has(f, one.SHA256) || !has(s, two.SHA256) {
		t.Fatalf("want one blob in each member; bucket keys %v", srv.Keys())
	}
	if readAll(t, g, one.SHA256) != "first" || readAll(t, g, two.SHA256) != "second" {
		t.Fatal("read back")
	}
	probes := g.Health()
	if len(probes) != 2 || !probes[0].OK || !probes[1].OK || !strings.HasPrefix(probes[0].Target, "member file: directory ") ||
		!strings.HasPrefix(probes[1].Target, "member cloud: primary ") {
		t.Fatalf("health %+v", probes)
	}
	if err := g.Check(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupConfigJSONAndValidate(t *testing.T) {
	var c blob.GroupConfig
	if err := json.Unmarshal([]byte(`{"members":["a",{"name":"b","readOnly":true}],"fillPolicy":"writeToFirst"}`), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Members) != 2 || c.Members[0] != (blob.GroupMember{Name: "a"}) || c.Members[1] != (blob.GroupMember{Name: "b", ReadOnly: true}) {
		t.Fatalf("decoded %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		cfg  blob.GroupConfig
		want string
	}{
		{blob.GroupConfig{}, "at least one member"},
		{blob.GroupConfig{Members: members("a", "a")}, "listed twice"},
		{blob.GroupConfig{Members: members("a!", "b!")}, "writable"},
		{blob.GroupConfig{Members: members("a"), FillPolicy: "random"}, "fillPolicy"},
	} {
		if err := tc.cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v, want %q", tc.cfg, err, tc.want)
		}
	}
	a := fileStore(t)
	g := groupOf(t, blob.GroupConfig{Members: members("a")}, map[string]*blob.Store{"a": a})
	if _, err := blob.OpenGroup(t.TempDir(), blob.GroupConfig{Members: members("g")}, func(string) *blob.Store { return g }); err == nil ||
		!strings.Contains(err.Error(), "do not nest") {
		t.Fatalf("nested group: %v", err)
	}
	if _, err := blob.OpenGroup(t.TempDir(), blob.GroupConfig{Members: members("gone")}, func(string) *blob.Store { return nil }); err == nil {
		t.Fatal("a missing member was accepted")
	}
	if err := a.SetGroup(blob.GroupConfig{Members: members("a")}, nil); err == nil {
		t.Fatal("SetGroup on a file store")
	}
}
