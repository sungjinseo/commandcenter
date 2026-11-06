package repo

import (
	"strings"
	"testing"

	"commandcenter/internal/config"
)

// Whether an offline member stands ahead of an online one, nested groups in
// place, and only for a group that does not merge.
func TestOfflineAhead(t *testing.T) {
	off := false
	hosted := func(name string) config.Repository {
		return config.Repository{Name: name, Format: "npm", Type: "hosted"}
	}
	group := func(name string, members ...string) config.Repository {
		return config.Repository{Name: name, Format: "npm", Type: "group", Members: members}
	}
	x := hosted("x")
	x.Online = &off
	down := group("down", "a")
	down.Online = &off
	for _, c := range []struct {
		members []string
		merge   bool
		want    string // the offline member named, "" for none
	}{
		{[]string{"x", "a"}, false, "x"},
		{[]string{"a", "x"}, false, ""},
		{[]string{"a", "x", "b"}, false, "x"},
		{[]string{"x", "a"}, true, ""},
		{[]string{"ax", "b"}, false, "x"},
		{[]string{"ax"}, false, ""},
		{[]string{"xa"}, false, "x"},
		{[]string{"down", "b"}, false, "down"},
		{[]string{"b", "down"}, false, ""},
		{[]string{"x", "onlyx"}, false, ""},
		{[]string{"a", "b"}, false, ""},
	} {
		g := group("g", c.members...)
		g.MergeMembers = c.merge
		rp := Build(&config.Config{Repositories: []config.Repository{hosted("a"), hosted("b"), x, down, group("ax", "a", "x"), group("xa", "x", "a"), group("onlyx", "x"), g}})["g"]
		err := rp.OfflineAhead()
		if c.want == "" && err != nil || c.want != "" && (StatusOf(err) != 503 || !strings.Contains(err.Error(), `"`+c.want+`"`)) {
			t.Errorf("%v merge=%v: %v, want %q", c.members, c.merge, err, c.want)
		}
	}
	if err := Build(&config.Config{Repositories: []config.Repository{hosted("a")}})["a"].OfflineAhead(); err != nil {
		t.Errorf("no group: %v", err)
	}
}
