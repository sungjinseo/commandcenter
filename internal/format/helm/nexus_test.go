package helm

import (
	"bytes"
	"context"
	"mime/multipart"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
)

// What a Nexus helm repository does that scripts and the helm client rely
// on: provenance files, DELETE of a chart, chart versions that are not
// strict SemVer, and a proxy whose cached charts follow the remote.

// provFor is a provenance file for chart as `helm package --sign` lays one
// out: the clear-signed Chart.yaml and the chart file's sha256.
func provFor(file string, chart []byte) []byte {
	return []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n\nname: demo\nkeywords:\n- - dash-escaped\n\n...\nfiles:\n  " + file + ": sha256:" + sum(chart) +
		"\n-----BEGIN PGP SIGNATURE-----\n\nwsBcBAEBCgAQBQJnc2lnAAoJEFRlc3RLZXkAAGJhc2U2NA==\n=abcd\n-----END PGP SIGNATURE-----\n")
}

func TestProvenance(t *testing.T) {
	e := newEnv(t)
	demo := chartTgz(t, "demo", "1.0.0", "")
	prov := provFor("demo-1.0.0.tgz", demo)
	// the chart first: a provenance file is the stored chart's
	if w := e.do("PUT", "helm-hosted", "demo-1.0.0.tgz.prov", prov); w.Code != 400 || !strings.Contains(w.Body.String(), "upload the chart before") {
		t.Fatalf("provenance before its chart: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "helm-hosted", "demo-1.0.0.tgz", demo); w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	e.sink.Reset()
	if w := e.do("PUT", "helm-hosted", "demo-1.0.0.tgz.prov", prov); w.Code != 201 || w.Body.String() != "{\"saved\":true}\n" {
		t.Fatalf("PUT provenance: %d %s", w.Code, w.Body)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0] != (repotest.Event{Kind: "upload", Repo: "helm-hosted", Subject: "demo-1.0.0.tgz.prov"}) {
		t.Fatalf("events: %v", got)
	}
	// helm pull --verify asks for <chart URL>.prov, of the hosted repository or the group
	for _, rp := range []string{"helm-hosted", "helm-all"} {
		w := e.do("GET", rp, "demo-1.0.0.tgz.prov", nil)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), prov) || w.Header().Get("Content-Type") != ctProv {
			t.Fatalf("GET provenance from %s: %d %v", rp, w.Code, w.Header())
		}
	}
	if w := e.do("HEAD", "helm-hosted", "demo-1.0.0.tgz.prov", nil); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD provenance: %d", w.Code)
	}
	for _, c := range []struct {
		name, path string
		body       []byte
		want       int
		msg        string
	}{
		{"another build of the chart", "demo-1.0.0.tgz.prov", provFor("demo-1.0.0.tgz", []byte("other bytes")), 400, "signs another build"},
		{"the file name of another chart", "other-1.0.0.tgz.prov", prov, 400, "does not match the chart file"},
		{"not clear-signed text", "demo-1.0.0.tgz.prov", []byte("name: demo\n...\nfiles:\n  demo-1.0.0.tgz: sha256:" + sum(demo) + "\n"), 400, "not a provenance file"},
		{"no files block", "demo-1.0.0.tgz.prov", []byte(provHead + "\n\nname: demo\n-----BEGIN PGP SIGNATURE-----\n"), 400, "does not name one chart file"},
		{"a path as the file", "demo-1.0.0.tgz.prov", provFor("../demo-1.0.0.tgz", demo), 400, "not a chart file"},
		{"too large", "demo-1.0.0.tgz.prov", append(bytes.Clone(prov), make([]byte, maxProv)...), 413, "larger than"},
	} {
		if w := e.do("PUT", "helm-hosted", c.path, c.body); w.Code != c.want || !strings.Contains(w.Body.String(), c.msg) {
			t.Errorf("%s: %d %s, want %d %q", c.name, w.Code, w.Body, c.want, c.msg)
		}
	}
	if w := e.do("PUT", "helm-ro", "demo-1.0.0.tgz.prov", prov); w.Code != 400 || !strings.Contains(w.Body.String(), "read-only") {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	// it is a file of the chart's component, and goes with it
	if _, name, version, ok := component.DeriveAsset("helm", "demo-1.0.0.tgz.prov"); !ok || name != "demo" || version != "1.0.0" {
		t.Fatalf("component of the provenance file: %q %q %v", name, version, ok)
	}
	res, err := e.h.DeleteComponent(context.Background(), e.repos["helm-hosted"], component.Component{Format: "helm", Name: "demo", Version: "1.0.0"}, "admin")
	if err != nil || res.Assets != 2 || res.Bytes != int64(len(demo)+len(prov)) {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if w := e.do("GET", "helm-hosted", "demo-1.0.0.tgz.prov", nil); w.Code != 404 {
		t.Fatalf("provenance of a deleted chart: %d", w.Code)
	}
}

// A provenance file may be replaced under allow_once, as in Nexus (a chart
// signed again), by a user with Edit; replacing a chart drops the
// provenance file of the content it replaces.
func TestProvenanceReplace(t *testing.T) {
	e := newEnv(t)
	a, ps := repotest.Principals(t, e.h.Store.DB, "helm-hosted", map[string][]string{
		"adder":  {meta.ActionAdd},
		"editor": {meta.ActionAdd, meta.ActionEdit},
	})
	e.h.Auth = a
	demo := chartTgz(t, "demo", "1.0.0", "")
	prov := provFor("demo-1.0.0.tgz", demo)
	again := append(bytes.Clone(prov), '\n')
	for _, c := range []struct {
		user string
		path string
		body []byte
		want int
	}{{"adder", "demo-1.0.0.tgz", demo, 201}, {"adder", "demo-1.0.0.tgz.prov", prov, 201}, {"adder", "demo-1.0.0.tgz.prov", again, 403}, {"editor", "demo-1.0.0.tgz.prov", again, 201}} {
		if w := e.doAs(ps[c.user], "PUT", "helm-hosted", c.path, c.body); w.Code != c.want {
			t.Fatalf("%s PUT %s: %d %s, want %d", c.user, c.path, w.Code, w.Body, c.want)
		}
	}
	if w := e.do("GET", "helm-hosted", "demo-1.0.0.tgz.prov", nil); !bytes.Equal(w.Body.Bytes(), again) {
		t.Fatal("the replaced provenance file is not served")
	}
	e.h.Auth = nil
	v1 := chartTgz(t, "demo", "1.0.0", "")
	v1b := chartTgz(t, "demo", "1.0.0", "name: demo\nversion: 1.0.0\ndescription: replaced\n")
	for _, c := range []struct {
		path string
		body []byte
	}{{"demo-1.0.0.tgz", v1}, {"demo-1.0.0.tgz.prov", provFor("demo-1.0.0.tgz", v1)}, {"demo-1.0.0.tgz", v1}} {
		if w := e.do("PUT", "helm-allow", c.path, c.body); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", c.path, w.Code, w.Body)
		}
	}
	if w := e.do("GET", "helm-allow", "demo-1.0.0.tgz.prov", nil); w.Code != 200 {
		t.Fatalf("the same content again keeps its provenance file: %d", w.Code)
	}
	if w := e.do("PUT", "helm-allow", "demo-1.0.0.tgz", v1b); w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	if w := e.do("GET", "helm-allow", "demo-1.0.0.tgz.prov", nil); w.Code != 404 {
		t.Fatalf("provenance file of replaced content: %d, want 404", w.Code)
	}
}

