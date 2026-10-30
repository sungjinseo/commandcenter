package blob

import (
	"bytes"
	"crypto"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testRSAKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// testAccountKey is a service account key file for the token endpoint at
// tokenURI, its key in PKCS#8 or PKCS#1 PEM.
func testAccountKey(t *testing.T, tokenURI string, pkcs1 bool) string {
	t.Helper()
	k := testRSAKey()
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}
	if !pkcs1 {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	}
	b, _ := json.Marshal(map[string]string{"type": "service_account", "project_id": "p", "private_key_id": "kid1",
		"private_key": string(pem.EncodeToMemory(block)), "client_email": "cc@p.iam.gserviceaccount.com", "token_uri": tokenURI})
	return string(b)
}

// tokenServer is an OAuth2 token endpoint that checks each JWT assertion
// (RS256 over the test key, claims for the account) and answers token-<n>.
type tokenServer struct {
	*httptest.Server
	t     *testing.T
	n     atomic.Int32
	fail  atomic.Bool
	now   func() time.Time
	ttl   int
	error atomic.Value // the first problem found, a string
}

func newTokenServer(t *testing.T, now func() time.Time) *tokenServer {
	ts := &tokenServer{t: t, now: now, ttl: 3600}
	ts.Server = httptest.NewServer(http.HandlerFunc(ts.serve))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *tokenServer) bad(format string, args ...any) {
	ts.error.CompareAndSwap(nil, fmt.Sprintf(format, args...))
}

func (ts *tokenServer) serve(w http.ResponseWriter, r *http.Request) {
	if ts.fail.Load() {
		http.Error(w, `{"error":"backend_error"}`, http.StatusInternalServerError)
		return
	}
	if r.Method != http.MethodPost || r.FormValue("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		ts.bad("request %s grant_type %q", r.Method, r.FormValue("grant_type"))
	}
	parts := strings.Split(r.FormValue("assertion"), ".")
	if len(parts) != 3 {
		ts.bad("assertion has %d parts", len(parts))
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	var hdr, claims map[string]any
	for i, v := range []*map[string]any{&hdr, &claims} {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil || json.Unmarshal(b, v) != nil {
			ts.bad("part %d does not decode", i)
		}
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testRSAKey().PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		ts.bad("signature: %v", err)
	}
	now := float64(ts.now().Unix())
	if hdr["alg"] != "RS256" || hdr["typ"] != "JWT" || hdr["kid"] != "kid1" {
		ts.bad("header %v", hdr)
	}
	iat, _ := claims["iat"].(float64)
	if claims["iss"] != "cc@p.iam.gserviceaccount.com" || claims["scope"] != gcsScope || claims["aud"] != ts.URL ||
		iat < now-5 || iat > now || claims["exp"] != iat+3600 {
		ts.bad("claims %v (now %.0f, aud %s)", claims, now, ts.URL)
	}
	n := ts.n.Add(1)
	json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("token-%d", n), "expires_in": ts.ttl, "token_type": "Bearer"})
}

func (ts *tokenServer) check() {
	ts.t.Helper()
	if e := ts.error.Load(); e != nil {
		ts.t.Fatal(e)
	}
}

// A key's JWT is signed and claimed as Google's token endpoint wants it, for
// a PKCS#8 and a PKCS#1 key; the token is cached, refreshed 5 minutes before
// it expires, and kept while a refresh fails until it expires.
func TestGCSServiceAccountToken(t *testing.T) {
	for _, pkcs1 := range []bool{false, true} {
		clock := time.Unix(1_800_000_000, 0)
		now := func() time.Time { return clock }
		ts := newTokenServer(t, now)
		sa, err := parseServiceAccount([]byte(testAccountKey(t, ts.URL, pkcs1)))
		if err != nil {
			t.Fatal(err)
		}
		a := newGCSAuth(sa.fetcher(ts.Client(), now))
		a.now = now
		want := func(tok string, fetches int32) {
			t.Helper()
			got, err := a.token()
			ts.check()
			if err != nil || got != tok || ts.n.Load() != fetches {
				t.Fatalf("pkcs1 %v at +%v: token %q, %v after %d fetches; want %q after %d", pkcs1, clock.Sub(time.Unix(1_800_000_000, 0)),
					got, err, ts.n.Load(), tok, fetches)
			}
		}
		want("token-1", 1)
		want("token-1", 1)
		clock = clock.Add(54 * time.Minute)
		want("token-1", 1)
		clock = clock.Add(2 * time.Minute) // 56 min: within 5 min of expiry
		want("token-2", 2)
		ts.fail.Store(true)
		clock = clock.Add(57 * time.Minute) // past the refresh time, before the expiry
		want("token-2", 2)
		clock = clock.Add(4 * time.Minute) // expired
		if tok, err := a.token(); err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("expired token with the endpoint failing: %q, %v", tok, err)
		}
	}
}

