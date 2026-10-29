package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// withGroup adds file stores a and b and the round-robin group g over them to
// h, and the hosted repository mvn-g on g; it returns the stores' roots.
func withGroup(t *testing.T, h *harness) map[string]string {
	t.Helper()
	ctx := context.Background()
	roots := map[string]string{}
	for _, n := range []string{"a", "b"} {
		roots[n] = t.TempDir()
		bs, err := blob.Open(roots[n])
		if err != nil {
			t.Fatal(err)
		}
		if err := h.db.CreateBlobStore(ctx, meta.BlobStore{Name: n, Path: roots[n]}); err != nil {
			t.Fatal(err)
		}
		h.store.AddStore(n, bs)
	}
	row := meta.BlobStore{Name: "g", Path: t.TempDir(), Type: meta.BlobStoreGroup,
		Group: &blob.GroupConfig{Members: []blob.GroupMember{{Name: "a"}, {Name: "b"}}, FillPolicy: blob.FillRoundRobin}}
	if err := h.db.CreateBlobStore(ctx, row); err != nil {
		t.Fatal(err)
	}
	g, err := repo.OpenGroup("", row, h.store.StoreByName)
	if err != nil {
		t.Fatal(err)
	}
	h.store.AddStore("g", g)
	if err := h.reg.Create(ctx, config.Repository{Name: "mvn-g", Format: "maven", Type: "hosted", BlobStore: "g"}); err != nil {
		t.Fatal(err)
	}
	h.store.StoreFor = func(name string) string {
		if rp := h.reg.Get(name); rp != nil {
			return repo.StoreOf(rp.Repository)
		}
		return ""
	}
	return roots
}

func groupMembers(t *testing.T, h *harness) []blob.GroupMember {
	t.Helper()
	row, err := h.db.GetBlobStore(context.Background(), "g")
	if err != nil {
		t.Fatal(err)
	}
	live, _ := h.store.StoreByName("g").Group()
	if fmt.Sprint(live.Members) != fmt.Sprint(row.Group.Members) {
		t.Fatalf("live members %v, stored %v", live.Members, row.Group.Members)
	}
	return row.Group.Members
}

