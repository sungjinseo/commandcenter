// Command nexusdiff sends the same requests to a Nexus Repository 3 and to a
// Command Center and reports where the answers differ: the status, the JSON
// shape (keys and types) and the values that do not change from one server to
// the next. A difference listed in known.txt, with its reason, is expected;
// any other fails the run. test/compat/nexus-diff.sh runs it.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// server is one side of the comparison.
type server struct {
	base, user, pass string
}

// reply is what a step got back.
type reply struct {
	status int
	ctype  string
	body   []byte
}

func (s server) do(st step) reply {
	var body io.Reader
	ctype := st.ctype
	switch {
	case st.parts != nil:
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		keys := make([]string, 0, len(st.parts))
		for k := range st.parts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := st.parts[k]
			if name, ok := strings.CutPrefix(v, "file:"); ok {
				f, _ := w.CreateFormFile(k, name)
				io.WriteString(f, "content of "+name+"\n")
			} else {
				w.WriteField(k, v)
			}
		}
		w.Close()
		body, ctype = &b, w.FormDataContentType()
	case st.body != "":
		body = strings.NewReader(st.body)
		if ctype == "" {
			ctype = "application/json"
		}
	}
	req, err := http.NewRequest(st.method, s.base+st.path, body)
	if err != nil {
		return reply{status: -1, body: []byte(err.Error())}
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range st.header {
		req.Header.Set(k, v)
	}
	switch st.as {
	case "":
		req.SetBasicAuth(s.user, s.pass)
	case "anon":
	default:
		req.SetBasicAuth(st.as, "Diff-password-1")
	}
	c := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return reply{status: -1, body: []byte(err.Error())}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return reply{resp.StatusCode, resp.Header.Get("Content-Type"), b}
}

// volatile are JSON keys whose values differ between any two servers.
var volatile = map[string]bool{"id": true, "lastModified": true, "blobCreated": true, "blobStoreCreated": true, "lastDownloaded": true,
	"lastVerified": true, "blobUpdated": true, "uploaderIp": true, "uploader": true, "nextRun": true, "lastRun": true, "message": true,
	"continuationToken": true, "startDate": true, "checksum": true, "availableSpaceInBytes": true, "totalSizeInBytes": true, "blobCount": true, "version": true}

// compareJSON appends how got differs from want, where want is the reference.
func compareJSON(path string, want, got any, base [2]string, out *[]string) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("shape %s: %s here, object in Nexus", path, kind(got)))
			return
		}
		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}
		for k := range g {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			wv, inW := w[k]
			gv, inG := g[k]
			switch {
			case !inG:
				*out = append(*out, fmt.Sprintf("shape %s.%s: missing here (Nexus: %s)", path, k, kind(wv)))
			case !inW:
				*out = append(*out, fmt.Sprintf("extra %s.%s: not in Nexus", path, k))
			case !volatile[k]:
				compareJSON(path+"."+k, wv, gv, base, out)
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("shape %s: %s here, array in Nexus", path, kind(got)))
			return
		}
		if len(w) != len(g) {
			*out = append(*out, fmt.Sprintf("value %s: %d items here, %d in Nexus", path, len(g), len(w)))
		}
		for i := 0; i < min(len(w), len(g)); i++ {
			compareJSON(fmt.Sprintf("%s[%d]", path, i), w[i], g[i], base, out)
		}
	default:
		if kind(want) != kind(got) {
			*out = append(*out, fmt.Sprintf("shape %s: %s here, %s in Nexus", path, kind(got), kind(want)))
			return
		}
		ws, gs := fmt.Sprint(want), fmt.Sprint(got)
		if ws != strings.ReplaceAll(gs, base[1], base[0]) {
			*out = append(*out, fmt.Sprintf("value %s: %.120q here, %.120q in Nexus", path, gs, ws))
		}
	}
}

func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "bool"
	}
	return "number"
}

