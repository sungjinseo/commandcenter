package blob

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The Azurite development account's published name and key.
const (
	azuriteAccount = "devstoreaccount1"
	azuriteKey     = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
)

// The string Shared Key signs, for requests shaped like the examples of
// "Authorize with Shared Key" (the canonicalized resource of a container
// metadata read, of a listing with a repeated parameter, of a path-style
// emulator URL) and for a PUT with every slot the backend fills.
func TestAzureSharedKeyString(t *testing.T) {
	const date = "Sun, 11 Oct 2009 21:49:13 GMT"
	req := func(method, u string, hdr map[string]string, size int64) *http.Request {
		r, _ := http.NewRequest(method, u, nil)
		r.ContentLength = size
		r.Header.Set("x-ms-date", date)
		r.Header.Set("x-ms-version", "2009-09-19")
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	empty := strings.Repeat("\n", 11) // Content-Encoding through Range, all empty
	for name, c := range map[string]struct {
		r    *http.Request
		want string
	}{
		"container metadata": {req("GET", "http://myaccount.blob.core.windows.net/mycontainer?restype=container&comp=metadata", nil, 0),
			"GET\n" + empty + "x-ms-date:" + date + "\nx-ms-version:2009-09-19\n/myaccount/mycontainer\ncomp:metadata\nrestype:container"},
		"repeated parameter": {req("GET", "http://myaccount.blob.core.windows.net/mycontainer?restype=container&comp=list&include=snapshots&include=metadata&include=uncommittedblobs", nil, 0),
			"GET\n" + empty + "x-ms-date:" + date + "\nx-ms-version:2009-09-19\n/myaccount/mycontainer\ncomp:list\ninclude:metadata,snapshots,uncommittedblobs\nrestype:container"},
		"emulator path style": {req("GET", "http://127.0.0.1:10000/devstoreaccount1/mycontainer/myblob", nil, 0),
			"GET\n" + empty + "x-ms-date:" + date + "\nx-ms-version:2009-09-19\n/devstoreaccount1/devstoreaccount1/mycontainer/myblob"},
		"put block": {req("PUT", "https://myaccount.blob.core.windows.net/mycontainer/p/blob?Comp=block&blockid=MDAwMDAx", map[string]string{
			"Content-MD5": "Q2hlY2sgSW50ZWdyaXR5IQ==", "Content-Type": "application/octet-stream", "Range": "bytes=0-9",
			"x-ms-blob-type": "BlockBlob", "X-Ms-Meta-Note": "  two   words ", "User-Agent": "not signed"}, 11),
			"PUT\n\n\n11\nQ2hlY2sgSW50ZWdyaXR5IQ==\napplication/octet-stream\n\n\n\n\n\nbytes=0-9\n" +
				"x-ms-blob-type:BlockBlob\nx-ms-date:" + date + "\nx-ms-meta-note:two words\nx-ms-version:2009-09-19\n" +
				"/myaccount/mycontainer/p/blob\nblockid:MDAwMDAx\ncomp:block"},
	} {
		account := "myaccount"
		if strings.Contains(c.r.URL.Path, azuriteAccount) {
			account = azuriteAccount
		}
		if got := sharedKeyString(c.r, account); got != c.want {
			t.Errorf("%s:\n%q\nwant\n%q", name, got, c.want)
		}
	}
	// The signature of the first, as openssl computes it:
	// printf '<string>' | openssl dgst -sha256 -mac HMAC -macopt hexkey:<key in hex> -binary | base64
	r := req("GET", "http://myaccount.blob.core.windows.net/mycontainer?restype=container&comp=metadata", nil, 0)
	key, _ := decodeKey(azuriteKey)
	SignSharedKey(r, "myaccount", key)
	if got, want := r.Header.Get("Authorization"), "SharedKey myaccount:cnYDuBhnHA2Nrlccz6P/KAAfs5XsltCrSwpgKV+uaso="; got != want {
		t.Fatalf("Authorization = %s, want %s", got, want)
	}
}

func decodeKey(s string) ([]byte, error) {
	cfg := AzureConfig{AccountName: "a12", ContainerName: "c12", AccountKey: s}
	a, err := newAzure(cfg)
	if err != nil {
		return nil, err
	}
	return a.auth.Load().key, nil
}

func TestAzureConfigValidate(t *testing.T) {
	ok := AzureConfig{AccountName: "acct1", ContainerName: "blobs-1", Prefix: "/cc/", AccountKey: azuriteKey, SASToken: "sig=x", ClientSecret: "s"}
	if err := ok.Validate(); err != nil || ok.Prefix != "cc" || ok.Auth != AzureAccountKey || ok.SASToken != "" || ok.ClientSecret != "" || ok.AccountKey == "" {
		t.Fatalf("valid: %v %+v", err, ok)
	}
	if ok.Location() != "https://acct1.blob.core.windows.net/blobs-1/cc" {
		t.Fatalf("location %s", ok.Location())
	}
	spelled := ok
	spelled.Endpoint = "HTTPS://Acct1.blob.core.windows.net:443/"
	if spelled.Validate() != nil || spelled.Location() != ok.Location() {
		t.Fatalf("one endpoint, two locations: %s %s", spelled.Location(), ok.Location())
	}
	sas := AzureConfig{AccountName: "acct1", ContainerName: "c12", Auth: AzureSASToken, SASToken: "?sv=2022-11-02&sp=rwdl&sig=abc%2B", AccountKey: azuriteKey}
	if err := sas.Validate(); err != nil || sas.SASToken != "sv=2022-11-02&sp=rwdl&sig=abc%2B" || sas.AccountKey != "" {
		t.Fatalf("sas: %v %+v", err, sas)
	}
	for name, c := range map[string]struct {
		mutate func(*AzureConfig)
		want   string
	}{
		"account":         {func(c *AzureConfig) { c.AccountName = "Acct" }, "accountName"},
		"container":       {func(c *AzureConfig) { c.ContainerName = "a--b" }, "containerName"},
		"container end":   {func(c *AzureConfig) { c.ContainerName = "abc-" }, "containerName"},
		"container short": {func(c *AzureConfig) { c.ContainerName = "ab" }, "containerName"},
		"prefix":          {func(c *AzureConfig) { c.Prefix = "a b" }, "prefix"},
		"endpoint":        {func(c *AzureConfig) { c.Endpoint = "azurite:10000" }, "endpoint"},
		"authority":       {func(c *AzureConfig) { c.AuthorityHost = "ftp://x" }, "authorityHost"},
		"method":          {func(c *AzureConfig) { c.Auth = "password" }, "authenticationMethod"},
		"key":             {func(c *AzureConfig) { c.AccountKey = "not base64!" }, "accountKey"},
		"sas":             {func(c *AzureConfig) { c.Auth, c.SASToken = AzureSASToken, "sv=1" }, "sasToken"},
		"client secret":   {func(c *AzureConfig) { c.Auth, c.TenantID, c.ClientID = AzureClientSecret, "t", "c" }, "clientSecret"},
		"tenant":          {func(c *AzureConfig) { c.Auth, c.TenantID = AzureManagedIdentity, "a/b" }, "tenantId"},
	} {
		cfg := AzureConfig{AccountName: "acct1", ContainerName: "c12", AccountKey: azuriteKey}
		c.mutate(&cfg)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want ~%s", name, err, c.want)
		}
	}
}

