package nexus

import (
	"context"
	"crypto/sha1" //nolint:gosec
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	tUser = "importer"
	tPass = "s3cr3t-P@ss/word"
)

func newClient(t *testing.T, base string, mut ...func(*Options)) *Client {
	t.Helper()
	o := Options{BaseURL: base, Username: tUser, Password: tPass, Timeout: 5 * time.Second}
	for _, m := range mut {
		m(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func authed(r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	return ok && u == tUser && p == tPass
}

func TestNewValidation(t *testing.T) {
	for _, bad := range []string{"", "ftp://x", "file:///etc", "http://", "http://u:p@host", "://x", "nexus.local"} {
		if _, err := New(Options{BaseURL: bad}); err == nil {
			t.Errorf("New(%q) accepted", bad)
		}
	}
	c, err := New(Options{BaseURL: "https://nexus.example:8443/ctx/?a=1#f"})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.base.String(); got != "https://nexus.example:8443/ctx" {
		t.Fatalf("base = %q", got)
	}
}

func TestRepositoriesAndSettings(t *testing.T) {
	var perRepo int
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"name":"mvn","format":"maven2","type":"hosted","url":"x"},{"name":"npmh","format":"npm","type":"hosted","url":"y"}]`)
	})
	mux.HandleFunc("/service/rest/v1/repositorySettings", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"name":"mvn","format":"maven2","type":"hosted","online":true,
		 "storage":{"blobStoreName":"default","writePolicy":"ALLOW_ONCE"},
		 "maven":{"versionPolicy":"RELEASE","layoutPolicy":"STRICT"}}]`)
	})
	mux.HandleFunc("/service/rest/v1/repositories/npm/hosted/npmh", func(w http.ResponseWriter, r *http.Request) {
		perRepo++
		fmt.Fprint(w, `{"name":"npmh","online":true,"storage":{"writePolicy":"ALLOW"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newClient(t, srv.URL)

	repos, err := c.Repositories(context.Background())
	if err != nil || len(repos) != 2 || repos[0].Format != "maven2" {
		t.Fatalf("repos %v %v", repos, err)
	}
	st, err := c.RepositorySettings(context.Background())
	if err != nil || len(st) != 2 {
		t.Fatalf("settings %v %v", st, err)
	}
	if st[0].Storage.WritePolicy != "ALLOW_ONCE" || st[0].Maven == nil || st[0].Maven.VersionPolicy != "RELEASE" {
		t.Fatalf("settings[0] = %+v", st[0])
	}
	if perRepo != 1 || st[1].Name != "npmh" || st[1].Format != "npm" || st[1].Type != "hosted" || st[1].Storage.WritePolicy != "ALLOW" {
		t.Fatalf("fallback: calls=%d %+v", perRepo, st[1])
	}
}

func TestSettingsEndpointMissingFallsBack(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"name":"px","format":"maven2","type":"proxy"}]`)
	})
	mux.HandleFunc("/service/rest/v1/repositories/maven/proxy/px", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"px","proxy":{"remoteUrl":"https://repo1.maven.org/maven2/","contentMaxAge":1440,"metadataMaxAge":1440}}`)
	})
	srv := httptest.NewServer(mux) // /repositorySettings -> 404
	defer srv.Close()
	st, err := newClient(t, srv.URL).RepositorySettings(context.Background())
	if err != nil || len(st) != 1 || st[0].Proxy == nil || st[0].Proxy.ContentMaxAge != 1440 {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestAssetsPagingThreePages(t *testing.T) {
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/service/rest/v1/assets" || r.URL.Query().Get("repository") != "mvn" {
			t.Errorf("bad request %s", r.URL)
		}
		tok := r.URL.Query().Get("continuationToken")
		tokens = append(tokens, tok)
		switch tok {
		case "":
			fmt.Fprint(w, `{"items":[{"path":"a/1","id":"1","fileSize":3,"checksum":{"sha1":"x"}},{"path":"a/2"}],"continuationToken":"t1"}`)
		case "t1":
			fmt.Fprint(w, `{"items":[{"path":"b/1"}],"continuationToken":"t2"}`)
		case "t2":
			fmt.Fprint(w, `{"items":[{"path":"c/1","lastModified":"2026-03-04T05:06:07.123+00:00"}],"continuationToken":null}`)
		default:
			t.Errorf("unexpected token %q", tok)
		}
	}))
	defer srv.Close()

	var got []string
	var last Asset
	err := newClient(t, srv.URL).Assets(context.Background(), "mvn", func(a Asset) error {
		got = append(got, a.Path)
		last = a
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "a/1,a/2,b/1,c/1" || strings.Join(tokens, "|") != "|t1|t2" {
		t.Fatalf("got %v tokens %v", got, tokens)
	}
	if last.ModTime().Year() != 2026 {
		t.Fatalf("modtime %v", last.ModTime())
	}
}

func TestAssetsCallbackErrorStopsAndTokenLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"items":[{"path":"p"}],"continuationToken":"same"}`)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	stop := errors.New("stop")
	if err := c.Assets(context.Background(), "r", func(Asset) error { return stop }); err != stop {
		t.Fatalf("err = %v", err)
	}
	err := c.Assets(context.Background(), "r", func(Asset) error { return nil })
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("loop err = %v", err)
	}
}

func TestAuthFailureAndNotFoundAndNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/repositories"):
			w.WriteHeader(401)
			fmt.Fprint(w, "bad credentials for "+tUser+":"+tPass)
		case strings.HasSuffix(r.URL.Path, "/assets"):
			w.WriteHeader(403)
		default:
			w.WriteHeader(404)
		}
	}))
	c := newClient(t, srv.URL)
	_, err := c.Repositories(context.Background())
	if !errors.Is(err, ErrAuth) || errors.Is(err, ErrNotFound) {
		t.Fatalf("401: %v", err)
	}
	if err := c.Assets(context.Background(), "r", func(Asset) error { return nil }); !errors.Is(err, ErrAuth) {
		t.Fatalf("403: %v", err)
	}
	if _, err := c.NpmPackument(context.Background(), "r", "x"); !errors.Is(err, ErrNotFound) || errors.Is(err, ErrAuth) {
		t.Fatalf("404: %v", err)
	}
	srv.Close()
	_, err = c.Repositories(context.Background())
	if !errors.Is(err, ErrNetwork) || errors.Is(err, ErrAuth) || errors.Is(err, ErrNotFound) {
		t.Fatalf("network: %v", err)
	}
}

func TestStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }))
	defer srv.Close()
	_, err := newClient(t, srv.URL).Repositories(context.Background())
	if !errors.Is(err, ErrStatus) || !strings.Contains(err.Error(), "502") {
		t.Fatalf("%v", err)
	}
}

func TestCredentialsNeverInErrors(t *testing.T) {
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/service/rest/v1/repositories":
			w.WriteHeader(401)
			fmt.Fprint(w, "echo "+r.Header.Get("Authorization"))
		case "/service/rest/v1/repositorySettings":
			fmt.Fprint(w, "<html>not json "+tPass+"</html>")
		case "/service/rest/v1/assets":
			http.Redirect(w, r, "http://other.invalid/x?token="+tPass, http.StatusFound)
		default:
			w.WriteHeader(500)
		}
	}))
	base = srv.URL
	c := newClient(t, base)
	var errs []error
	_, e := c.Repositories(context.Background())
	errs = append(errs, e)
	errs = append(errs, c.getJSON(context.Background(), c.apiURL("repositorySettings", nil), 1<<20, &struct{}{}))
	errs = append(errs, c.Assets(context.Background(), "r", func(Asset) error { return nil }))
	_, e = c.Download(context.Background(), Asset{Path: "p", Repository: "r"})
	errs = append(errs, e)
	srv.Close()
	_, e = c.Repositories(context.Background())
	errs = append(errs, e)
	for i, e := range errs {
		if e == nil {
			t.Errorf("err %d is nil", i)
			continue
		}
		s := e.Error()
		for _, secret := range []string{tPass, "s3cr3t", tUser, "Basic ", "c2VjcjN0"} {
			if strings.Contains(s, secret) {
				t.Errorf("err %d leaks %q: %s", i, secret, s)
			}
		}
	}
}

func TestCrossHostRedirectRefused(t *testing.T) {
	var otherHits int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits++
		fmt.Fprint(w, `[]`)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/service/rest/v1/repositories", http.StatusFound)
	}))
	defer srv.Close()
	_, err := newClient(t, srv.URL).Repositories(context.Background())
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("err = %v", err)
	}
	if otherHits != 0 {
		t.Fatal("the other host was contacted")
	}
}

func TestSameHostRedirectFollowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/service/rest/v1/repositories" {
			http.Redirect(w, r, "/moved", http.StatusFound)
			return
		}
		if !authed(r) {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `[{"name":"ok"}]`)
	}))
	defer srv.Close()
	repos, err := newClient(t, srv.URL).Repositories(context.Background())
	if err != nil || len(repos) != 1 {
		t.Fatalf("%v %v", repos, err)
	}
}