// POST api/charts takes the provenance file as ChartMuseum's "prov" field
// (before or after "chart"), as a "chart" field that holds one (Nexus) and
// as the body.
func TestProvenanceAPICharts(t *testing.T) {
	e := newEnv(t)
	post := func(parts ...[]byte) (int, string) { // name, content pairs
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for i := 0; i+1 < len(parts); i += 2 {
			fw, _ := mw.CreateFormFile(string(parts[i]), "/tmp/helm-push-1/upload")
			fw.Write(parts[i+1])
		}
		mw.Close()
		w := e.do("POST", "helm-all", "api/charts", buf.Bytes(), "Content-Type", mw.FormDataContentType())
		return w.Code, w.Body.String()
	}
	for i, v := range []string{"1.0.0", "1.1.0"} {
		chart := chartTgz(t, "demo", v, "")
		parts := [][]byte{[]byte("chart"), chart, []byte("prov"), provFor(ChartFile("demo", v), chart)}
		if i == 1 {
			parts = [][]byte{parts[2], parts[3], parts[0], parts[1]} // the provenance file first
		}
		if code, body := post(parts...); code != 201 {
			t.Fatalf("chart and prov %s: %d %s", v, code, body)
		}
		if w := e.do("GET", "helm-hosted", ChartFile("demo", v)+".prov", nil); w.Code != 200 {
			t.Fatalf("provenance of %s: %d", v, w.Code)
		}
	}
	if got := e.sink.Events(); len(got) != 2 || got[1].Subject != "demo@1.1.0" {
		t.Fatalf("one event per request: %v", got)
	}
	chart := chartTgz(t, "demo", "1.2.0", "")
	prov := provFor("demo-1.2.0.tgz", chart)
	if code, body := post([]byte("chart"), chart); code != 201 {
		t.Fatal(code, body)
	}
	if code, body := post([]byte("chart"), prov); code != 201 { // Nexus: a "chart" field that is a provenance file
		t.Fatalf("provenance as the chart field: %d %s", code, body)
	}
	if w := e.do("GET", "helm-hosted", "demo-1.2.0.tgz.prov", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), prov) {
		t.Fatalf("GET: %d", w.Code)
	}
	// the component upload API sends the file as the body
	chart = chartTgz(t, "demo", "1.3.0", "")
	prov = provFor("demo-1.3.0.tgz", chart)
	for _, body := range [][]byte{chart, prov} {
		if w := e.do("POST", "helm-hosted", "api/charts", body, "Content-Type", "application/octet-stream"); w.Code != 201 {
			t.Fatalf("as the body: %d %s", w.Code, w.Body)
		}
	}
	if w := e.do("GET", "helm-hosted", "demo-1.3.0.tgz.prov", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), prov) {
		t.Fatalf("GET: %d", w.Code)
	}
	// the first field of each name counts
	other := chartTgz(t, "other", "1.0.0", "")
	if code, body := post([]byte("chart"), other, []byte("prov"), provFor("other-1.0.0.tgz", other), []byte("chart"), []byte("x"), []byte("prov"), []byte("x")); code != 201 {
		t.Fatalf("repeated fields: %d %s", code, body)
	}
	// a PUT to a chart's own path takes a chart only
	if w := e.do("PUT", "helm-hosted", "demo-1.2.0.tgz", prov); w.Code != 400 || !strings.Contains(w.Body.String(), "not a gzip-compressed tar") {
		t.Fatalf("provenance PUT as a chart: %d %s", w.Code, w.Body)
	}
}

