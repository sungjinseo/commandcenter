package meta

import (
	"errors"
	"testing"
	"time"
)

func TestTxPackagesAndDistTags(t *testing.T) {
	db := openTest(t)
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	err := db.Tx(ctx, func(tx *Tx) error {
		if err := tx.InsertPackage(ctx, Package{Repo: "r", Format: "npm", Name: "a", Version: "1.0.0", Attrs: `{"v":1}`, CreatedAt: t0}); err != nil {
			return err
		}
		if err := tx.InsertPackage(ctx, Package{Repo: "r", Format: "npm", Name: "a", Version: "2.0.0", Attrs: `{"v":2}`, CreatedAt: t0.Add(time.Second)}); err != nil {
			return err
		}
		if err := tx.SetDistTag(ctx, "r", "a", "latest", "1.0.0"); err != nil {
			return err
		}
		if err := tx.SetDistTag(ctx, "r", "a", "latest", "2.0.0"); err != nil {
			return err
		}
		if err := tx.SetDistTag(ctx, "r", "a", "beta", "2.0.0"); err != nil {
			return err
		}
		return tx.PutAsset(ctx, Asset{Repo: "r", Path: "a/-/a-1.0.0.tgz", BlobSHA256: "s"})
	})
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := db.ListPackages(ctx, "r", "a")
	if err != nil || len(pkgs) != 2 || pkgs[0].Version != "1.0.0" || pkgs[1].Attrs != `{"v":2}` || !pkgs[0].CreatedAt.Equal(t0) || pkgs[0].Format != "npm" {
		t.Fatalf("pkgs = %+v err=%v", pkgs, err)
	}
	tags, _ := db.ListDistTags(ctx, "r", "a")
	if len(tags) != 2 || tags[0] != (DistTag{"beta", "2.0.0"}) || tags[1] != (DistTag{"latest", "2.0.0"}) {
		t.Fatalf("tags = %+v", tags)
	}
	if _, err := db.GetAsset(ctx, "r", "a/-/a-1.0.0.tgz"); err != nil {
		t.Fatalf("asset written in tx missing: %v", err)
	}
}

