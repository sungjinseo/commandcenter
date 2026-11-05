package nexusapi

import (
	"fmt"
	"math/rand/v2"
	"net/url"
	"slices"
	"strings"
	"testing"

	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

// A window gives the head of what an unbounded search finds, whatever order
// the files come in and however few components it holds on to.
func TestCollectorWindow(t *testing.T) {
	rp := &repo.Repository{Repository: config.Repository{Name: "r", Format: "maven"}}
	type file struct{ group, name, version, p string }
	var files []file
	for i := range 300 {
		name, version := fmt.Sprintf("a%d", i%20), fmt.Sprintf("%d.0", i)
		dir := "org/x/" + name + "/" + version + "/" + name + "-" + version
		files = append(files, file{"org.x", name, version, dir + ".pom"}, file{"org.x", name, version, dir + ".jar"})
		if i%3 == 0 { // q=sources and maven.classifier find these by one file only
			files = append(files, file{"org.x", name, version, dir + "-sources.jar"})
		}
	}
	rand.New(rand.NewPCG(1, 2)).Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })
	hide := func(_ *repo.Repository, p string) bool {
		return !strings.HasSuffix(p, "7.0/") && !strings.Contains(p, "7.0.")
	}
	run := func(cr criteria, win window, keep func(*repo.Repository, string) bool) (*collector, []string) {
		s := &collector{cr: cr, win: win, keep: keep, rp: rp, open: map[string]*held{}}
		for _, f := range files {
			s.add(f.group, f.name, f.version, f.p)
		}
		if win.limit == 0 {
			slices.SortFunc(s.best, func(a, b *held) int { return cr.compare(a.comp, b.comp) })
		}
		var out []string
		for _, e := range s.best {
			paths := slices.Sorted(slices.Values(e.paths))
			out = append(out, e.key()+" "+strings.Join(paths, ","))
		}
		return s, out
	}
	for _, query := range []string{"", "name=a1*", "q=sources", "maven.extension=pom", "maven.classifier=sources", "org.x:a3:3.0:jar:sources",
		"sort=version", "sort=name&direction=desc", "q=a1&maven.extension=jar&sort=version"} {
		for _, keep := range []func(*repo.Repository, string) bool{nil, hide} {
			q, err := url.ParseQuery(query)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(query, ":") {
				q = url.Values{"q": {query}}
			}
			cr, err := parseCriteria(q)
			if err != nil {
				t.Fatal(err)
			}
			_, all := run(cr, window{}, keep)
			if len(all) == 0 {
				t.Fatalf("%q finds nothing: the test proves nothing", query)
			}
			for _, limit := range []int{1, 7, 51} {
				s, head := run(cr, window{limit: limit}, keep)
				if want := all[:min(limit, len(all))]; !slices.Equal(head, want) {
					t.Errorf("%q limit %d:\n got %v\nwant %v", query, limit, head, want)
				}
				if cr.file == nil && len(s.open) > limit {
					t.Errorf("%q limit %d holds %d components", query, limit, len(s.open))
				}
				// The second page starts right after the first one's last item.
				last := s.best[len(s.best)-1].comp
				_, next := run(cr, window{limit: limit, after: last}, keep)
				from := min(limit, len(all))
				if want := all[from:min(from+limit, len(all))]; !slices.Equal(next, want) && !(len(next) == 0 && len(want) == 0) {
					t.Errorf("%q page 2, limit %d:\n got %v\nwant %v", query, limit, next, want)
				}
				_, at := run(cr, window{limit: limit, after: last, atToken: true}, keep)
				if len(at) == 0 || at[0] != head[len(head)-1] {
					t.Errorf("%q limit %d at token starts with %v, want %s", query, limit, at, head[len(head)-1])
				}
			}
		}
	}
}