// The removal task makes the member read-only, copies what only it holds to
// the other member (a corrupt copy fails the run and leaves the member in
// place), keeps every asset readable while it copies, then drops the member;
// verify-blobs passes afterwards.
func TestGroupRemoveMemberTask(t *testing.T) {
	h := newHarness(t)
	roots := withGroup(t, h)
	ctx := context.Background()
	want := map[string]string{}
	for i := range 6 {
		p := fmt.Sprintf("org/x/%d/x-%d.jar", i, i)
		want[p] = fmt.Sprintf("jar number %d", i)
		h.put(t, "mvn-g", p, want[p])
	}
	a, b := h.store.StoreByName("a"), h.store.StoreByName("b")
	// One blob in both members already: it is not copied again.
	for _, s := range []*blob.Store{a, b} {
		if _, err := s.Write(strings.NewReader("in both")); err != nil {
			t.Fatal(err)
		}
	}
	var onlyA []string
	a.Walk(func(sha string, _ time.Time) error {
		if _, err := b.Stat(sha); err != nil {
			onlyA = append(onlyA, sha)
		}
		return nil
	})
	if len(onlyA) != 3 {
		t.Fatalf("round robin left %d blobs only in a, want 3", len(onlyA))
	}

	for params, want := range map[string]string{
		`{"group":"g"}`:                    "params.group and params.member are required",
		`{"group":"g","member":"x"}`:       `"x" is not a member of the group "g"`,
		`{"group":"a","member":"b"}`:       `"a" is not a group`,
		`{"group":"nope","member":"a"}`:    `"nope" does not exist`,
		`{"group":"g","member":"a","x":1}`: "unknown field",
	} {
		if _, err := h.s.Create(ctx, Def{Name: "x", Type: TypeGroupRemove, Schedule: "manual", Params: json.RawMessage(params)}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want ~%q", params, err, want)
		}
	}

	// A corrupt copy (same size, other bytes) fails the run; a stays, read-only.
	bad := blobFile(roots["a"], onlyA[0])
	good, _ := os.ReadFile(bad)
	os.Chmod(bad, 0o644)
	if err := os.WriteFile(bad, []byte(strings.Repeat("?", len(good))), 0o644); err != nil {
		t.Fatal(err)
	}
	out, tk := h.run(t, TypeGroupRemove, `{"group":"g","member":"a"}`)
	if out.Result != ResultFailed || !strings.Contains(tk.LastLog, "it is corrupt") {
		t.Fatalf("corrupt member copy: %+v\n%s", out, tk.LastLog)
	}
	if m := groupMembers(t, h); len(m) != 2 || m[0] != (blob.GroupMember{Name: "a", ReadOnly: true}) {
		t.Fatalf("after the failed run: %+v", m)
	}
	if err := os.WriteFile(bad, good, 0o644); err != nil {
		t.Fatal(err)
	}

	// Reads keep working while the run is paused after its first copy, and
	// a write meanwhile goes to b.
	paused, resume := make(chan struct{}), make(chan struct{})
	first := true
	groupCopyHook = func() {
		if first {
			first = false
			close(paused)
			<-resume
		}
	}
	t.Cleanup(func() { groupCopyHook = nil })
	tk = h.create(t, Def{Name: "drain a", Type: TypeGroupRemove, Schedule: "manual", Params: json.RawMessage(`{"group":"g","member":"a"}`)})
	done := make(chan Outcome, 1)
	go func() {
		out, err := h.s.RunNow(tk.ID, "admin")
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	<-paused
	for p, body := range want {
		asset, err := h.db.GetAsset(ctx, "mvn-g", p)
		if err != nil {
			t.Fatal(err)
		}
		f, err := h.store.Open(asset)
		if err != nil {
			t.Fatalf("read %s during the copy: %v", p, err)
		}
		got, _ := io.ReadAll(f)
		f.Close()
		if string(got) != body {
			t.Fatalf("read %s during the copy: %q", p, got)
		}
	}
	h.put(t, "mvn-g", "org/x/new/x-new.jar", "written during the copy")
	fresh, _ := h.db.GetAsset(ctx, "mvn-g", "org/x/new/x-new.jar")
	if _, err := a.Stat(fresh.BlobSHA256); err == nil {
		t.Fatal("a write during the copy went to the read-only member")
	}
	close(resume)
	out = <-done
	tk = h.get(t, tk.ID)
	if out.Result != ResultOK || out.Removed != 3 || !strings.Contains(out.Message, "1 were there already") || !strings.Contains(out.Message, `"a" left the group`) {
		t.Fatalf("removal: %+v\n%s", out, tk.LastLog)
	}
	if m := groupMembers(t, h); len(m) != 1 || m[0].Name != "b" {
		t.Fatalf("after the removal: %+v", m)
	}
	for _, sha := range onlyA {
		if _, err := b.Stat(sha); err != nil {
			t.Fatalf("blob %s was not copied to b: %v", sha, err)
		}
	}
	if !h.store.HasStore("a") || h.store.GroupOf("a") != "" {
		t.Fatal("a should stay a blob store of its own, outside the group")
	}
	out, tk = h.run(t, TypeVerify, `{"repository":"mvn-g","checksum":true}`)
	if out.Result != ResultOK || !strings.Contains(out.Message, "checked 7 assets in 1 repositories: 0 missing, 0 mismatched") {
		t.Fatalf("verify after the removal: %+v\n%s", out, tk.LastLog)
	}
	// The group's last member cannot go.
	if _, err := h.s.Create(ctx, Def{Name: "x", Type: TypeGroupRemove, Schedule: "manual", Params: json.RawMessage(`{"group":"g","member":"b"}`)}); err == nil ||
		!strings.Contains(err.Error(), "no other writable member") {
		t.Fatalf("removing the last member: %v", err)
	}
}

// Recalculate-size reports each member's blobs and bytes and the group's as
// their sum; a member alone says it serves its group.
func TestGroupRecalc(t *testing.T) {
	h := newHarness(t)
	withGroup(t, h)
	for i := range 3 {
		h.put(t, "mvn-g", fmt.Sprintf("p/%d.jar", i), fmt.Sprintf("blob-%d", i)) // 6 bytes each
	}
	out, tk := h.run(t, TypeRecalc, `{"blobStore":"g"}`)
	if out.Result != ResultOK || !strings.Contains(tk.LastLog, "g: member a holds 2 blobs (12 bytes)") ||
		!strings.Contains(tk.LastLog, "g: member b holds 1 blobs (6 bytes)") || !strings.Contains(tk.LastLog, "g: the group holds 3 blobs (18 bytes) in 2 members") ||
		!strings.Contains(tk.LastLog, "g: 3 blobs in the store, 3 referenced by assets (18 bytes)") {
		t.Fatalf("recalc g: %+v\n%s", out, tk.LastLog)
	}
	out, tk = h.run(t, TypeRecalc, `{"blobStore":"a"}`)
	if out.Result != ResultOK || !strings.Contains(tk.LastLog, `a: 2 blobs (12 bytes) in the store, a member of the group "g"`) {
		t.Fatalf("recalc a: %+v\n%s", out, tk.LastLog)
	}
	out, tk = h.run(t, TypeRecalc, `{}`)
	if out.Result != ResultOK || strings.Contains(tk.LastLog, "a: 2 blobs") || !strings.Contains(tk.LastLog, "g: the group holds 3 blobs") {
		t.Fatalf("recalc all: %+v\n%s", out, tk.LastLog)
	}
}

func blobFile(root, sha string) string {
	return filepath.Join(root, "blobs", "sha256", sha[:2], sha[2:4], sha)
}