func TestTxRollbackAndErrors(t *testing.T) {
	db := openTest(t)
	boom := errors.New("boom")
	err := db.Tx(ctx, func(tx *Tx) error {
		tx.InsertPackage(ctx, Package{Repo: "r", Format: "npm", Name: "a", Version: "1.0.0", Attrs: "{}"})
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if pkgs, _ := db.ListPackages(ctx, "r", "a"); len(pkgs) != 0 {
		t.Fatalf("rollback failed: %+v", pkgs)
	}
	err = db.Tx(ctx, func(tx *Tx) error {
		if err := tx.InsertPackage(ctx, Package{Repo: "r", Format: "npm", Name: "a", Version: "1.0.0", Attrs: "{}"}); err != nil {
			return err
		}
		if err := tx.InsertPackage(ctx, Package{Repo: "r", Format: "npm", Name: "a", Version: "1.0.0", Attrs: "{}"}); !errors.Is(err, ErrExists) {
			t.Errorf("duplicate insert: %v", err)
		}
		if err := tx.UpdatePackage(ctx, "r", "a", "1.0.0", `{"deprecated":"x"}`); err != nil {
			return err
		}
		if err := tx.UpdatePackage(ctx, "r", "a", "9.9.9", "{}"); !errors.Is(err, ErrNotFound) {
			t.Errorf("update missing: %v", err)
		}
		if err := tx.DeleteDistTag(ctx, "r", "a", "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete missing tag: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	pkgs, _ := db.ListPackages(ctx, "r", "a")
	if len(pkgs) != 1 || pkgs[0].Attrs != `{"deprecated":"x"}` {
		t.Fatalf("pkgs = %+v", pkgs)
	}
}

func TestTxPanicReleasesLock(t *testing.T) {
	db := openTest(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected panic to propagate")
			}
		}()
		db.Tx(ctx, func(tx *Tx) error {
			if err := tx.InsertPackage(ctx, Package{Repo: "r", Format: "npm", Name: "p", Version: "1.0.0", Attrs: "{}", CreatedAt: time.Now()}); err != nil {
				t.Error(err)
			}
			panic("boom")
		})
	}()
	done := make(chan error, 1)
	go func() {
		done <- db.Tx(ctx, func(tx *Tx) error {
			return tx.InsertPackage(ctx, Package{Repo: "r", Format: "npm", Name: "q", Version: "1.0.0", Attrs: "{}", CreatedAt: time.Now()})
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Tx blocked: lock not released after panic")
	}
	if pkgs, _ := db.ListPackages(ctx, "r", "p"); len(pkgs) != 0 {
		t.Fatalf("panicked tx was not rolled back: %+v", pkgs)
	}
}

func TestListPackageNames(t *testing.T) {
	db := openTest(t)
	err := db.Tx(ctx, func(tx *Tx) error {
		for _, p := range []Package{{Name: "b", Version: "1"}, {Name: "a", Version: "1"}, {Name: "a", Version: "2"}} {
			p.Repo, p.Format, p.Attrs = "r", "pypi", "{}"
			if err := tx.InsertPackage(ctx, p); err != nil {
				return err
			}
		}
		return tx.InsertPackage(ctx, Package{Repo: "other", Format: "pypi", Name: "z", Version: "1", Attrs: "{}"})
	})
	if err != nil {
		t.Fatal(err)
	}
	names, err := db.ListPackageNames(ctx, "r")
	if err != nil || len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("names=%v err=%v", names, err)
	}
}

func TestSetYanked(t *testing.T) {
	db := openTest(t)
	err := db.Tx(ctx, func(tx *Tx) error {
		if err := tx.InsertPackage(ctx, Package{Repo: "r", Format: "cargo", Name: "a", Version: "1.0.0", Attrs: "{}"}); err != nil {
			return err
		}
		if err := tx.SetYanked(ctx, "r", "a", "1.0.0", true); err != nil {
			return err
		}
		if err := tx.SetYanked(ctx, "r", "a", "9.9.9", true); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pkgs, _ := db.ListPackages(ctx, "r", "a"); !pkgs[0].Yanked {
		t.Fatal("not yanked")
	}
}

func TestListPackageNamesCanonical(t *testing.T) {
	db := openTest(t)
	err := db.Tx(ctx, func(tx *Tx) error {
		for _, n := range []string{"foo-bar", "foo_bar", "foobar", "other"} {
			if err := tx.InsertPackage(ctx, Package{Repo: "r", Format: "cargo", Name: n, Version: "1", Attrs: "{}"}); err != nil {
				return err
			}
		}
		return tx.InsertPackage(ctx, Package{Repo: "x", Format: "cargo", Name: "foo-bar", Version: "1", Attrs: "{}"})
	})
	if err != nil {
		t.Fatal(err)
	}
	names, err := db.ListPackageNamesCanonical(ctx, "r", "foo-bar")
	if err != nil || len(names) != 2 || names[0] != "foo-bar" || names[1] != "foo_bar" {
		t.Fatalf("names=%v err=%v", names, err)
	}
}

func TestPublishedName(t *testing.T) {
	db := openTest(t)
	err := db.Tx(ctx, func(tx *Tx) error {
		for _, p := range []Package{
			{Repo: "r", Format: "cargo", Name: "foo-bar", Version: "1.0.0", Attrs: `{"name":"Foo-Bar","vers":"1.0.0"}`},
			{Repo: "r", Format: "cargo", Name: "plain", Version: "1.0.0", Attrs: "{}"},
			{Repo: "r", Format: "cargo", Name: "broken", Version: "1.0.0", Attrs: "not json"},
		} {
			if err := tx.InsertPackage(ctx, p); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"foo-bar": "Foo-Bar", "plain": "", "missing": ""} {
		if got, err := db.PublishedName(ctx, "r", name); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", name, got, err, want)
		}
	}
	if _, err := db.PublishedName(ctx, "r", "broken"); err == nil {
		t.Error("malformed attrs: want an error")
	}
}

func TestTxDeletes(t *testing.T) {
	db := openTest(t)
	err := db.Tx(ctx, func(tx *Tx) error {
		for _, v := range []string{"1.0.0", "2.0.0"} {
			if err := tx.InsertPackage(ctx, Package{Repo: "r", Format: "npm", Name: "a", Version: v, Attrs: "{}"}); err != nil {
				return err
			}
			if err := tx.PutAsset(ctx, Asset{Repo: "r", Path: "a/-/a-" + v + ".tgz", BlobSHA256: "s", Size: 42}); err != nil {
				return err
			}
		}
		return tx.PutAsset(ctx, Asset{Repo: "r", Path: "b/-/b-1.0.0.tgz", BlobSHA256: "s"})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Tx(ctx, func(tx *Tx) error {
		as, err := tx.ListAssets(ctx, "r", "a/")
		if err != nil || len(as) != 2 || as[0].Path != "a/-/a-1.0.0.tgz" {
			t.Fatalf("tx ListAssets = %+v %v", as, err)
		}
		if n, err := tx.DeleteAsset(ctx, "r", "a/-/a-1.0.0.tgz"); err != nil || n != 42 {
			t.Fatalf("DeleteAsset = %d %v", n, err)
		}
		if _, err := tx.DeleteAsset(ctx, "r", "a/-/a-1.0.0.tgz"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second DeleteAsset = %v", err)
		}
		if err := tx.DeletePackage(ctx, "r", "a", "1.0.0"); err != nil {
			return err
		}
		if err := tx.DeletePackage(ctx, "r", "a", "1.0.0"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second DeletePackage = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pkgs, _ := db.ListPackages(ctx, "r", "a"); len(pkgs) != 1 || pkgs[0].Version != "2.0.0" {
		t.Fatalf("packages = %+v", pkgs)
	}
	if as, _ := db.ListAssets(ctx, "r", ""); len(as) != 2 || as[0].Path != "a/-/a-2.0.0.tgz" || as[1].Path != "b/-/b-1.0.0.tgz" {
		t.Fatalf("assets = %+v", as)
	}
}
