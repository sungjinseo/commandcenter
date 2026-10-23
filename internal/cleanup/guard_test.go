package cleanup

import (
	"context"
	"errors"
	"testing"
	"time"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
)

func TestCheckUnchanged(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := &component.Evaluated{Paths: map[string]bool{"a": true, "b": true}, MaxCreated: t1, MaxModified: t1, LastDownloaded: t1}
	row := func(p string, created, modified, dl time.Time) meta.Asset {
		return meta.Asset{Path: p, CreatedAt: created, LastModified: modified, LastDownloaded: dl}
	}
	later := t1.Add(time.Second)
	for _, c := range []struct {
		name string
		rows []meta.Asset
		ok   bool
	}{
		{"same", []meta.Asset{row("a", t1, t1, t1), row("b", t1.Add(-time.Hour), t1, time.Time{})}, true},
		{"fewer", []meta.Asset{row("a", t1, t1, t1)}, true},
		{"none", nil, true},
		{"new path", []meta.Asset{row("a", t1, t1, t1), row("c", t1, t1, time.Time{})}, false},
		{"created later", []meta.Asset{row("a", later, later, time.Time{})}, false},
		{"replaced later", []meta.Asset{row("a", t1, later, time.Time{})}, false},
		{"downloaded later", []meta.Asset{row("b", t1, t1, later)}, false},
	} {
		err := CheckUnchanged(component.Component{Format: "maven", Group: "g", Name: "a", Version: "1", Evaluated: ev}, c.rows)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrChanged)) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// Without an evaluation (a delete from the UI, say) nothing is checked.
	if err := CheckUnchanged(component.Component{}, []meta.Asset{row("c", later, later, later)}); err != nil {
		t.Fatal(err)
	}
}

// The proxy deleter checks every cached file of the component, the ones
// found by its directory scan included.
func TestProxyDeleterSkipsChanged(t *testing.T) {
	e := newEnv(t)
	jar, sha := "org/example/app/1.0/app-1.0.jar", "org/example/app/1.0/app-1.0.jar.sha1"
	e.put(t, "maven-proxy", jar, sha) // the sha1 was cached after the evaluation
	future := time.Now().Add(time.Hour)
	c := component.Component{Format: "maven", Group: "org.example", Name: "app", Version: "1.0", Assets: assets(jar),
		Evaluated: &component.Evaluated{Paths: map[string]bool{jar: true}, MaxCreated: future, MaxModified: future}}
	d := &ProxyDeleter{Store: e.store}
	if _, err := d.DeleteComponent(context.Background(), e.repos["maven-proxy"], c, "system"); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed: %v", err)
	}
	samePaths(t, "after the skip", e.paths(t, "maven-proxy"), []string{jar, sha})
	c.Evaluated.Paths[sha] = true
	if res, err := d.DeleteComponent(context.Background(), e.repos["maven-proxy"], c, "system"); err != nil || res.Assets != 2 {
		t.Fatalf("unchanged: %+v %v", res, err)
	}
}