func sums(b []byte) (s1, s256 string) {
	a := sha1.Sum(b) //nolint:gosec
	c := sha256.Sum256(b)
	return hex.EncodeToString(a[:]), hex.EncodeToString(c[:])
}

func TestDownloadChecksum(t *testing.T) {
	body := []byte(strings.Repeat("payload-", 5000))
	s1, s256 := sums(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(401)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	read := func(a Asset) ([]byte, error) {
		rc, err := c.Download(context.Background(), a)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	path := "/repository/r/g/a/1/a-1.jar"

	// sha256 ok (upper-case digest accepted)
	got, err := read(Asset{DownloadURL: srv.URL + path, Checksum: Checksum{SHA256: strings.ToUpper(s256), SHA1: "bad"}})
	if err != nil || string(got) != string(body) {
		t.Fatalf("sha256 ok: %v", err)
	}
	// sha1 used when sha256 absent
	if _, err := read(Asset{DownloadURL: srv.URL + path, Checksum: Checksum{SHA1: s1}}); err != nil {
		t.Fatalf("sha1 ok: %v", err)
	}
	// mismatch on sha256 (sha1 correct is ignored)
	_, err = read(Asset{DownloadURL: srv.URL + path, Checksum: Checksum{SHA256: strings.Repeat("0", 64), SHA1: s1}})
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("sha256 mismatch: %v", err)
	}
	_, err = read(Asset{DownloadURL: srv.URL + path, Checksum: Checksum{SHA1: strings.Repeat("0", 40)}})
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("sha1 mismatch: %v", err)
	}
	// no checksum: not verified
	if _, err := read(Asset{DownloadURL: srv.URL + path}); err != nil {
		t.Fatalf("no checksum: %v", err)
	}
	// mismatch is sticky on further reads
	rc, _ := c.Download(context.Background(), Asset{DownloadURL: srv.URL + path, Checksum: Checksum{SHA256: "00"}})
	io.Copy(io.Discard, rc)
	if _, err := rc.Read(make([]byte, 1)); !errors.Is(err, ErrChecksum) {
		t.Fatalf("sticky: %v", err)
	}
	rc.Close()
}

func TestDownloadUsesSourceHostAndFallbackPath(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		if !authed(r) {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, "x")
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	// downloadUrl naming a foreign host: path is fetched from the source host only.
	rc, err := c.Download(context.Background(), Asset{DownloadURL: "http://nexus-internal.invalid:8081/repository/r/a/b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	// no downloadUrl: built from repository + path, segments escaped.
	rc, err = c.Download(context.Background(), Asset{Repository: "r", Path: "/a b/c#d.txt"})
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if strings.Join(paths, " ") != "/repository/r/a/b.txt /repository/r/a%20b/c%23d.txt" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestDownloadErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(401)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	if _, err := c.Download(context.Background(), Asset{Repository: "r", Path: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := c.Download(context.Background(), Asset{Repository: "r", Path: "x"}); !errors.Is(err, ErrAuth) {
		t.Fatalf("%v", err)
	}
}

func TestOversizedResponsesCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// chunked, no Content-Length: the reader cap must still hold
		fl := w.(http.Flusher)
		w.Write([]byte(`{"items":[],"pad":"`))
		fl.Flush()
		chunk := []byte(strings.Repeat("a", 1<<20))
		for i := 0; i < 20; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		w.Write([]byte(`"}`))
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	if err := c.Assets(context.Background(), "r", func(Asset) error { return nil }); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("assets: %v", err)
	}
	if _, err := c.Repositories(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("repos: %v", err)
	}
	if _, err := c.CargoIndexLine(context.Background(), "r", "serde", "1.0.0"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("index: %v", err)
	}
	if b, err := c.getBounded(context.Background(), "t", srv.URL, "", 1024); !errors.Is(err, ErrTooLarge) || b != nil {
		t.Fatalf("bounded: %v", err)
	}
}

func TestOversizedByContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "99999999999")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if _, err := newClient(t, srv.URL).Repositories(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("%v", err)
	}
}