// DELETE of a chart file, as Nexus takes it: 200, and 404 when it is gone.
func TestDeleteChart(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"1.0.0", "1.1.0"} {
		chart := chartTgz(t, "demo", v, "")
		if w := e.do("PUT", "helm-hosted", ChartFile("demo", v), chart); w.Code != 201 {
			t.Fatal(w.Code)
		}
		if w := e.do("PUT", "helm-hosted", ChartFile("demo", v)+".prov", provFor(ChartFile("demo", v), chart)); w.Code != 201 {
			t.Fatal(w.Code, w.Body)
		}
	}
	e.sink.Reset()
	if w := e.do("DELETE", "helm-hosted", "demo-1.0.0.tgz", nil); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0].Kind != "delete" || got[0].Subject != "demo@1.0.0" || got[0].Detail != "assets=2" {
		t.Fatalf("events: %v", got)
	}
	for _, p := range []string{"demo-1.0.0.tgz", "demo-1.0.0.tgz.prov"} {
		if w := e.do("GET", "helm-hosted", p, nil); w.Code != 404 {
			t.Fatalf("GET %s after DELETE: %d", p, w.Code)
		}
	}
	if got := strings.Join(versions(index(t, e.do("GET", "helm-hosted", "index.yaml", nil)), "demo"), ","); got != "1.1.0" {
		t.Fatalf("index after DELETE: %s", got)
	}
	if w := e.do("DELETE", "helm-hosted", "demo-1.0.0.tgz", nil); w.Code != 404 {
		t.Fatalf("DELETE again: %d", w.Code)
	}
	// a provenance file alone; its chart stays
	if w := e.do("DELETE", "helm-hosted", "demo-1.1.0.tgz.prov", nil); w.Code != 200 {
		t.Fatalf("DELETE provenance: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "helm-hosted", "demo-1.1.0.tgz.prov", nil); w.Code != 404 {
		t.Fatalf("DELETE provenance again: %d", w.Code)
	}
	if w := e.do("GET", "helm-hosted", "demo-1.1.0.tgz", nil); w.Code != 200 {
		t.Fatalf("the chart of a deleted provenance file: %d", w.Code)
	}
	// through the group: its writeMember's chart
	if w := e.do("DELETE", "helm-all", "demo-1.1.0.tgz", nil); w.Code != 200 {
		t.Fatalf("DELETE through the group: %d %s", w.Code, w.Body)
	}
	if ix := index(t, e.do("GET", "helm-hosted", "index.yaml", nil)); len(ix.Entries) != 0 {
		t.Fatalf("index: %+v", ix.Entries)
	}
	if w := e.do("DELETE", "helm-ro", "demo-1.0.0.tgz", nil); w.Code != 400 || !strings.Contains(w.Body.String(), "read-only") {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "helm-proxy-first", "demo-1.0.0.tgz", nil); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("a group without writeMember: %d Allow %q", w.Code, w.Header().Get("Allow"))
	}
}

