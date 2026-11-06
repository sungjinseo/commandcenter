package yum

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// The rebuild-metadata task regenerates the index files byte for byte.
func TestRebuildMetadata(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "yum-hosted", "Packages/d/demo-1.0-1.el9.x86_64.rpm", demoRPM()); w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	// Depth 1: one repodata/ per top-level directory.
	for _, d := range []string{"el9/", "el8/"} {
		if w := e.do("PUT", "yum-deep", d+"x/demo-1.0-1.noarch.rpm", nvra("demo", "1.0", "1", "noarch")); w.Code != 201 {
			t.Fatalf("upload %s: %d %s", d, w.Code, w.Body)
		}
	}
	for _, d := range []string{"7/os/x86_64/", "7/os/aarch64/"} {
		if w := e.do("PUT", "yum-deep3", d+"demo-1.0-1.noarch.rpm", nvra("demo", "1.0", "1", "noarch")); w.Code != 201 {
			t.Fatalf("upload %s: %d %s", d, w.Code, w.Body)
		}
	}
	for _, name := range []string{"yum-hosted", "yum-deep", "yum-deep3"} {
		t.Run(name, func(t *testing.T) { roundTrip(t, e, e.repos[name]) })
	}
}

func roundTrip(t *testing.T, e *env, rp *repo.Repository) {
	ctx := context.Background()
	db := e.h.Store.DB
	indexes := func() map[string][]byte {
		out := map[string][]byte{}
		all, err := db.ListAssets(ctx, rp.Name, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range all {
			if !isIndexPath(a.Path) {
				continue
			}
			f, err := e.h.Store.Open(a)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(f)
			f.Close()
			out[a.Path] = b
		}
		return out
	}
	before := indexes()
	if len(before) == 0 {
		t.Fatal("no index files after the upload")
	}
	for p := range before {
		if err := db.DeleteAsset(ctx, rp.Name, p); err != nil {
			t.Fatal(err)
		}
	}
	if got := indexes(); len(got) != 0 {
		t.Fatalf("still there: %v", got)
	}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, format) }
	if err := e.h.RebuildMetadata(ctx, rp, logf); err != nil {
		t.Fatal(err)
	}
	after := indexes()
	if len(after) != len(before) || len(logged) == 0 {
		t.Fatalf("rebuilt %d of %d index files (log %d lines)", len(after), len(before), len(logged))
	}
	for p, b := range before {
		if !bytes.Equal(after[p], b) {
			t.Errorf("%s differs after the rebuild:\n%s\n---\n%s", p, b, after[p])
		}
	}
	if rp.Depth > 0 && len(before) != 8 {
		t.Fatalf("depth %d: %d index files, want 8", rp.Depth, len(before))
	}
	// Not for a proxy or group.
	for _, name := range []string{"yum-proxy", "yum-all"} {
		if err := e.h.RebuildMetadata(ctx, e.repos[name], logf); err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Errorf("%s: %v", name, err)
		}
	}
	_ = meta.ErrNotFound
}

func isIndexPath(p string) bool { return isRepodata(p) }

// A package uploaded before the server read the weak dependencies has a
// row without them. The rebuild reads the stored file again, so its
// repodata gains them and the upload time stays; a package whose file is
// gone keeps its row.
func TestRebuildReadsStoredPackagesAgain(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["yum-hosted"]
	for path, b := range map[string][]byte{"demo-1.0-1.el9.x86_64.rpm": demoRPM(), "gone-1-1.noarch.rpm": nvra("gone", "1", "1", "noarch")} {
		if w := e.do("PUT", "yum-hosted", path, b); w.Code != 201 {
			t.Fatalf("upload %s: %d %s", path, w.Code, w.Body)
		}
	}
	uploaded := e.now
	rows, err := e.db.ListPackages(ctx, "yum-hosted", "demo")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows: %v %v", rows, err)
	}
	var old Package
	if err := json.Unmarshal([]byte(rows[0].Attrs), &old); err != nil {
		t.Fatal(err)
	}
	old.Recommends, old.Suggests, old.Supplements, old.Enhances = nil, nil, nil, nil // as an earlier version stored it
	attrs, _ := json.Marshal(old)
	err = e.h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		if err := tx.UpdatePackage(ctx, "yum-hosted", "demo", rows[0].Version, string(attrs)); err != nil {
			return err
		}
		_, err := tx.DeleteAsset(ctx, "yum-hosted", "gone-1-1.noarch.rpm")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Hour)
	var logged []string
	if err := e.h.RebuildMetadata(ctx, rp, func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }); err != nil {
		t.Fatal(err)
	}
	_, data := e.repodata(t, "yum-hosted", "")
	if !strings.Contains(data["primary"], `<rpm:entry name="demo-plugins" flags="EQ" epoch="0" ver="1.0" rel="1.el9"/>`) ||
		!strings.Contains(data["primary"], `<time file="`+fmt.Sprint(uploaded.Unix())+`"`) || !strings.Contains(data["primary"], "<name>gone</name>") {
		t.Fatalf("primary.xml after the rebuild:\n%s", data["primary"])
	}
	if log := strings.Join(logged, "\n"); !strings.Contains(log, "gone-1-1.noarch.rpm is not read again") || !strings.Contains(log, "1 packages read again") {
		t.Fatalf("log: %s", log)
	}
	// nothing differs now: a second rebuild rewrites no row
	logged = nil
	if err := e.h.RebuildMetadata(ctx, rp, func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }); err != nil || strings.Contains(strings.Join(logged, "\n"), "read again from") {
		t.Fatalf("second rebuild: %v %v", err, logged)
	}
}