func TestGCSConfigValidate(t *testing.T) {
	key := testAccountKey(t, "https://oauth2.example/token", false)
	ok := GCSConfig{Bucket: "my-bucket", Prefix: "/cc/blobs/", AccountKey: key}
	if err := ok.Validate(); err != nil || ok.Prefix != "cc/blobs" || ok.Authentication != GCSAuthKey {
		t.Fatalf("valid: %v, %+v", err, ok)
	}
	if adc := (GCSConfig{Bucket: "a.b.c"}); adc.Validate() != nil || adc.Authentication != GCSAuthDefault {
		t.Fatalf("application default: %+v", adc)
	}
	pkcs1 := strings.Replace(key, "PRIVATE KEY", "RSA PRIVATE KEY", -1)
	for name, c := range map[string]struct {
		cfg  GCSConfig
		want string
	}{
		"no bucket":        {GCSConfig{}, "bucket"},
		"upper bucket":     {GCSConfig{Bucket: "Bucket"}, "bucket"},
		"long part":        {GCSConfig{Bucket: strings.Repeat("a", 64)}, "bucket"},
		"bad prefix":       {GCSConfig{Bucket: "bkt", Prefix: "a b"}, "prefix"},
		"bad auth":         {GCSConfig{Bucket: "bkt", Authentication: "hmac"}, "authentication must be"},
		"key without key":  {GCSConfig{Bucket: "bkt", Authentication: GCSAuthKey}, "needs accountKey"},
		"key with default": {GCSConfig{Bucket: "bkt", Authentication: GCSAuthDefault, AccountKey: key}, "goes with"},
		"none":             {GCSConfig{Bucket: "bkt", Authentication: GCSAuthNone}, "emulator"},
		"bad endpoint":     {GCSConfig{Bucket: "bkt", Endpoint: "storage:443"}, "endpoint"},
		"not json":         {GCSConfig{Bucket: "bkt", AccountKey: "-----BEGIN PRIVATE KEY-----"}, "JSON"},
		"not sa":           {GCSConfig{Bucket: "bkt", AccountKey: strings.Replace(key, "service_account", "authorized_user", 1)}, "service_account"},
		"pkcs8 as pkcs1":   {GCSConfig{Bucket: "bkt", AccountKey: pkcs1}, "does not parse"},
		"bad token_uri":    {GCSConfig{Bucket: "bkt", AccountKey: strings.Replace(key, "https://oauth2.example/token", "file:///x", 1)}, "token_uri"},
	} {
		err := c.cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "MII") {
			t.Errorf("%s: %v, want ~%q", name, err, c.want)
		}
	}
	// One namespace however it is reached: the default endpoint spelled out,
	// or an S3 store on the bucket through GCS's S3 API.
	g := GCSConfig{Bucket: "bkt", Prefix: "cc"}
	for _, loc := range []string{(GCSConfig{Bucket: "bkt", Prefix: "/cc/", Endpoint: "HTTPS://storage.googleapis.com:443/"}).Location(),
		(S3Config{Bucket: "bkt", Prefix: "cc", Endpoint: "https://storage.googleapis.com", PathStyle: true}).Location()} {
		if loc != g.Location() {
			t.Errorf("location %s, want %s", loc, g.Location())
		}
	}
}