// Chart versions helm accepts that are not strict SemVer 2 (v1.2.3, 1.2):
// uploaded, listed newest first with the others, downloaded and deleted.
func TestLooseVersions(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"1.0.0", "v1.2.3", "1.2", "1.10.0-rc.1", "v2", "01.5.0"} {
		chart := chartTgz(t, "demo", v, "name: demo\nversion: \""+v+"\"\n")
		if w := e.do("PUT", "helm-hosted", ChartFile("demo", v), chart); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", v, w.Code, w.Body)
		}
		if w := e.do("GET", "helm-all", ChartFile("demo", v), nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), chart) {
			t.Fatalf("GET %s: %d", v, w.Code)
		}
	}
	ix := index(t, e.do("GET", "helm-hosted", "index.yaml", nil))
	if got := strings.Join(versions(ix, "demo"), ","); got != "v2,1.10.0-rc.1,01.5.0,v1.2.3,1.2,1.0.0" {
		t.Fatalf("order: %s", got)
	}
	if w := e.do("PUT", "helm-hosted", "demo-v1.2.3.tgz", chartTgz(t, "demo", "v1.2.3", "name: demo\nversion: v1.2.3\n")); w.Code != 409 {
		t.Fatalf("republish: %d %s", w.Code, w.Body)
	}
	// a file name that reads two ways: helm takes the version, and DELETE finds its row
	for _, v := range []string{"1.0.0-2.3.4", "3.0-2"} {
		if w := e.do("PUT", "helm-hosted", ChartFile("ambi", v), chartTgz(t, "ambi", v, "name: ambi\nversion: "+v+"\n")); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", v, w.Code, w.Body)
		}
		if got := versions(index(t, e.do("GET", "helm-hosted", "index.yaml", nil)), "ambi"); !strings.Contains(strings.Join(got, ","), v) {
			t.Fatalf("%s is not in the index: %v", v, got)
		}
	}
	other := chartTgz(t, "ambi-1.0.0", "2.3.4", "name: ambi-1.0.0\nversion: 2.3.4\n") // another chart's file name
	if w := e.do("PUT", "helm-hosted", "ambi-1.0.0-2.3.4.tgz", other); w.Code != 409 || !strings.Contains(w.Body.String(), "another chart") {
		t.Fatalf("a file of another chart: %d %s", w.Code, w.Body)
	}
	if ix := index(t, e.do("GET", "helm-hosted", "index.yaml", nil)); len(ix.Entries["ambi-1.0.0"]) != 0 {
		t.Fatalf("the refused chart is listed: %+v", ix.Entries["ambi-1.0.0"])
	}
	if w := e.do("DELETE", "helm-hosted", "ambi-1.0.0-2.3.4.tgz", nil); w.Code != 200 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body)
	}
	if got := strings.Join(versions(index(t, e.do("GET", "helm-hosted", "index.yaml", nil)), "ambi"), ","); got != "3.0-2" {
		t.Fatalf("after the DELETE: %s", got)
	}
	// unquoted in a chart that helm package did not write: a number, as helm reads it
	for _, c := range [][2]string{{"7.5", "7.5"}, {"8", "8"}, {"9.10", "9.1"}} {
		if w := e.do("PUT", "helm-hosted", ChartFile("num", c[1]), chartTgz(t, "num", c[1], "name: num\nversion: "+c[0]+"\n")); w.Code != 201 {
			t.Fatalf("version: %s: %d %s", c[0], w.Code, w.Body)
		}
	}
	if got := strings.Join(versions(index(t, e.do("GET", "helm-hosted", "index.yaml", nil)), "num"), ","); got != "9.1,8,7.5" {
		t.Fatalf("unquoted versions: %s", got)
	}
	if w := e.do("DELETE", "helm-hosted", "demo-v1.2.3.tgz", nil); w.Code != 200 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body)
	}
	if got := strings.Join(versions(index(t, e.do("GET", "helm-hosted", "index.yaml", nil)), "demo"), ","); got != "v2,1.10.0-rc.1,01.5.0,1.2,1.0.0" {
		t.Fatalf("after DELETE: %s", got)
	}
	// the owner rule of a group holds for such a file name too: hosted owns
	// demo, so the proxy's demo is not served
	e.files.Store("/charts/demo-v9.0.0.tgz", chartTgz(t, "demo", "v9.0.0", "name: demo\nversion: v9.0.0\n"))
	idx := strings.Replace(*e.index.Load(), "  other:", "  - name: demo\n    version: v9.0.0\n    urls:\n    - demo-v9.0.0.tgz\n  other:", 1)
	e.index.Store(&idx)
	if w := e.do("GET", "helm-proxy", "demo-v9.0.0.tgz", nil); w.Code != 200 {
		t.Fatalf("the proxy's own chart: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "helm-all", "demo-v9.0.0.tgz", nil); w.Code != 404 {
		t.Fatalf("a version of a chart the hosted member owns: %d, want 404", w.Code)
	}
}