// compare lists the differences between a step's two answers.
func compare(st step, a, b reply, base [2]string) []string {
	var out []string
	if a.status != b.status {
		out = append(out, fmt.Sprintf("status: %d here, %d in Nexus (here: %.200s) (Nexus: %.200s)", b.status, a.status, oneLine(b.body), oneLine(a.body)))
		return out
	}
	if st.raw {
		if !bytes.Equal(a.body, b.body) {
			out = append(out, fmt.Sprintf("body: differs (here %.200q) (Nexus %.200q)", b.body, a.body))
		}
		return out
	}
	var ja, jb any
	ea, eb := json.Unmarshal(a.body, &ja), json.Unmarshal(b.body, &jb)
	switch {
	case ea != nil && eb != nil: // neither is JSON: the status said it all
	case ea != nil && len(bytes.TrimSpace(a.body)) > 0:
		out = append(out, fmt.Sprintf("shape: JSON here, not in Nexus (%.200s)", oneLine(a.body)))
	case eb != nil && len(bytes.TrimSpace(b.body)) > 0:
		out = append(out, fmt.Sprintf("shape: not JSON here (%.200s), JSON in Nexus", oneLine(b.body)))
	case st.status: // an error's or a listing's text is not the comparison
	default:
		compareJSON("$", ja, jb, base, &out)
	}
	return out
}

func oneLine(b []byte) string { return strings.Join(strings.Fields(string(b)), " ") }

// known reads "step<TAB>prefix of the difference<TAB>reason" lines.
func known(file string) map[string][]string {
	out := map[string][]string{}
	b, err := os.ReadFile(file)
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, "\t")
		if len(f) >= 3 && !strings.HasPrefix(l, "#") {
			out[f[0]] = append(out[f[0]], f[1])
		}
	}
	return out
}

func main() {
	nexus := flag.String("nexus", "", "URL of the Nexus Repository 3 (the reference)")
	cc := flag.String("cc", "", "URL of the Command Center")
	nexusPW := flag.String("nexus-password-file", "", "file with the Nexus admin password")
	ccPW := flag.String("cc-password-file", "", "file with the Command Center admin password")
	knownFile := flag.String("known", "test/compat/nexusdiff/known.txt", "the expected differences")
	flag.Parse()
	read := func(f string) string {
		b, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return strings.TrimSpace(string(b))
	}
	a := server{strings.TrimRight(*nexus, "/"), "admin", read(*nexusPW)}
	b := server{strings.TrimRight(*cc, "/"), "admin", read(*ccPW)}
	base := [2]string{a.base, b.base}
	expected := known(*knownFile)
	var unexpected, explained, same int
	for _, st := range steps() {
		ra, rb := a.do(st), b.do(st)
		diffs := compare(st, ra, rb, base)
		st.after(ra, rb)
		if len(diffs) == 0 {
			same++
			continue
		}
		for _, d := range diffs {
			ok := false
			for _, p := range expected[st.name] {
				ok = ok || strings.HasPrefix(d, p)
			}
			if ok {
				explained++
				continue
			}
			// "extra" is what Command Center adds; it breaks nobody.
			if strings.HasPrefix(d, "extra ") {
				fmt.Printf("note\t%s\t%s\t%s\n", st.name, st.method+" "+st.path, d)
				continue
			}
			unexpected++
			fmt.Printf("DIFF\t%s\t%s\t%s\n", st.name, st.method+" "+st.path, d)
		}
	}
	fmt.Printf("%d steps the same, %d differences expected, %d not\n", same, explained, unexpected)
	if unexpected > 0 {
		os.Exit(1)
	}
}

// step is one request sent to both servers.
type step struct {
	name, method, path, body, ctype, as string
	parts                               map[string]string
	header                              map[string]string
	raw                                 bool // compare the body byte for byte
	status                              bool // compare the status only
	hook                                func(a, b reply)
}

func (st step) after(a, b reply) {
	if st.hook != nil {
		st.hook(a, b)
	}
}

const v1 = "/service/rest/v1"

func q(s string) string { return url.QueryEscape(s) }