// Application default credentials: the metadata server (with
// Metadata-Flavor) unless GOOGLE_APPLICATION_CREDENTIALS
// names a key file.
func TestGCSDefaultCredentials(t *testing.T) {
	var calls atomic.Int32
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != metadataTokenPath || r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing Metadata-Flavor", http.StatusForbidden)
			return
		}
		calls.Add(1)
		io.WriteString(w, `{"access_token":"md-token","expires_in":120,"token_type":"Bearer"}`)
	}))
	defer md.Close()
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(md.URL, "http://"))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	fetch, err := defaultCredentials(http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	a := newGCSAuth(fetch)
	for range 2 {
		if tok, err := a.token(); err != nil || tok != "md-token" {
			t.Fatalf("metadata token %q, %v", tok, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d metadata calls, want 1 (cached)", calls.Load())
	}
	if a.refresh.Sub(a.exp) != -30*time.Second { // a quarter of 120 s
		t.Fatalf("refresh %v before expiry", a.exp.Sub(a.refresh))
	}

	ts := newTokenServer(t, time.Now)
	path := filepath.Join(t.TempDir(), "key.json")
	os.WriteFile(path, []byte(testAccountKey(t, ts.URL, false)), 0o600)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	if fetch, err = defaultCredentials(ts.Client()); err != nil {
		t.Fatal(err)
	}
	if tok, _, err := fetch(); err != nil || tok != "token-1" || calls.Load() != 1 {
		t.Fatalf("key file token %q, %v", tok, err)
	}
	ts.check()
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
	if _, err := defaultCredentials(ts.Client()); err == nil || !strings.Contains(err.Error(), "GOOGLE_APPLICATION_CREDENTIALS") {
		t.Fatalf("missing key file: %v", err)
	}
}

// fakeGCS is enough of the JSON API for the paths fake-gcs-server cannot
// show: list pages, retries, the 403 bucket fallback and a resumable upload
// that loses part of a chunk.
type fakeGCS struct {
	*httptest.Server
	mu        sync.Mutex
	objects   map[string][]byte
	auth      []string // Authorization of each request
	bucketErr []int    // statuses the bucket GET answers first
	chunkErr  int      // the resumable chunk that fails after half of it is stored
	chunkDown bool     // every chunk fails, nothing stored
	chunks    int
	session   []byte
	pageSize  int
}

func newFakeGCS(t *testing.T) *fakeGCS {
	f := &fakeGCS{objects: map[string][]byte{}, pageSize: 2, chunkErr: -1}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGCS) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	path := r.URL.EscapedPath()
	objJSON := func(name string) {
		sum := md5.Sum(f.objects[name])
		json.NewEncoder(w).Encode(map[string]any{"name": name, "size": strconv.Itoa(len(f.objects[name])),
			"md5Hash": base64.StdEncoding.EncodeToString(sum[:]), "updated": time.Now().UTC().Format(time.RFC3339Nano)})
	}
	switch {
	case r.Method == http.MethodGet && path == "/storage/v1/b/bkt":
		if len(f.bucketErr) > 0 {
			code := f.bucketErr[0]
			f.bucketErr = f.bucketErr[1:]
			http.Error(w, `{"error":{"code":`+strconv.Itoa(code)+`}}`, code)
			return
		}
		io.WriteString(w, `{"name":"bkt"}`)
	case r.Method == http.MethodGet && path == "/storage/v1/b/bkt/o":
		var names []string
		for n := range f.objects {
			if strings.HasPrefix(n, r.FormValue("prefix")) {
				names = append(names, n)
			}
		}
		slices.Sort(names)
		start, _ := strconv.Atoi(r.FormValue("pageToken"))
		max, _ := strconv.Atoi(r.FormValue("maxResults"))
		end := min(start+min(max, f.pageSize), len(names))
		items := []map[string]any{}
		for _, n := range names[start:end] {
			items = append(items, map[string]any{"name": n, "size": strconv.Itoa(len(f.objects[n])), "updated": "2026-10-30T00:00:00Z"})
		}
		page := map[string]any{"items": items}
		if end < len(names) {
			page["nextPageToken"] = strconv.Itoa(end)
		}
		json.NewEncoder(w).Encode(page)
	case r.Method == http.MethodPost && path == "/upload/storage/v1/b/bkt/o" && r.FormValue("uploadType") == "resumable":
		var meta struct{ Name string }
		json.NewDecoder(r.Body).Decode(&meta)
		f.session = nil
		// Another host than the endpoint: the client must keep to its own.
		w.Header().Set("Location", "http://elsewhere.invalid/upload/storage/v1/b/bkt/o?uploadType=resumable&name="+meta.Name+"&upload_id=u1")
	case r.Method == http.MethodPut && path == "/upload/storage/v1/b/bkt/o" && r.FormValue("upload_id") == "u1":
		name := r.FormValue("name")
		rng := r.Header.Get("Content-Range") // bytes a-b/total or bytes */total
		spec, total, _ := strings.Cut(strings.TrimPrefix(rng, "bytes "), "/")
		size, _ := strconv.Atoi(total)
		if spec != "*" {
			from, _, _ := strings.Cut(spec, "-")
			if off, _ := strconv.Atoi(from); off != len(f.session) {
				http.Error(w, fmt.Sprintf("chunk at %d, have %d", off, len(f.session)), http.StatusBadRequest)
				return
			}
			b, _ := io.ReadAll(r.Body)
			f.chunks++
			if f.chunkDown {
				http.Error(w, "backend error", http.StatusServiceUnavailable)
				return
			}
			if f.chunks-1 == f.chunkErr { // half of it arrived, then the server failed
				f.session = append(f.session, b[:len(b)/2]...)
				http.Error(w, "backend error", http.StatusServiceUnavailable)
				return
			}
			f.session = append(f.session, b...)
		}
		if len(f.session) == size {
			f.objects[name] = f.session
			objJSON(name)
			return
		}
		if len(f.session) > 0 {
			w.Header().Set("Range", "bytes=0-"+strconv.Itoa(len(f.session)-1))
		}
		w.WriteHeader(http.StatusPermanentRedirect)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusNotImplemented)
	}
}