// republish replaces the upstream's demo 2.0.0 with other bytes: the file,
// its provenance file and the digest in the index.
func (e *env) republish(t *testing.T, description string) (chart, prov []byte) {
	t.Helper()
	chart = chartTgz(t, "demo", "2.0.0", "name: demo\nversion: 2.0.0\ndescription: "+description+"\n")
	prov = provFor("demo-2.0.0.tgz", chart)
	old := "digest: " + sum(upChart["demo-2.0.0.tgz"])
	if b, ok := e.files.Load("/files/demo-2.0.0.tgz"); ok {
		old = "digest: " + sum(b.([]byte))
	}
	idx := *e.index.Load()
	if !strings.Contains(idx, old) {
		t.Fatalf("upstream index without %s", old)
	}
	idx = strings.Replace(idx, old, "digest: "+sum(chart), 1)
	e.files.Store("/files/demo-2.0.0.tgz", chart)
	e.files.Store("/files/demo-2.0.0.tgz.prov", prov)
	e.index.Store(&idx)
	return chart, prov
}

// A proxy's cached chart is content: it is fetched again when the index
// lists another digest for it (the version was published again), after
// invalidate-cache and after contentMaxAge, and its provenance file follows.
func TestProxyChartFollowsRemote(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	get := func(path string) []byte {
		t.Helper()
		w := e.do("GET", "helm-proxy", path, nil)
		if w.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
		}
		return w.Body.Bytes()
	}
	e.files.Store("/files/demo-2.0.0.tgz.prov", provFor("demo-2.0.0.tgz", upChart["demo-2.0.0.tgz"]))
	first, firstProv := get("demo-2.0.0.tgz"), get("demo-2.0.0.tgz.prov")
	if !bytes.Equal(first, upChart["demo-2.0.0.tgz"]) || !bytes.Equal(firstProv, provFor("demo-2.0.0.tgz", first)) {
		t.Fatal("first fetch")
	}
	if w := e.do("GET", "helm-proxy", "other-1.0.0.tgz.prov", nil); w.Code != 404 {
		t.Fatalf("a chart the remote has no provenance file for: %d", w.Code)
	}
	// published again: nothing changes while the cached index stands
	second, secondProv := e.republish(t, "second")
	if !bytes.Equal(get("demo-2.0.0.tgz"), first) {
		t.Fatal("within metadataMaxAge the cached index and chart are served")
	}
	// the index is read again after metadataMaxAge: its digest is no longer the cached chart's
	e.now = e.now.Add(2 * time.Hour)
	if got := index(t, e.do("GET", "helm-proxy", "index.yaml", nil)).Entries["demo"][0].str("digest"); got != sum(second) {
		t.Fatalf("index digest %s", got)
	}
	if !bytes.Equal(get("demo-2.0.0.tgz"), second) || !bytes.Equal(get("demo-2.0.0.tgz.prov"), secondProv) {
		t.Fatal("the chart and its provenance file were not fetched again, though contentMaxAge never expires")
	}
	hits := e.hits.Load()
	get("demo-2.0.0.tgz")
	get("demo-2.0.0.tgz.prov")
	if e.hits.Load() != hits {
		t.Fatalf("cached again: %d more upstream requests", e.hits.Load()-hits)
	}
	// invalidate-cache: the index and the chart are asked for again at once
	third, _ := e.republish(t, "third")
	if err := e.h.Store.DB.InvalidateCache(ctx, []string{"helm-proxy"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(get("demo-2.0.0.tgz"), third) {
		t.Fatal("after invalidate-cache the old chart is served")
	}
	// the remote's file is not what its index lists: the cached chart is served, marked stale, and nothing else is kept
	idx := strings.Replace(*e.index.Load(), "digest: "+sum(third), "digest: "+sum([]byte("not the chart")), 1)
	e.index.Store(&idx)
	if err := e.h.Store.DB.InvalidateCache(ctx, []string{"helm-proxy"}); err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "helm-proxy", "demo-2.0.0.tgz", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), third) || !strings.Contains(w.Header().Get("Warning"), "Stale") {
		t.Fatalf("a remote file that does not match its index: %d %v", w.Code, w.Header())
	}
	idx = strings.Replace(idx, "digest: "+sum([]byte("not the chart")), "digest: "+sum(third), 1)
	e.index.Store(&idx)
	e.now = e.now.Add(2 * time.Hour)
	// contentMaxAge: an expired chart is checked with the remote
	e.repos["helm-proxy"].ContentMaxAge = config.Duration(time.Hour)
	hits = e.hits.Load()
	get("other-1.0.0.tgz")
	get("other-1.0.0.tgz")
	e.now = e.now.Add(90 * time.Minute)
	get("other-1.0.0.tgz")
	if got := e.hits.Load() - hits; got != 2 {
		t.Fatalf("upstream requests for an expiring chart: %d, want 2", got)
	}
	// a chart the index no longer lists, and any cached chart while the remote is down, is still served
	idx = strings.Replace(*e.index.Load(), "  other:", "  gone:", 1)
	e.index.Store(&idx)
	e.now = e.now.Add(2 * time.Hour)
	if got := versions(index(t, e.do("GET", "helm-proxy", "index.yaml", nil)), "other"); got != nil {
		t.Fatalf("other is still listed: %v", got)
	}
	get("other-1.0.0.tgz")
	e.down.Store(true)
	e.now = e.now.Add(2 * time.Hour)
	if !bytes.Equal(get("demo-2.0.0.tgz"), third) {
		t.Fatal("cached chart while the remote is down")
	}
}

