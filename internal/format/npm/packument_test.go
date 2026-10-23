package npm

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"commandcenter/internal/repo"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("invalid JSON %q: %v", b, err)
	}
	return m
}

func get(m map[string]any, path ...string) any {
	var v any = m
	for _, p := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[p]
	}
	return v
}

func url(file string) string { return "http://repo/repository/npm-all/left-pad/-/" + file }

const leftPad = `{"_id":"left-pad","name":"left-pad","description":"pad <left> & é",
 "dist-tags":{"latest":"1.1.0","beta":"2.0.0-beta.1"},
 "versions":{
  "1.0.0":{"name":"left-pad","version":"1.0.0","dist":{"tarball":"https://registry.npmjs.org/left-pad/-/left-pad-1.0.0.tgz","shasum":"a"},"size":12345678901234567890},
  "1.1.0":{"name":"left-pad","version":"1.1.0","dist":{"tarball":"https://registry.npmjs.org/left-pad/-/left-pad-1.1.0.tgz?x=1"},"keywords":["a",["b",{"c":null}]],"ok":true}
 },
 "time":{"created":"2020-01-01T00:00:00.000Z","1.0.0":"2020-01-01T00:00:00.000Z"},
 "readme":"see tarball: https://registry.npmjs.org/left-pad/-/left-pad-1.0.0.tgz"}`

func TestRewritePackument(t *testing.T) {
	var out bytes.Buffer
	if err := rewritePackument(strings.NewReader(leftPad), &out, url); err != nil {
		t.Fatal(err)
	}
	m := decode(t, out.Bytes())
	if get(m, "versions", "1.0.0", "dist", "tarball") != url("left-pad-1.0.0.tgz") ||
		get(m, "versions", "1.1.0", "dist", "tarball") != url("left-pad-1.1.0.tgz") {
		t.Fatalf("tarballs not rewritten: %s", out.String())
	}
	if !strings.Contains(get(m, "readme").(string), "registry.npmjs.org") {
		t.Fatal("only dist.tarball may be rewritten")
	}
	if get(m, "versions", "1.0.0", "size").(json.Number).String() != "12345678901234567890" {
		t.Fatal("big numbers must be preserved exactly")
	}
	if get(m, "description") != "pad <left> & é" {
		t.Fatalf("description = %q", get(m, "description"))
	}
	want := decode(t, []byte(leftPad))
	if !reflect.DeepEqual(get(m, "versions", "1.1.0", "keywords"), get(want, "versions", "1.1.0", "keywords")) {
		t.Fatal("nested arrays must round-trip")
	}
}

const hostedDoc = `{"name":"left-pad","description":"internal fork",
 "dist-tags":{"latest":"1.0.1","beta":"9.9.9"},
 "versions":{
  "1.0.0":{"name":"left-pad","version":"1.0.0","from":"hosted"},
  "1.0.1":{"name":"left-pad","version":"1.0.1","from":"hosted"}
 },
 "time":{"1.0.1":"2026-01-01T00:00:00.000Z"}}`

func TestMergePackuments(t *testing.T) {
	var out bytes.Buffer
	members := []io.ReadSeeker{strings.NewReader(hostedDoc), strings.NewReader(leftPad)}
	if err := mergePackuments(members, &out); err != nil {
		t.Fatal(err)
	}
	m := decode(t, out.Bytes())
	if get(m, "description") != "internal fork" {
		t.Fatal("first member wins for top-level fields")
	}
	if get(m, "dist-tags", "latest") != "1.0.1" || get(m, "dist-tags", "beta") != "9.9.9" {
		t.Fatalf("dist-tags = %v", get(m, "dist-tags"))
	}
	if get(m, "versions", "1.0.0", "from") != "hosted" || get(m, "versions", "1.1.0", "version") != "1.1.0" || get(m, "versions", "1.0.1") == nil {
		t.Fatalf("versions = %v", get(m, "versions"))
	}
	if get(m, "time", "1.0.1") == nil || get(m, "time", "created") == nil {
		t.Fatalf("time = %v", get(m, "time"))
	}
}

func TestMergePackumentsSkipsBrokenMember(t *testing.T) {
	var out bytes.Buffer
	members := []io.ReadSeeker{strings.NewReader(`not json`), strings.NewReader(hostedDoc)}
	if err := mergePackuments(members, &out); err != nil {
		t.Fatal(err)
	}
	if get(decode(t, out.Bytes()), "versions", "1.0.1") == nil {
		t.Fatal("valid member must still be merged")
	}
	if err := mergePackuments([]io.ReadSeeker{strings.NewReader(`[]`)}, &out); err == nil {
		t.Fatal("all-broken members must fail")
	}
}

func TestWriteVersionDoc(t *testing.T) {
	for _, want := range []string{"1.1.0", "latest"} {
		var out bytes.Buffer
		if err := writeVersionDoc(strings.NewReader(leftPad), want, url, &out); err != nil {
			t.Fatal(err)
		}
		m := decode(t, out.Bytes())
		if get(m, "version") != "1.1.0" || get(m, "dist", "tarball") != url("left-pad-1.1.0.tgz") {
			t.Fatalf("%s: %s", want, out.String())
		}
	}
	var out bytes.Buffer
	if err := writeVersionDoc(strings.NewReader(leftPad), "3.0.0", url, &out); repo.StatusOf(err) != 404 || out.Len() != 0 {
		t.Fatalf("missing version: err=%v wrote=%q", err, out.String())
	}
}

