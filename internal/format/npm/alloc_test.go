package npm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// TestRewritePackumentAllocs: copying a packument allocates about its size
// once (the decoder's strings), not once more per string written. 64 clients
// reading an 18 MB packument through a proxy took a 128 MiB server down on a
// busy host when it was four times.
func TestRewritePackumentAllocs(t *testing.T) {
	var b bytes.Buffer
	b.WriteString(`{"name":"big","dist-tags":{"latest":"1.499.0"},"versions":{`)
	pad := strings.Repeat("x", 8<<10)
	for i := range 500 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"1.%d.0":{"name":"big","version":"1.%d.0","description":"%s","readme":"<b>\"q\" & é</b>","dist":{"tarball":"https://r.example/big/-/big-1.%d.0.tgz"}}`, i, i, pad, i)
	}
	b.WriteString(`}}`)
	doc := b.Bytes()
	var out bytes.Buffer
	out.Grow(len(doc) + len(doc)/4)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	err := rewritePackument(bytes.NewReader(doc), &out, func(v, u string) string { return "http://cc/big/-/big-" + v + ".tgz" })
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > uint64(len(doc))*3/2 && !raceEnabled {
		t.Errorf("allocated %d KiB copying %d KiB", got>>10, len(doc)>>10)
	}
	// What is written is what json.Marshal writes.
	var in, res map[string]any
	if err := json.Unmarshal(doc, &in); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("%v: %.200s", err, out.Bytes())
	}
	v := res["versions"].(map[string]any)["1.7.0"].(map[string]any)
	if v["description"] != pad || v["readme"] != "<b>\"q\" & é</b>" || v["dist"].(map[string]any)["tarball"] != "http://cc/big/-/big-1.7.0.tgz" {
		t.Fatalf("1.7.0: %.100v", v)
	}
	want, _ := json.Marshal(v["readme"])
	if !bytes.Contains(out.Bytes(), append([]byte(`"readme":`), want...)) {
		t.Fatalf("escaping differs from json.Marshal's: %.300s", out.Bytes()[bytes.Index(out.Bytes(), []byte(`"readme"`)):])
	}
}