func fastRetries(t *testing.T) {
	old := gcsBackoff
	gcsBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { gcsBackoff = old })
}

func openFake(t *testing.T, f *fakeGCS) (*Store, *gcs) {
	t.Helper()
	s, err := OpenGCS(t.TempDir(), GCSConfig{Bucket: "bkt", Prefix: "cc", Endpoint: f.URL, Authentication: GCSAuthNone})
	if err != nil {
		t.Fatal(err)
	}
	g := s.be.(*gcs)
	g.auth = newGCSAuth(func() (string, time.Duration, error) { return "tok", time.Hour, nil })
	return s, g
}

// Retries: a bucket answering 503 twice is reached on the third try; one
// answering 403 (no storage.buckets.get) passes the check when its objects
// list; a 404 is a missing bucket, at once. Every request carries the token;
// a token endpoint refusing the key is not asked again.
func TestGCSCheckRetries(t *testing.T) {
	fastRetries(t)
	f := newFakeGCS(t)
	s, g := openFake(t, f)
	f.bucketErr = []int{503, 429}
	if err := s.Check(); err != nil || len(f.auth) != 3 {
		t.Fatalf("check after 503, 429: %v after %d requests", err, len(f.auth))
	}
	f.bucketErr = []int{403}
	if err := s.Check(); err != nil || len(f.auth) != 5 {
		t.Fatalf("check after 403: %v after %d requests", err, len(f.auth))
	}
	f.bucketErr = []int{404}
	if err := s.Check(); err == nil || !strings.Contains(err.Error(), "not found") || len(f.auth) != 6 {
		t.Fatalf("check after 404: %v after %d requests", err, len(f.auth))
	}
	f.bucketErr = []int{500, 500, 500, 500, 500}
	if err := s.Check(); err == nil || len(f.auth) != 11 {
		t.Fatalf("check after 5 x 500: %v after %d requests", err, len(f.auth))
	}
	for _, a := range f.auth {
		if a != "Bearer tok" {
			t.Fatalf("Authorization %q", a)
		}
	}
	fetches := 0
	g.auth = newGCSAuth(func() (string, time.Duration, error) {
		fetches++
		return "", 0, permanent{errors.New("400 invalid_grant")}
	})
	if err := s.Check(); err == nil || !strings.Contains(err.Error(), "invalid_grant") || fetches != 1 || len(f.auth) != 11 {
		t.Fatalf("refused key: %v after %d fetches", err, fetches)
	}
}