// A chart published since the index was read is found by its URL: the
// remote is asked for the index again, at most once a minute.
func TestProxyChartNewerThanIndex(t *testing.T) {
	e := newEnv(t)
	index(t, e.do("GET", "helm-proxy", "index.yaml", nil))
	newer := chartTgz(t, "other", "1.1.0", "")
	e.files.Store("/charts/other-1.1.0.tgz", newer)
	idx := strings.Replace(*e.index.Load(), "    version: 1.0.0\n  bad:", "    version: 1.0.0\n  - name: other\n    version: 1.1.0\n    digest: "+sum(newer)+"\n    urls:\n    - other-1.1.0.tgz\n  bad:", 1)
	e.index.Store(&idx)
	asked := e.idxHits.Load()
	if w := e.do("GET", "helm-proxy", "other-1.1.0.tgz", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), newer) {
		t.Fatalf("a chart newer than the cached index: %d %s", w.Code, w.Body)
	}
	if got := strings.Join(versions(index(t, e.do("GET", "helm-proxy", "index.yaml", nil)), "other"), ","); got != "1.0.0,1.1.0" {
		t.Fatalf("the proxy's index after it: %s", got)
	}
	for i := 0; i < 3; i++ {
		if w := e.do("GET", "helm-proxy", "nothere-1.0.0.tgz", nil); w.Code != 404 {
			t.Fatalf("a chart the remote does not have: %d", w.Code)
		}
	}
	if got := e.idxHits.Load() - asked; got != 1 {
		t.Fatalf("the remote was asked for its index %d times within a minute, want 1", got)
	}
	e.now = e.now.Add(2 * time.Minute)
	if w := e.do("GET", "helm-proxy", "nothere-1.0.0.tgz", nil); w.Code != 404 || e.idxHits.Load()-asked != 2 {
		t.Fatalf("a minute later: %d, index requests %d", w.Code, e.idxHits.Load()-asked)
	}
}