func TestDistTags(t *testing.T) {
	tags, err := readDistTags(strings.NewReader(leftPad))
	if err != nil || len(tags) != 2 || tags[0] != (tagPair{"latest", "1.1.0"}) {
		t.Fatalf("tags=%v err=%v", tags, err)
	}
	var out bytes.Buffer
	if err := writeDistTags(&out, tags); err != nil {
		t.Fatal(err)
	}
	if out.String() != `{"latest":"1.1.0","beta":"2.0.0-beta.1"}` {
		t.Fatalf("got %s", out.String())
	}
}

func TestFileFromURL(t *testing.T) {
	cases := map[string]string{
		"https://r/x/-/x-1.0.0.tgz":        "x-1.0.0.tgz",
		"https://r/@s/x/-/x-1.0.0.tgz?a=b": "x-1.0.0.tgz",
		"x-1.0.0.tgz":                      "x-1.0.0.tgz",
	}
	for in, want := range cases {
		if got := fileFromURL(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

func TestMergeTakesFieldsFromLaterMembers(t *testing.T) {
	var out bytes.Buffer
	members := []io.ReadSeeker{strings.NewReader(hostedDoc), strings.NewReader(leftPad)}
	if err := mergePackuments(members, &out); err != nil {
		t.Fatal(err)
	}
	m := decode(t, out.Bytes())
	if get(m, "readme") == nil {
		t.Fatalf("readme must come from the later member: %s", out.String())
	}
	if get(m, "_id") != "left-pad" {
		t.Fatalf("_id = %v", get(m, "_id"))
	}
	if get(m, "description") != "internal fork" {
		t.Fatalf("description = %v", get(m, "description"))
	}
}

func TestMergeToleratesOddSections(t *testing.T) {
	const odd = `{"name":"x","versions":null,"time":[1,{"a":2}],"dist-tags":"weird"}`
	var out bytes.Buffer
	members := []io.ReadSeeker{strings.NewReader(odd), strings.NewReader(hostedDoc)}
	if err := mergePackuments(members, &out); err != nil {
		t.Fatal(err)
	}
	m := decode(t, out.Bytes())
	if get(m, "versions", "1.0.1") == nil {
		t.Fatalf("hosted version must survive odd sections: %s", out.String())
	}
	if get(m, "dist-tags", "latest") != "1.0.1" {
		t.Fatalf("dist-tags = %v", get(m, "dist-tags"))
	}
}

func TestWriteVersionDocNonObjectVersions(t *testing.T) {
	var out bytes.Buffer
	err := writeVersionDoc(strings.NewReader(`{"versions":[]}`), "1.0.0", url, &out)
	if repo.StatusOf(err) != 404 || out.Len() != 0 {
		t.Fatalf("err=%v wrote=%q", err, out.String())
	}
}

func TestTokenWriterEmptyContainers(t *testing.T) {
	const in = `{"a":{},"b":[],"c":[[],{}]}`
	dec := json.NewDecoder(strings.NewReader(in))
	dec.UseNumber()
	var out bytes.Buffer
	tw := newTokenWriter(&out)
	if err := copyValue(dec, tw, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	if out.String() != in {
		t.Fatalf("got %s", out.String())
	}
}

// TestGroupLatestFollowsMemberOrder: latest comes from the first member (in
// group order) whose document has a latest tag, never from a newer release of a
// later member (QA M-4, Decision 3).
func TestGroupLatestFollowsMemberOrder(t *testing.T) {
	hosted := `{"name":"p","dist-tags":{"latest":"0.5.0"},"versions":{"0.5.0":{"version":"0.5.0"}}}`
	noLatest := `{"name":"p","dist-tags":{"beta":"0.6.0-beta.1"},"versions":{"0.6.0-beta.1":{"version":"0.6.0-beta.1"}}}`
	proxied := `{"name":"p","dist-tags":{"latest":"1.0.1"},"versions":{"1.0.1":{"version":"1.0.1"}}}`
	cases := []struct {
		members []string
		want    string
	}{
		{[]string{hosted, proxied}, "0.5.0"},
		{[]string{proxied, hosted}, "1.0.1"},
		{[]string{noLatest, proxied, hosted}, "1.0.1"},
		{[]string{noLatest}, ""},
	}
	for i, c := range cases {
		var members []io.ReadSeeker
		for _, m := range c.members {
			members = append(members, strings.NewReader(m))
		}
		var out bytes.Buffer
		if err := mergePackuments(members, &out); err != nil {
			t.Fatal(err)
		}
		m := decode(t, out.Bytes())
		if got, _ := get(m, "dist-tags", "latest").(string); got != c.want {
			t.Errorf("case %d: latest = %q, want %q", i, got, c.want)
		}
		if get(m, "versions", "1.0.1") == nil && i < 3 {
			t.Errorf("case %d: versions must still be merged: %v", i, get(m, "versions"))
		}
	}
}
