package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	b, err := blob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Store{Blobs: map[string]*blob.Store{DefaultStore: b}, DB: db, Now: time.Now}
}

func TestBuild(t *testing.T) {
	cfg := &config.Config{Repositories: []config.Repository{
		{Name: "p", Format: "maven", Type: "proxy", Remote: "https://x.example/maven2"},
		{Name: "h", Format: "maven", Type: "hosted"},
		{Name: "g", Format: "maven", Type: "group", Members: []string{"h", "p"}, WriteMember: "h"},
	}}
	m := Build(cfg)
	if m["p"].RemoteURL.String() != "https://x.example/maven2/" {
		t.Fatalf("remote = %s", m["p"].RemoteURL)
	}
	g := m["g"]
	if len(g.MemberRepos) != 2 || g.MemberRepos[0] != m["h"] || g.MemberRepos[1] != m["p"] || g.WriteTarget != m["h"] {
		t.Fatalf("group = %+v", g)
	}
}

func TestStorePutGetDelete(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a, err := s.Put(ctx, "r", "x/y.txt", "text/plain", strings.NewReader("hello"), func(a *meta.Asset) { a.RemoteETag = "e" })
	if err != nil {
		t.Fatal(err)
	}
	if a.Size != 5 || a.SHA1 != "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d" || a.RemoteETag != "e" {
		t.Fatalf("asset = %+v", a)
	}
	got, err := s.Get(ctx, "r", "x/y.txt")
	if err != nil || got.BlobSHA256 != a.BlobSHA256 {
		t.Fatalf("get: %+v %v", got, err)
	}
	if err := s.Delete(ctx, "r", "x/y.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "r", "x/y.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := s.Delete(ctx, "r", "x/y.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestServeAsset(t *testing.T) {
	s := newStore(t)
	a, _ := s.Put(context.Background(), "r", "f.jar", "application/java-archive", strings.NewReader("hello"))

	w := httptest.NewRecorder()
	ServeAsset(w, httptest.NewRequest("GET", "/", nil), s, a)
	if w.Code != 200 || w.Body.String() != "hello" || w.Header().Get("Content-Type") != "application/java-archive" {
		t.Fatalf("GET: %d %q %v", w.Code, w.Body, w.Header())
	}
	if w.Header().Get("ETag") != `"{SHA1{aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d}}"` {
		t.Fatalf("etag = %s", w.Header().Get("ETag"))
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	w = httptest.NewRecorder()
	ServeAsset(w, r, s, a)
	if w.Code != http.StatusNotModified {
		t.Fatalf("conditional: %d", w.Code)
	}

	w = httptest.NewRecorder()
	ServeAsset(w, httptest.NewRequest("HEAD", "/", nil), s, a)
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "5" {
		t.Fatalf("HEAD: %d %d %v", w.Code, w.Body.Len(), w.Header())
	}
}

func TestServeAssetCountsOnlyServedGets(t *testing.T) {
	s := newStore(t)
	var got []string
	s.Downloads = func(repo, path string) { got = append(got, repo+"/"+path) }
	a, _ := s.Put(context.Background(), "r", "f.jar", "application/java-archive", strings.NewReader("hello"))

	do := func(method string, hdr map[string]string) int {
		r := httptest.NewRequest(method, "/", nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		ServeAsset(w, r, s, a)
		return w.Code
	}
	if c := do("GET", nil); c != 200 || len(got) != 1 || got[0] != "r/f.jar" {
		t.Fatalf("GET: %d %v", c, got)
	}
	if c := do("GET", map[string]string{"Range": "bytes=0-1"}); c != 206 || len(got) != 2 {
		t.Fatalf("range: %d %v", c, got)
	}
	if c := do("HEAD", nil); c != 200 || len(got) != 2 {
		t.Fatalf("HEAD counted: %d %v", c, got)
	}
	if c := do("GET", map[string]string{"If-None-Match": `"{SHA1{` + a.SHA1 + `}}"`}); c != 304 || len(got) != 2 {
		t.Fatalf("304 counted: %d %v", c, got)
	}
	if c := do("GET", map[string]string{"Range": "bytes=50-60"}); c != 416 || len(got) != 2 {
		t.Fatalf("416 counted: %d %v", c, got)
	}
	s.Downloads = nil
	if c := do("GET", nil); c != 200 { // a nil hook is fine
		t.Fatal(c)
	}
}

func TestStatusOf(t *testing.T) {
	cases := map[error]int{
		Errorf(400, "bad"):                    400,
		ErrNotFound:                           404,
		meta.ErrNotFound:                      404,
		blob.ErrNotFound:                      404,
		fmt.Errorf("x: %w", blob.ErrDiskFull): 507,
		errors.New("boom"):                    500,
		// QA N-8: a request body cut by IdleBody's deadline
		&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}: 408,
	}
	for err, want := range cases {
		if got := StatusOf(err); got != want {
			t.Errorf("StatusOf(%v) = %d, want %d", err, got, want)
		}
	}
	w := httptest.NewRecorder()
	WriteError(w, httptest.NewRequest("GET", "/", nil), errors.New("secret detail"))
	b, _ := io.ReadAll(w.Body)
	if w.Code != 500 || strings.Contains(string(b), "secret") {
		t.Fatalf("500 must hide details: %d %q", w.Code, b)
	}
}

func TestKeyedMutex(t *testing.T) {
	var k KeyedMutex
	var inside, max int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := k.Lock("a")
			n := atomic.AddInt32(&inside, 1)
			for {
				m := atomic.LoadInt32(&max)
				if n <= m || atomic.CompareAndSwapInt32(&max, m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&inside, -1)
			unlock()
		}()
	}
	wg.Wait()
	if max != 1 {
		t.Fatalf("max concurrent holders = %d", max)
	}
	if len(k.m) != 0 {
		t.Fatalf("entries leaked: %d", len(k.m))
	}
}

func TestWriteBlobVerifyAndNewAsset(t *testing.T) {
	s := newStore(t)
	reject := errors.New("bad checksum")
	if _, err := s.WriteBlob("r", strings.NewReader("hello"), func(sums blob.Sums) error {
		if sums.SHA1 != "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d" {
			t.Errorf("sums passed to verify: %+v", sums)
		}
		return reject
	}); !errors.Is(err, reject) {
		t.Fatalf("err = %v", err)
	}
	sums, err := s.WriteBlob("r", strings.NewReader("hello"), nil)
	if err != nil {
		t.Fatal(err)
	}
	a := s.NewAsset("r", "p", "text/plain", sums)
	if a.Repo != "r" || a.Path != "p" || a.Size != 5 || a.SHA512 != sums.SHA512 || a.CreatedAt.IsZero() || a.CheckedAt.IsZero() {
		t.Fatalf("asset = %+v", a)
	}
}

func TestWriteTarget(t *testing.T) {
	m := Build(&config.Config{Repositories: []config.Repository{
		{Name: "h", Format: "maven", Type: "hosted"},
		{Name: "p", Format: "maven", Type: "proxy", Remote: "https://x.example/"},
		{Name: "g", Format: "maven", Type: "group", Members: []string{"h"}, WriteMember: "h"},
		{Name: "ro", Format: "maven", Type: "group", Members: []string{"h"}},
	}})
	if got, err := WriteTarget(m["g"]); err != nil || got != m["h"] {
		t.Fatalf("group: %v %v", got, err)
	}
	if got, err := WriteTarget(m["h"]); err != nil || got != m["h"] {
		t.Fatalf("hosted: %v %v", got, err)
	}
	for _, name := range []string{"p", "ro"} {
		if _, err := WriteTarget(m[name]); StatusOf(err) != 405 {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestErrorMessageHidesInternalErrors(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if s, msg := ErrorMessage(r, Errorf(400, "bad thing")); s != 400 || msg != "bad thing" {
		t.Fatalf("%d %q", s, msg)
	}
	if s, msg := ErrorMessage(r, errors.New("secret")); s != 500 || strings.Contains(msg, "secret") {
		t.Fatalf("%d %q", s, msg)
	}
}

func TestOnlineMembersAndWriteTarget(t *testing.T) {
	off := false
	m := Build(&config.Config{Repositories: []config.Repository{
		{Name: "h", Format: "maven", Type: "hosted"},
		{Name: "p", Format: "maven", Type: "proxy", Remote: "https://x.example/maven2"},
		{Name: "g", Format: "maven", Type: "group", Members: []string{"h", "p"}, WriteMember: "h"},
	}})
	g := m["g"]
	if len(g.OnlineMembers()) != 2 {
		t.Fatalf("all online: %d", len(g.OnlineMembers()))
	}
	m["p"].Online = &off
	if om := g.OnlineMembers(); len(om) != 1 || om[0].Name != "h" || len(g.MemberRepos) != 2 {
		t.Fatalf("online=%v members=%d", om, len(g.MemberRepos))
	}
	if tg, err := WriteTarget(g); err != nil || tg.Name != "h" {
		t.Fatalf("write target: %v %v", tg, err)
	}
	m["h"].Online = &off
	if _, err := WriteTarget(g); StatusOf(err) != http.StatusServiceUnavailable {
		t.Fatalf("offline write member: %v", err)
	}
}

// QA N-27: a group serves nothing when each member is offline or itself such
// a group; a group without members or a hosted repository is not offline.
func TestMembersOffline(t *testing.T) {
	off := false
	m := Build(&config.Config{Repositories: []config.Repository{
		{Name: "h", Format: "maven", Type: "hosted", Online: &off},
		{Name: "h2", Format: "maven", Type: "hosted"},
		{Name: "inner", Format: "maven", Type: "group", Members: []string{"h"}},
		{Name: "outer", Format: "maven", Type: "group", Members: []string{"h", "inner"}},
		{Name: "empty", Format: "maven", Type: "group"},
	}})
	for name, want := range map[string]bool{"inner": true, "outer": true, "empty": false, "h": false, "h2": false} {
		if got := m[name].MembersOffline(); got != want {
			t.Errorf("%s: MembersOffline = %v, want %v", name, got, want)
		}
	}
	m["inner"].MemberRepos = append(m["inner"].MemberRepos, m["h2"])
	if m["outer"].MembersOffline() {
		t.Error("outer: a nested group with an online member serves")
	}
}