// A proxy's routing rule sees the request path of a chart, not the path of
// its upstream URL, cached or not, directly and as a group member.
func TestProxyRoutingRuleSeesRequestPath(t *testing.T) {
	e := newEnv(t)
	if w := e.do("GET", "helm-proxy", "other-1.0.0.tgz", nil); w.Code != 200 { // cached before the rule
		t.Fatal(w.Code)
	}
	rule, err := repo.CompileRoute(meta.RoutingRule{Name: "only-demo", Mode: meta.RoutingAllow, Matchers: []string{`^/index\.yaml$`, `^/demo-.*`}})
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	e.h.Fetcher.RouteAllowed = func(rp *repo.Repository, path string) bool {
		seen = append(seen, path)
		ok, _ := rule.Allowed(path)
		return ok
	}
	// demo 2.0.0 lives at /files/demo-2.0.0.tgz on the remote: the rule allows its request path
	for _, rp := range []string{"helm-proxy", "helm-proxy-first"} {
		if w := e.do("GET", rp, "demo-2.0.0.tgz", nil); w.Code != 200 {
			t.Fatalf("%s: allowed chart: %d %s (rule saw %v)", rp, w.Code, w.Body, seen)
		}
		if w := e.do("GET", rp, "other-1.0.0.tgz", nil); w.Code != 404 {
			t.Fatalf("%s: a cached chart the rule refuses: %d", rp, w.Code)
		}
		if w := e.do("GET", rp, "other-1.0.0.tgz.prov", nil); w.Code != 404 {
			t.Fatalf("%s: its provenance file: %d", rp, w.Code)
		}
	}
	for _, p := range seen {
		if strings.HasPrefix(p, "/files/") || strings.HasPrefix(p, "/charts/") {
			t.Fatalf("the rule saw an upstream path: %v", seen)
		}
	}
	// a cached chart the index no longer lists is served from the cache alone: the rule still comes first
	idx := strings.Replace(*e.index.Load(), "  other:", "  gone:", 1)
	e.index.Store(&idx)
	e.now = e.now.Add(2 * time.Hour)
	if got := versions(index(t, e.do("GET", "helm-proxy", "index.yaml", nil)), "other"); got != nil {
		t.Fatalf("other is still listed: %v", got)
	}
	if w := e.do("GET", "helm-proxy", "other-1.0.0.tgz", nil); w.Code != 404 {
		t.Fatalf("a cached chart the index does not list and the rule refuses: %d", w.Code)
	}
	e.h.Fetcher.RouteAllowed = nil
	if w := e.do("GET", "helm-proxy", "other-1.0.0.tgz", nil); w.Code != 200 {
		t.Fatalf("the same chart without the rule: %d", w.Code)
	}
}

// Every request for a chart reads the upstream table: concurrent ones on a
// node that has not read the index yet all get it (run with -race). That they
// read the index once is a memory matter no test here can tell.
func TestUpstreamTableConcurrentFirstRead(t *testing.T) {
	e := newEnv(t)
	rp := e.repos["helm-proxy"]
	if _, err := e.h.upstreamFor(context.Background(), rp); err != nil {
		t.Fatal(err)
	}
	first, _ := e.h.upstreams.Load("helm-proxy")
	e.h.upstreams.Delete("helm-proxy") // another node, or a restart: the cached index is here, its table is not
	var wg sync.WaitGroup
	got := make([]*upstream, 16)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = e.h.upstreamFor(context.Background(), rp)
		}()
	}
	wg.Wait()
	for i, up := range got {
		if up == nil || len(up.charts) != len(first.(*upstream).charts) {
			t.Fatalf("#%d: %+v", i, up)
		}
	}
}