func TestCargoIndexPath(t *testing.T) {
	for in, want := range map[string]string{
		"a": "1/a", "ab": "2/ab", "abc": "3/a/abc", "abcd": "ab/cd/abcd",
		"serde": "se/rd/serde", "Serde_JSON": "se/rd/serde_json", "": "",
	} {
		if got := CargoIndexPath(in); got != want {
			t.Errorf("CargoIndexPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCargoIndexLine(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if !authed(r) {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"name":"my-crate","vers":"0.1.0","deps":[],"cksum":"AABB","features":{},"yanked":false}`+"\n"+
			`{"name":"my-crate","vers":"0.2.0","deps":[],"cksum":"ccdd","features":{},"yanked":true}`+"\n")
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	l, err := c.CargoIndexLine(context.Background(), "cargo-hosted", "my-crate", "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repository/cargo-hosted/index/my/-c/my-crate" {
		t.Fatalf("path %q", gotPath)
	}
	if l.Vers != "0.2.0" || l.Cksum != "ccdd" || !l.Yanked || !strings.Contains(string(l.Raw), `"yanked":true`) || strings.Contains(string(l.Raw), "\n") {
		t.Fatalf("%+v %s", l, l.Raw)
	}
	l, err = c.CargoIndexLine(context.Background(), "cargo-hosted", "my-crate", "0.1.0")
	if err != nil || l.Cksum != "aabb" || l.Yanked {
		t.Fatalf("%+v %v", l, err)
	}
	if _, err := c.CargoIndexLine(context.Background(), "cargo-hosted", "my-crate", "9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version: %v", err)
	}
	if _, err := c.CargoIndexLine(context.Background(), "r", "../x", "1"); err == nil {
		t.Fatal("bad name accepted")
	}
}

func TestNpmPackument(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		fmt.Fprint(w, `{"name":"@s/pkg","dist-tags":{"latest":"1.1.0","beta":"2.0.0-beta.1"},
		 "versions":{
		  "1.0.0":{"name":"@s/pkg","deprecated":"use 1.1.0","dist":{"tarball":"x"},"readme":"big"},
		  "1.1.0":{"name":"@s/pkg"},
		  "2.0.0-beta.1":{"deprecated":false},
		  "0.9.0":{"deprecated":""}},
		 "time":{"created":"x"}}`)
	}))
	defer srv.Close()
	p, err := newClient(t, srv.URL).NpmPackument(context.Background(), "npm-hosted", "@s/pkg")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repository/npm-hosted/@s%2Fpkg" && gotPath != "/repository/npm-hosted/%40s%2Fpkg" {
		t.Fatalf("path %q", gotPath)
	}
	if p.DistTags["latest"] != "1.1.0" || p.DistTags["beta"] != "2.0.0-beta.1" || len(p.DistTags) != 2 {
		t.Fatalf("tags %v", p.DistTags)
	}
	if len(p.Deprecated) != 1 || p.Deprecated["1.0.0"] != "use 1.1.0" {
		t.Fatalf("deprecated %v", p.Deprecated)
	}
}

func TestNpmPackumentBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>") }))
	defer srv.Close()
	if _, err := newClient(t, srv.URL).NpmPackument(context.Background(), "r", "x"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("%v", err)
	}
}

func TestInsecureSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[]`) }))
	defer srv.Close()
	if _, err := newClient(t, srv.URL).Repositories(context.Background()); !errors.Is(err, ErrNetwork) {
		t.Fatalf("verification should fail by default: %v", err)
	}
	c := newClient(t, srv.URL, func(o *Options) { o.InsecureSkipVerify = true })
	if _, err := c.Repositories(context.Background()); err != nil {
		t.Fatalf("opt-in: %v", err)
	}
}

func TestContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newClient(t, srv.URL).Repositories(ctx); err == nil {
		t.Fatal("expected error")
	}
}

// A downloadUrl on another host is rebuilt on the configured base, keeping
// its context path.
func TestDownloadForeignHostKeepsBasePath(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		fmt.Fprint(w, "x")
	}))
	defer srv.Close()
	c := newClient(t, srv.URL+"/nexus")
	rc, err := c.Download(context.Background(), Asset{Repository: "r", Path: "a/b.txt", DownloadURL: "http://other.invalid/repository/r/a/b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if strings.Join(paths, " ") != "/nexus/repository/r/a/b.txt" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestDownloadCapped(t *testing.T) {
	body := strings.Repeat("x", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("chunked") != "" {
			w.(http.Flusher).Flush() // no Content-Length
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	if _, err := c.DownloadCapped(context.Background(), Asset{DownloadURL: srv.URL + "/a"}, 99); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("content-length: %v", err)
	}
	rc, err := c.DownloadCapped(context.Background(), Asset{DownloadURL: srv.URL + "/a?chunked=1"}, 99)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(rc)
	rc.Close()
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("streamed: %v", err)
	}
	rc, err = c.DownloadCapped(context.Background(), Asset{DownloadURL: srv.URL + "/a?chunked=1"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(rc); err != nil || len(b) != 100 {
		t.Fatalf("at the cap: %d %v", len(b), err)
	}
	rc.Close()
}
