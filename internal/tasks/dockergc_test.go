package tasks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo"
)

type fakeCollector struct {
	repos   []string
	before  time.Time
	preview bool
}

func (f *fakeCollector) CollectUnused(_ context.Context, rp *repo.Repository, before time.Time, preview bool, _ string, logf func(string, ...any)) (int64, error) {
	f.repos, f.before, f.preview = append(f.repos, rp.Name), before, preview
	logf("%s: unused manifest x", rp.Name)
	return 2, nil
}

func TestDockerGCTask(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for params, want := range map[string]string{
		`{"repository":"nope"}`: "does not exist",
		`{"repository":"mvn"}`:  "Docker hosted repository",
		`{"deployOffset":-1}`:   "at least 0",
		`{"unknown":1}`:         "unknown field",
	} {
		if _, err := h.s.Create(ctx, Def{Name: "x", Type: TypeDockerGC, Schedule: "manual", Params: json.RawMessage(params)}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want ~%q", params, err, want)
		}
	}
	// Without a handler the run fails rather than doing nothing silently.
	tk := h.create(t, Def{Name: "gc", Type: TypeDockerGC, Schedule: "manual", Params: json.RawMessage(`{"repository":"*"}`)})
	if tk.Params != `{"deployOffset":24}` {
		t.Fatalf("stored params %s", tk.Params)
	}
	if out, err := h.s.RunNow(tk.ID, "admin"); err != nil || out.Result != ResultFailed {
		t.Fatalf("no handler: %+v, %v", out, err)
	}
	f := &fakeCollector{}
	h.s.Docker = f
	up, err := h.s.Update(ctx, tk.ID, Def{Params: json.RawMessage(`{"deployOffset":48,"preview":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out, err := h.s.RunNow(up.ID, "admin")
	if err != nil || out.Result != ResultOK {
		t.Fatalf("run: %+v, %v", out, err)
	}
	if !f.preview || f.before.After(start.Add(-47*time.Hour)) || strings.Join(f.repos, ",") != "images" {
		t.Fatalf("preview %v, before %v, repositories %v", f.preview, f.before, f.repos)
	}
	if out.Removed != 2 || !strings.Contains(out.Message, "would remove 2 unused manifests in 1 repositories") {
		t.Fatalf("outcome: %+v", out)
	}
}