// fakeAzure answers a container listing at /c12 and tokens at
// /<tenant>/oauth2/v2.0/token; tokenFail makes the token endpoint fail.
type fakeAzure struct {
	*httptest.Server
	tokens, lists atomic.Int32
	tokenFail     atomic.Bool
	lastForm      atomic.Value // url.Values of the last token request
	lastAuth      atomic.Value // the Authorization of the last listing
	lastMeta      atomic.Value // the Metadata header and query of the last metadata service request
	failLists     atomic.Int32 // how many listings answer 503 first
}

func newFakeAzure(t *testing.T) *fakeAzure {
	f := &fakeAzure{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") || strings.HasSuffix(r.URL.Path, "/metadata/identity/oauth2/token"):
			r.ParseForm()
			f.lastForm.Store(r.Form)
			f.lastMeta.Store(r.Header.Get("Metadata") + " " + r.URL.RawQuery)
			n := f.tokens.Add(1)
			if f.tokenFail.Load() {
				http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			if r.Method == http.MethodGet { // the metadata service: expires_in as a string
				fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":"86399","token_type":"Bearer"}`, n)
				return
			}
			fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":3600,"token_type":"Bearer"}`, n)
		case r.URL.Path == "/c12" && r.URL.Query().Get("comp") == "list":
			f.lists.Add(1)
			f.lastAuth.Store(r.Header.Get("Authorization") + "|" + r.URL.RawQuery)
			if f.failLists.Add(-1) >= 0 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?><EnumerationResults><Blobs></Blobs><NextMarker/></EnumerationResults>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAzure) open(t *testing.T, cfg AzureConfig) *azure {
	t.Helper()
	cfg.AccountName, cfg.ContainerName, cfg.Endpoint = "acct1", "c12", f.URL
	a, err := newAzure(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A client-credentials token is fetched once, used until five minutes
// before it expires, refreshed then; a failed refresh keeps the old token
// until it expires, and after that the request fails.
func TestAzureTokenRefresh(t *testing.T) {
	f := newFakeAzure(t)
	a := f.open(t, AzureConfig{Auth: AzureClientSecret, TenantID: "tenant-1", ClientID: "app-1", ClientSecret: "s3cret", AuthorityHost: f.URL})
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	step := func(d time.Duration, wantTokens int32, wantAuth string) {
		t.Helper()
		now = now.Add(d)
		err := a.check()
		if got := f.tokens.Load(); got != wantTokens {
			t.Fatalf("after %v: %d token fetches, want %d (%v)", d, got, wantTokens, err)
		}
		if wantAuth == "" {
			if err == nil {
				t.Fatal("a request with an expired token and no new one succeeded")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if auth, _ := f.lastAuth.Load().(string); !strings.HasPrefix(auth, "Bearer "+wantAuth+"|") {
			t.Fatalf("Authorization %q, want Bearer %s", auth, wantAuth)
		}
	}
	step(0, 1, "tok-1")
	form := f.lastForm.Load().(url.Values)
	if form["grant_type"][0] != "client_credentials" || form["client_id"][0] != "app-1" || form["client_secret"][0] != "s3cret" ||
		form["scope"][0] != "https://storage.azure.com/.default" {
		t.Fatalf("token request %v", form)
	}
	step(54*time.Minute, 1, "tok-1")
	step(2*time.Minute, 2, "tok-2") // 56 minutes: within five minutes of expiry
	f.tokenFail.Store(true)
	step(56*time.Minute, 3, "tok-2") // the refresh fails, tok-2 has 4 minutes left
	step(10*time.Second, 3, "tok-2") // and is not tried again for 30 seconds
	step(30*time.Second, 4, "tok-2")
	step(5*time.Minute, 5, "") // now it has expired
	f.tokenFail.Store(false)
	step(0, 6, "tok-6")
}

// Workload identity sends the projected service account token, read anew
// for every fetch, as the client assertion; the tenant and client come from
// the environment the webhook sets.
func TestAzureWorkloadIdentity(t *testing.T) {
	f := newFakeAzure(t)
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("jwt-1\n"), 0o600)
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", file)
	t.Setenv("AZURE_TENANT_ID", "tenant-2")
	t.Setenv("AZURE_CLIENT_ID", "app-2")
	t.Setenv("AZURE_AUTHORITY_HOST", f.URL)
	a := f.open(t, AzureConfig{Auth: AzureWorkloadIdentity})
	now := time.Now()
	a.now = func() time.Time { return now }
	if err := a.check(); err != nil {
		t.Fatal(err)
	}
	form := f.lastForm.Load().(url.Values)
	if form["client_assertion"][0] != "jwt-1" || form["client_id"][0] != "app-2" ||
		form["client_assertion_type"][0] != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
		t.Fatalf("token request %v", form)
	}
	os.WriteFile(file, []byte("jwt-2"), 0o600) // the kubelet rotated it
	now = now.Add(time.Hour)
	if err := a.check(); err != nil || f.lastForm.Load().(url.Values)["client_assertion"][0] != "jwt-2" {
		t.Fatalf("after rotation: %v %v", err, f.lastForm.Load())
	}
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", "")
	if _, err := newAzure(AzureConfig{AccountName: "acct1", ContainerName: "c12", Auth: AzureWorkloadIdentity}); err == nil ||
		!strings.Contains(err.Error(), "AZURE_FEDERATED_TOKEN_FILE") {
		t.Fatalf("no token file: %v", err)
	}
}

// Managed identity asks the metadata service, with Metadata: true and the
// user-assigned identity's client id, and takes its string expires_in.
func TestAzureManagedIdentity(t *testing.T) {
	f := newFakeAzure(t)
	a := f.open(t, AzureConfig{Auth: AzureManagedIdentity, ClientID: "mi-1", IMDSEndpoint: f.URL + "/metadata/identity/oauth2/token"})
	if err := a.check(); err != nil {
		t.Fatal(err)
	}
	if h, _ := f.lastMeta.Load().(string); !strings.HasPrefix(h, "true ") || !strings.Contains(h, "client_id=mi-1") ||
		!strings.Contains(h, "resource=https%3A%2F%2Fstorage.azure.com%2F") {
		t.Fatalf("metadata request %q", h)
	}
	if tok := a.auth.Load().tok; tok.expires.Sub(tok.refresh) != 5*time.Minute || time.Until(tok.expires) < 23*time.Hour {
		t.Fatalf("expires %v refresh %v", tok.expires, tok.refresh)
	}
}

// A SAS rides on the query instead of an Authorization header, and a 503
// is retried.
func TestAzureSASAndRetry(t *testing.T) {
	old := azureBackoff
	azureBackoff = time.Millisecond
	t.Cleanup(func() { azureBackoff = old })
	f := newFakeAzure(t)
	a := f.open(t, AzureConfig{Auth: AzureSASToken, SASToken: "sv=2022-11-02&sp=rl&sig=abc%2Bdef%3D"})
	f.failLists.Store(2)
	if err := a.check(); err != nil {
		t.Fatal(err)
	}
	if n := f.lists.Load(); n != 3 {
		t.Fatalf("%d listings, want 2 failed and 1 answered", n)
	}
	if auth, _ := f.lastAuth.Load().(string); !strings.HasPrefix(auth, "|") || !strings.Contains(auth, "sig=abc%2Bdef%3D") || !strings.Contains(auth, "sp=rl") {
		t.Fatalf("request %q", auth)
	}
	f.failLists.Store(azureRetries + 1)
	if err := a.check(); err == nil || !isUnavailable(err) {
		t.Fatalf("after %d retries: %v", azureRetries, err)
	}
}

// ---- against Azurite (COMMANDCENTER_TEST_AZURITE_URL, e.g.
// http://azurite-azure:10000/devstoreaccount1) ----

// AzuriteContainer creates a fresh container on the Azurite at endpoint and
// removes it when the test ends.
func azuriteContainer(t *testing.T) (endpoint, container string) {
	t.Helper()
	endpoint = os.Getenv("COMMANDCENTER_TEST_AZURITE_URL")
	if endpoint == "" {
		t.Skip("COMMANDCENTER_TEST_AZURITE_URL is not set")
	}
	b := make([]byte, 6)
	rand.Read(b)
	container = "t" + hex.EncodeToString(b)
	a, err := newAzure(AzureConfig{AccountName: azuriteAccount, ContainerName: container, Endpoint: endpoint, AccountKey: azuriteKey})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method string, status int) error {
		resp, err := a.do(func() (*http.Request, error) {
			return a.request(method, "", map[string][]string{"restype": {"container"}}, nil, nil, 0)
		}, status)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := call(http.MethodPut, http.StatusCreated); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { call(http.MethodDelete, http.StatusAccepted) })
	return endpoint, container
}

func azuriteStore(t *testing.T, prefix string) (*Store, *azure) {
	t.Helper()
	ep, c := azuriteContainer(t)
	s, err := OpenAzure(t.TempDir(), AzureConfig{AccountName: azuriteAccount, ContainerName: c, Endpoint: ep, Prefix: prefix, AccountKey: azuriteKey})
	if err != nil {
		t.Fatal(err)
	}
	return s, s.be.(*azure)
}

func TestAzuriteBlobs(t *testing.T) {
	s, _ := azuriteStore(t, "/cc/blobs/")
	if err := s.Check(); err != nil {
		t.Fatal(err)
	}
	data := []byte("0123456789abcdefghij")
	sums, err := s.Write(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(sums.SHA256)
	if err != nil || info.Size != 20 || time.Since(info.ModTime) > time.Minute {
		t.Fatalf("stat %+v %v", info, err)
	}
	f, err := s.Open(sums.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := make([]byte, 5)
	if n, err := f.ReadAt(p, 10); n != 5 || err != nil || string(p) != "abcde" {
		t.Fatalf("ReadAt = %d %v %q", n, err, p)
	}
	if n, err := f.ReadAt(make([]byte, 10), 15); n != 5 || err != io.EOF {
		t.Fatalf("ReadAt past the end = %d %v", n, err)
	}
	if _, err := f.Seek(-4, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(f); err != nil || string(rest) != "ghij" {
		t.Fatalf("after Seek: %q %v", rest, err)
	}
	f.Seek(0, io.SeekStart)
	if all, err := io.ReadAll(f); err != nil || !bytes.Equal(all, data) {
		t.Fatalf("whole: %q %v", all, err)
	}
	empty, err := s.Write(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if ef, err := s.Open(empty.SHA256); err != nil {
		t.Fatal(err)
	} else if b, err := io.ReadAll(ef); err != nil || len(b) != 0 {
		t.Fatalf("empty blob: %q %v", b, err)
	}
	if p := s.Health(); len(p) != 1 || !p[0].OK || !strings.Contains(p[0].Target, "/cc/blobs") {
		t.Fatalf("health %+v", p)
	}
	if err := s.Remove(sums.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(sums.SHA256); err != nil {
		t.Fatalf("removing a missing blob: %v", err)
	}
	if _, err := s.Stat(sums.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat after remove: %v", err)
	}
	if _, err := s.Open(sums.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open after remove: %v", err)
	}
}

// A blob over the block size goes up in blocks of it, committed as one block
// list, and reads back whole and by range.
func TestAzuriteBlocks(t *testing.T) {
	old := azureBlockSize
	azureBlockSize = 1000
	t.Cleanup(func() { azureBlockSize = old })
	s, a := azuriteStore(t, "")
	data := make([]byte, 4500)
	rand.Read(data)
	sums, err := s.Write(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.do(func() (*http.Request, error) {
		return a.request(http.MethodGet, sums.SHA256, map[string][]string{"comp": {"blocklist"}}, nil, nil, 0)
	}, http.StatusOK)
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Blocks []struct {
			Size int64 `xml:"Size"`
		} `xml:"CommittedBlocks>Block"`
	}
	xml.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Blocks) != 5 || list.Blocks[0].Size != 1000 || list.Blocks[4].Size != 500 {
		t.Fatalf("committed blocks %+v", list.Blocks)
	}
	f, err := s.Open(sums.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if all, err := io.ReadAll(f); err != nil || !bytes.Equal(all, data) {
		t.Fatalf("read back %d bytes, %v", len(all), err)
	}
	p := make([]byte, 1500)
	if n, err := f.ReadAt(p, 2900); n != 1500 || err != nil || !bytes.Equal(p, data[2900:4400]) {
		t.Fatalf("ReadAt across blocks = %d %v", n, err)
	}
	if info, err := s.Stat(sums.SHA256); err != nil || info.Size != 4500 {
		t.Fatalf("stat %+v %v", info, err)
	}
}

// Walk pages through every blob and never reports a meta object; the meta
// objects list in lexical order under their prefix, read back, and a missing
// one is ErrNotFound.
func TestAzuriteWalkAndMeta(t *testing.T) {
	old := azureListMax
	azureListMax = 2
	t.Cleanup(func() { azureListMax = old })
	s, a := azuriteStore(t, "p")
	want := map[string]bool{}
	for i := range 5 {
		sums, err := s.Write(strings.NewReader(fmt.Sprint("blob ", i)))
		if err != nil {
			t.Fatal(err)
		}
		want[sums.SHA256] = true
	}
	for _, k := range []string{"journal/2026/10/30/09/b.json", "journal/2026/10/30/09/a.json", "journal/2026/10/29/23/z.json", "other/x"} {
		if err := a.putMeta(k, []byte("{"+k+"}")); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]bool{}
	if err := s.Walk(func(sha string, mod time.Time) error { got[sha] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("walk %v, want %v", got, want)
	}
	if n, b, err := s.Usage(t.Context()); err != nil || n != 5 || b != 30 {
		t.Fatalf("usage %d %d %v", n, b, err)
	}
	var keys []string
	if err := a.listMeta("journal/", func(k string) error { keys = append(keys, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, " ") != "journal/2026/10/29/23/z.json journal/2026/10/30/09/a.json journal/2026/10/30/09/b.json" {
		t.Fatalf("listMeta %v", keys)
	}
	if b, err := a.getMeta("other/x"); err != nil || string(b) != "{other/x}" {
		t.Fatalf("getMeta %q %v", b, err)
	}
	if _, err := a.getMeta("other/missing"); err != ErrNotFound {
		t.Fatalf("missing meta object: %v", err)
	}
	if err := a.removeMeta("other/x"); err != nil {
		t.Fatal(err)
	}
	if err := a.removeMeta("other/x"); err != nil {
		t.Fatalf("removing a missing meta object: %v", err)
	}
	if _, err := a.getMeta("other/x"); err != ErrNotFound {
		t.Fatalf("after remove: %v", err)
	}
	// The blobs live at p/<sha>, the meta objects at p/meta/<key>.
	if _, err := a.stat(strings.Repeat("0", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat of a missing blob: %v", err)
	}
	if body, _, err := a.get("p/meta/journal/2026/10/30/09/a.json", 0, -1); err != nil {
		t.Fatalf("meta object not at p/meta/<key>: %v", err)
	} else {
		body.Close()
	}
}

// A wrong key or a missing container fails the check; new credentials
// replace the old ones in place, but the location cannot change.
func TestAzuriteCheckAndCredentials(t *testing.T) {
	s, a := azuriteStore(t, "")
	cfg := a.cfg
	bad := cfg
	bad.AccountKey = "d3Jvbmc="
	if err := CheckAzure(bad); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("wrong key: %v", err)
	}
	missing := cfg
	missing.ContainerName = "missing-container"
	if err := CheckAzure(missing); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing container: %v", err)
	}
	if err := s.SetAzure(missing); err == nil {
		t.Fatal("SetAzure moved the store to another container")
	}
	if err := s.SetAzure(bad); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(); err == nil {
		t.Fatal("the wrong key still lists")
	}
	if err := s.SetAzure(cfg); err != nil || s.Check() != nil {
		t.Fatalf("back to the right key: %v %v", err, s.Check())
	}
	if got, ok := s.Azure(); !ok || got != cfg {
		t.Fatalf("Azure() = %+v %v", got, ok)
	}
}