// Walk follows nextPageToken and skips names that are not blobs.
func TestGCSWalkPages(t *testing.T) {
	f := newFakeGCS(t)
	s, _ := openFake(t, f)
	var want []string
	for i := range 5 {
		sha := fmt.Sprintf("%064x", i)
		f.objects["cc/"+sha] = []byte("x")
		want = append(want, sha)
	}
	f.objects["cc/not-a-blob"] = []byte("x")
	var got []string
	if err := s.Walk(func(sha string, _ time.Time) error { got = append(got, sha); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("walk %v, want %v", got, want)
	}
}

// A chunk that fails after the server stored half of it: the client asks
// the session where it is and goes on from there, on its own endpoint.
func TestGCSResumableRecovers(t *testing.T) {
	fastRetries(t)
	old := gcsChunkSize
	gcsChunkSize = 1000
	t.Cleanup(func() { gcsChunkSize = old })
	f := newFakeGCS(t)
	s, _ := openFake(t, f)
	f.chunkErr = 1
	data := make([]byte, 3500)
	rand.Read(data)
	sums, err := s.Write(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.objects["cc/"+sums.SHA256]; !bytes.Equal(got, data) {
		t.Fatalf("stored %d bytes, want %d", len(got), len(data))
	}
	// 0-999, 1000-1999 (half stored, 503), a status query, 1500-2499, 2500-3499.
	if f.chunks != 4 {
		t.Fatalf("%d chunk requests, want 4", f.chunks)
	}
	// A session that never gets further gives up after the retries.
	f.chunkDown, f.chunks = true, 0
	if _, err := s.Write(bytes.NewReader(data[1:])); err == nil || !strings.Contains(err.Error(), "503") || f.chunks != gcsRetries+1 {
		t.Fatalf("chunks always failing: %v after %d chunk requests", err, f.chunks)
	}
}

// fakeGCSURL is the fake-gcs-server for the integration tests, or a skip.
func fakeGCSURL(t *testing.T) string {
	u := os.Getenv("COMMANDCENTER_TEST_FAKEGCS_URL")
	if u == "" {
		t.Skip("COMMANDCENTER_TEST_FAKEGCS_URL is not set (a fake-gcs-server, e.g. http://fakegcs:4443)")
	}
	return strings.TrimSuffix(u, "/")
}

// NewFakeGCSBucket creates a bucket with a unique name in the
// fake-gcs-server at u.
func newFakeGCSBucket(t *testing.T, u string) string {
	t.Helper()
	b := make([]byte, 6)
	rand.Read(b)
	name := "cc-test-" + hex.EncodeToString(b)
	resp, err := http.Post(u+"/storage/v1/b?project=test", "application/json", strings.NewReader(`{"name":"`+name+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create bucket: %s", resp.Status)
	}
	return name
}

// Against fake-gcs-server: a small and a resumable upload read back whole
// and by range, stat, walk, delete, and the meta objects next to the blobs.
func TestGCSFakeServer(t *testing.T) {
	u := fakeGCSURL(t)
	old := gcsChunkSize
	gcsChunkSize = 256 << 10
	t.Cleanup(func() { gcsChunkSize = old })
	bucket := newFakeGCSBucket(t, u)
	s, err := OpenGCS(t.TempDir(), GCSConfig{Bucket: bucket, Prefix: "/cc/", Endpoint: u, Authentication: GCSAuthNone})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Check(); err != nil {
		t.Fatal(err)
	}
	if h := s.Health(); len(h) != 1 || !h[0].OK || !strings.Contains(h[0].Target, bucket+"/cc") {
		t.Fatalf("health %+v", h)
	}
	small, err := s.Write(strings.NewReader("hello gcs"))
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 700<<10) // three chunks, the last short
	rand.Read(big)
	large, err := s.Write(bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		sums Sums
		data []byte
	}{{small, []byte("hello gcs")}, {large, big}} {
		info, err := s.Stat(c.sums.SHA256)
		if err != nil || info.Size != int64(len(c.data)) || time.Since(info.ModTime) > time.Hour {
			t.Fatalf("stat %+v, %v", info, err)
		}
		f, err := s.Open(c.sums.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(f)
		if err != nil || !bytes.Equal(got, c.data) {
			t.Fatalf("read back %d bytes, %v; want %d", len(got), err, len(c.data))
		}
		// Range reads: a seek, a read across a chunk boundary, ReadAt, the end.
		at := int64(len(c.data)) * 2 / 5
		if _, err := f.Seek(at, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		p := make([]byte, min(1000, len(c.data)-int(at)))
		if _, err := io.ReadFull(f, p); err != nil || !bytes.Equal(p, c.data[at:at+int64(len(p))]) {
			t.Fatalf("read after seek: %v", err)
		}
		if n, err := f.ReadAt(p[:3], int64(len(c.data))-3); n != 3 || err != nil || !bytes.Equal(p[:3], c.data[len(c.data)-3:]) {
			t.Fatalf("ReadAt the last 3 bytes: %d, %v", n, err)
		}
		if n, err := f.ReadAt(p[:4], int64(len(c.data))-3); n != 3 || err != io.EOF {
			t.Fatalf("ReadAt past the end: %d, %v", n, err)
		}
		if end, _ := f.Seek(0, io.SeekEnd); end != int64(len(c.data)) {
			t.Fatalf("seek end %d", end)
		}
		if n, err := f.Read(p); n != 0 || err != io.EOF {
			t.Fatalf("read at the end: %d, %v", n, err)
		}
		f.Close()
	}
	// The same content again replaces the object (a fresh updated time).
	if again, err := s.Write(strings.NewReader("hello gcs")); err != nil || again != small {
		t.Fatalf("write again: %+v, %v", again, err)
	}

	g := s.be.(*gcs)
	journal := []string{"journal/2026/10/30/09/a.json", "journal/2026/10/30/09/b.json"}
	for i, k := range append(journal, "other/x") {
		if err := g.putMeta(k, []byte(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := g.getMeta(journal[1]); err != nil || string(b) != `{"n":1}` {
		t.Fatalf("getMeta %q, %v", b, err)
	}
	if _, err := g.getMeta("journal/missing.json"); err != ErrNotFound {
		t.Fatalf("getMeta missing: %v", err)
	}
	list := func(prefix string) []string {
		var keys []string
		if err := g.listMeta(prefix, func(k string) error { keys = append(keys, k); return nil }); err != nil {
			t.Fatal(err)
		}
		return keys
	}
	if got := list("journal/"); !slices.Equal(got, journal) {
		t.Fatalf("listMeta journal/: %v", got)
	}
	if got := list(""); len(got) != 3 || got[2] != "other/x" {
		t.Fatalf("listMeta: %v", got)
	}
	for _, bad := range []string{"", "/abs", "a/../b", "a b"} {
		if err := g.putMeta(bad, nil); err == nil {
			t.Errorf("putMeta %q accepted", bad)
		}
	}
	var walked []string
	if err := s.Walk(func(sha string, _ time.Time) error { walked = append(walked, sha); return nil }); err != nil {
		t.Fatal(err)
	}
	slices.Sort(walked)
	want := []string{small.SHA256, large.SHA256}
	slices.Sort(want)
	if !slices.Equal(walked, want) {
		t.Fatalf("walk %v, want the two blobs and no meta object", walked)
	}
	if blobs, n, err := s.Usage(t.Context()); err != nil || blobs != 2 || n != int64(len(big)+9) {
		t.Fatalf("usage %d %d %v", blobs, n, err)
	}
	for range 2 {
		if err := g.removeMeta(journal[0]); err != nil {
			t.Fatal(err)
		}
		if err := s.Remove(small.SHA256); err != nil {
			t.Fatal(err)
		}
	}
	if got := list("journal/"); !slices.Equal(got, journal[1:]) {
		t.Fatalf("listMeta after remove: %v", got)
	}
	if _, err := s.Stat(small.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat removed: %v", err)
	}
	if _, err := s.Open(small.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open removed: %v", err)
	}

	missing, err := OpenGCS(t.TempDir(), GCSConfig{Bucket: "cc-no-such-bucket", Endpoint: u, Authentication: GCSAuthNone})
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Check(); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing bucket: %v", err)
	}
}
