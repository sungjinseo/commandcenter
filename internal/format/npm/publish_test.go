package npm

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/repo"
)

func collect(got map[string][]byte) func(string, io.Reader) error {
	return func(name string, r io.Reader) error {
		b, err := io.ReadAll(r)
		got[name] = b
		return err
	}
}

func TestSplitPublish(t *testing.T) {
	tgz := bytes.Repeat([]byte("tarball-bytes/"), 50_000) // ~700 KB, contains '/'
	data := base64.StdEncoding.EncodeToString(tgz)
	escaped := strings.ReplaceAll(data, "/", `\/`) // some encoders escape slashes
	doc := fmt.Sprintf(`{"name":"@s/p","versions":{"1.0.0":{"name":"@s/p","version":"1.0.0","data":"keep me"}},
 "_attachments":{"@s/p-1.0.0.tgz":{"content_type":"application/octet-stream","data":%q,"length":%d},
                 "other.tgz":{"data":"%s"}}}`, data, len(tgz), escaped)
	got := map[string][]byte{}
	manifest, err := splitPublish(strings.NewReader(doc), collect(got))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got["@s/p-1.0.0.tgz"], tgz) || !bytes.Equal(got["other.tgz"], tgz) {
		t.Fatalf("attachments: %d %d bytes", len(got["@s/p-1.0.0.tgz"]), len(got["other.tgz"]))
	}
	if len(manifest) > 1000 {
		t.Fatalf("manifest still holds attachment data: %d bytes", len(manifest))
	}
	var m map[string]any
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatalf("manifest not JSON: %v\n%s", err, manifest)
	}
	if get(m, "versions", "1.0.0", "data") != "keep me" || get(m, "_attachments", "other.tgz", "data") != "" {
		t.Fatalf("manifest = %s", manifest)
	}
}

func TestSplitPublishErrors(t *testing.T) {
	for _, doc := range []string{`{"a":`, `{"a":"unterminated`, `{"a":1}}`} {
		if _, err := splitPublish(strings.NewReader(doc), collect(map[string][]byte{})); repo.StatusOf(err) != 400 {
			t.Errorf("%q: err = %v", doc, err)
		}
	}
	huge := `{"readme":"` + strings.Repeat("x", 11<<20) + `"}`
	if _, err := splitPublish(strings.NewReader(huge), collect(map[string][]byte{})); repo.StatusOf(err) != 413 {
		t.Errorf("huge manifest: %v", err)
	}
	bad := `{"_attachments":{"a.tgz":{"data":"!!!not base64"}}}`
	if _, err := splitPublish(strings.NewReader(bad), collect(map[string][]byte{})); repo.StatusOf(err) != 400 {
		t.Errorf("invalid base64: err = %v", err)
	}
}

func TestVerifyTarball(t *testing.T) {
	tgz := []byte("content")
	s1 := sha1.Sum(tgz)
	s512 := sha512.Sum512(tgz)
	sums := blob.Sums{SHA1: hex.EncodeToString(s1[:]), SHA512: hex.EncodeToString(s512[:])}
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(s512[:])
	if err := verifyTarball(sums.SHA1, integrity, sums); err != nil {
		t.Fatal(err)
	}
	if err := verifyTarball("", "", sums); err != nil {
		t.Fatal(err)
	}
	if err := verifyTarball("deadbeef", "", sums); repo.StatusOf(err) != 400 {
		t.Fatalf("shasum: %v", err)
	}
	if err := verifyTarball("", "sha512-AAAA", sums); repo.StatusOf(err) != 400 {
		t.Fatalf("integrity: %v", err)
	}
}

func TestApplyDeprecation(t *testing.T) {
	old := `{"version":"1.0.0","description":"d","dependencies":{"a":"1"},"dist":{"tarball":"orig","shasum":"x"}}`
	check := func(name, newM, wantDep string) {
		t.Helper()
		got, err := applyDeprecation(json.RawMessage(newM), old)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		json.Unmarshal([]byte(got), &m)
		if wantDep == "" {
			if _, ok := m["deprecated"]; ok {
				t.Errorf("%s: deprecated must be absent: %s", name, got)
			}
		} else if m["deprecated"] != wantDep {
			t.Errorf("%s: deprecated = %v", name, m["deprecated"])
		}
		if get(m, "dist", "tarball") != "orig" || get(m, "dist", "shasum") != "x" || m["description"] != "d" || get(m, "dependencies", "a") != "1" {
			t.Errorf("%s: stored fields changed: %s", name, got)
		}
	}
	check("set", `{"version":"1.0.0","deprecated":"old"}`, "old")
	check("ignored changes", `{"version":"1.0.0","deprecated":"old","description":"x","dependencies":{"evil":"1"},"dist":{"tarball":"evil"}}`, "old")
	check("clear empty", `{"version":"1.0.0","deprecated":""}`, "")
	check("clear absent", `{"version":"1.0.0"}`, "")

	// an already deprecated stored version is cleared by an empty string
	got, err := applyDeprecation(json.RawMessage(`{"deprecated":""}`), `{"version":"1.0.0","deprecated":"was"}`)
	if err != nil || strings.Contains(got, "deprecated") {
		t.Fatalf("undeprecate: %s %v", got, err)
	}
}

func TestApplyDeprecationRejectsNonObjects(t *testing.T) {
	if _, err := applyDeprecation(json.RawMessage("null"), "{}"); repo.StatusOf(err) != 400 {
		t.Errorf("null new manifest: err = %v", err)
	}
	if _, err := applyDeprecation(json.RawMessage(`{"version":"1.0.0"}`), "null"); err == nil {
		t.Error("null old attrs must fail")
	}
}

func TestSplitPublishBodyOverCapIs413(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 10_000))
	docs := map[string]string{
		"attachment": fmt.Sprintf(`{"name":"p","_attachments":{"p.tgz":{"data":%q}}}`, data),
		"manifest":   fmt.Sprintf(`{"name":"p","description":%q}`, data),
	}
	for name, doc := range docs {
		w := httptest.NewRecorder()
		body := http.MaxBytesReader(w, io.NopCloser(strings.NewReader(doc)), 100)
		_, err := splitPublish(body, collect(map[string][]byte{}))
		if repo.StatusOf(err) != 413 {
			t.Errorf("%s: status %d (%v), want 413", name, repo.StatusOf(err), err)
		}
	}
}

func TestSplitPublishRejectsDeepNesting(t *testing.T) {
	start := time.Now()
	_, err := splitPublish(strings.NewReader(strings.Repeat("[", 100000)), collect(map[string][]byte{}))
	if repo.StatusOf(err) != 400 || !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("err = %v (status %d), want 400 about nesting depth", err, repo.StatusOf(err))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
	ok := strings.Repeat("[", 64) + strings.Repeat("]", 64)
	if _, err := splitPublish(strings.NewReader(ok), collect(map[string][]byte{})); err != nil {
		t.Fatalf("depth 64 must be allowed: %v", err)
	}
}
