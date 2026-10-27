package maven

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/repo"
)

func versionsOf(t *testing.T, e *env, repoName string) string {
	t.Helper()
	w := e.do("GET", repoName, "org/example/app/maven-metadata.xml", "")
	if w.Code != 200 {
		t.Fatalf("metadata: %d %s", w.Code, w.Body)
	}
	m, err := ParseMetadata(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(m.Versioning.Versions, ",") + " release=" + m.Versioning.Release
}

func TestPutAndGet(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "releases", "org/example/app/1.0/app-1.0.jar", "jar1"); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "releases", "org/example/app/1.0/app-1.0.jar", ""); w.Body.String() != "jar1" {
		t.Fatalf("GET after PUT: %q", w.Body)
	}
	if w := e.do("PUT", "releases", "org/example/app/1.0/app-1.0.jar.sha1", "ignored"); w.Code != 201 {
		t.Fatalf("PUT checksum: %d", w.Code)
	}
	if w := e.do("GET", "releases", "org/example/app/1.0/app-1.0.jar.sha1", ""); w.Body.String() != sha1hex("jar1") {
		t.Fatalf("checksum must be generated, got %q", w.Body)
	}
}

func TestWritePolicies(t *testing.T) {
	e := newEnv(t)
	e.do("PUT", "releases", "org/example/app/1.0/app-1.0.jar", "a")
	if w := e.do("PUT", "releases", "org/example/app/1.0/app-1.0.jar", "b"); w.Code != 400 {
		t.Fatalf("allow_once redeploy: %d", w.Code)
	}
	// allow: snapshots repo accepts redeploys
	path := "org/example/app/1.0-SNAPSHOT/app-1.0-SNAPSHOT.jar"
	e.do("PUT", "snapshots", path, "a")
	if w := e.do("PUT", "snapshots", path, "b"); w.Code != 201 {
		t.Fatalf("allow redeploy: %d", w.Code)
	}
}

func TestDenyPolicy(t *testing.T) {
	e := newEnv(t, configRepo("frozen", "deny"))
	if w := e.do("PUT", "frozen", "org/example/app/1.0/app-1.0.jar", "a"); w.Code != 400 {
		t.Fatalf("deny: %d", w.Code)
	}
	e.seed(t, "frozen", "org/example/app/1.0/app-1.0.jar", "kept")
	if w := e.do("DELETE", "frozen", "org/example/app/1.0/app-1.0.jar", ""); w.Code != 400 {
		t.Fatalf("deny DELETE: %d", w.Code)
	}
	if w := e.do("GET", "frozen", "org/example/app/1.0/app-1.0.jar", ""); w.Code != 200 || w.Body.String() != "kept" {
		t.Fatalf("asset must survive denied DELETE: %d %q", w.Code, w.Body)
	}
}

func TestAllowOnceConcurrent(t *testing.T) {
	e := newEnv(t)
	const n = 10
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes <- e.do("PUT", "releases", "org/example/app/1.0/app-1.0.jar", fmt.Sprintf("body-%d", i)).Code
		}(i)
	}
	wg.Wait()
	close(codes)
	created := 0
	for c := range codes {
		switch c {
		case 201:
			created++
		case 400:
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if created != 1 {
		t.Fatalf("allow_once: %d concurrent PUTs created, want exactly 1", created)
	}
}

func TestVersionPolicies(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "releases", "org/example/app/1.0-SNAPSHOT/app-1.0-SNAPSHOT.jar", "a"); w.Code != 400 {
		t.Fatalf("release repo accepted snapshot: %d", w.Code)
	}
	if w := e.do("PUT", "releases", "org/example/app/1.0-SNAPSHOT/maven-metadata.xml", "<metadata/>"); w.Code != 400 {
		t.Fatalf("release repo accepted snapshot metadata: %d", w.Code)
	}
	if w := e.do("PUT", "snapshots", "org/example/app/1.0/app-1.0.jar", "a"); w.Code != 400 {
		t.Fatalf("snapshot repo accepted release: %d", w.Code)
	}
	if w := e.do("PUT", "releases", "org/example/app/1.0/bogus.jar", "a"); w.Code != 400 {
		t.Fatalf("invalid artifact path: %d", w.Code)
	}
}

func TestMetadataMaintenance(t *testing.T) {
	e := newEnv(t)
	e.putAll(t, "releases", "org/example/app/1.0/app-1.0.pom", "org/example/app/2.0/app-2.0.pom")
	if got := versionsOf(t, e, "releases"); got != "1.0,2.0 release=2.0" {
		t.Fatalf("after puts: %s", got)
	}
	// a client-uploaded document is accepted, but the versions are the server's (L-11)
	up := `<metadata><groupId>org.example</groupId><artifactId>app</artifactId>
<versioning><versions><version>0.5</version></versions><lastUpdated>20200101000000</lastUpdated></versioning></metadata>`
	if w := e.do("PUT", "releases", "org/example/app/maven-metadata.xml", up); w.Code != 201 {
		t.Fatalf("PUT metadata: %d %s", w.Code, w.Body)
	}
	if got := versionsOf(t, e, "releases"); got != "1.0,2.0 release=2.0" {
		t.Fatalf("after metadata put: %s", got)
	}
	if w := e.do("PUT", "releases", "org/example/app/maven-metadata.xml", "not xml"); w.Code != 400 {
		t.Fatalf("bad metadata: %d", w.Code)
	}
	// delete rebuilds from the remaining artifacts
	if w := e.do("DELETE", "releases", "org/example/app/2.0/app-2.0.pom", ""); w.Code != 204 {
		t.Fatalf("DELETE: %d", w.Code)
	}
	if got := versionsOf(t, e, "releases"); got != "1.0 release=1.0" {
		t.Fatalf("after delete: %s", got)
	}
	if w := e.do("DELETE", "releases", "org/example/app/2.0/app-2.0.pom", ""); w.Code != 404 {
		t.Fatalf("DELETE missing: %d", w.Code)
	}
}

func TestGroupAndProxyWrites(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "public", "org/example/app/3.0/app-3.0.jar", "g"); w.Code != 201 {
		t.Fatalf("group PUT: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "releases", "org/example/app/3.0/app-3.0.jar", ""); w.Body.String() != "g" {
		t.Fatalf("group PUT should land in writeMember: %q", w.Body)
	}
	if w := e.do("PUT", "readonly-group", "org/example/app/3.0/app-3.0.jar", "g"); w.Code != 405 {
		t.Fatalf("group without writeMember: %d", w.Code)
	}
	if w := e.do("PUT", "central", "org/example/app/3.0/app-3.0.jar", "g"); w.Code != 405 {
		t.Fatalf("proxy PUT: %d", w.Code)
	}
	if w := e.do("POST", "releases", "x/y/1/y-1.jar", "g"); w.Code != 405 {
		t.Fatalf("POST: %d", w.Code)
	}
}

func configRepo(name, writePolicy string) config.Repository {
	return config.Repository{Name: name, Format: "maven", Type: "hosted", VersionPolicy: "mixed", WritePolicy: writePolicy}
}

func TestPutMetadataOverGlobalCapIs413(t *testing.T) {
	e := newEnv(t)
	doc := "<metadata><groupId>g</groupId><artifactId>a</artifactId>" + strings.Repeat("<!-- x -->", 100) + "</metadata>"
	w := httptest.NewRecorder()
	body := http.MaxBytesReader(w, io.NopCloser(strings.NewReader(doc)), 100)
	err := e.h.putMetadata(context.Background(), e.repos["releases"], "org/example/app/maven-metadata.xml", body)
	if repo.StatusOf(err) != 413 {
		t.Fatalf("err = %v (status %d), want 413", err, repo.StatusOf(err))
	}
}

func TestPutMetadataOverUploadCapIs413(t *testing.T) {
	e := newEnv(t)
	doc := func(size int) string {
		return "<metadata>" + strings.Repeat(" ", size-len("<metadata></metadata>")) + "</metadata>"
	}
	if w := e.do("PUT", "releases", "org/example/app/maven-metadata.xml", doc(maxUploadedMetadata)); w.Code != 201 {
		t.Fatalf("at the cap: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "releases", "org/example/app/maven-metadata.xml", doc(maxUploadedMetadata+1)); w.Code != 413 {
		t.Fatalf("over the cap: %d %s", w.Code, w.Body)
	}
}

// Uploads are read and decoded in at most cap(metadataSem) slots: one that
// finds every slot busy for metadataWaitTimeout gets 503, and every upload,
// failed or not, gives its slot back.
func TestMetadataUploadSlots(t *testing.T) {
	e := newEnv(t)
	oldWait := metadataWaitTimeout
	metadataWaitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { metadataWaitTimeout = oldWait })
	const path = "org/example/app/maven-metadata.xml"
	put := func(body io.Reader) int {
		w := httptest.NewRecorder()
		e.h.Serve(w, httptest.NewRequest("PUT", "/repository/releases/"+path, body), e.repos["releases"], path)
		return w.Code
	}
	codes := make(chan int, cap(metadataSem))
	var stalled []*io.PipeWriter
	for range cap(metadataSem) {
		pr, pw := io.Pipe()
		t.Cleanup(func() { pw.Close() })
		stalled = append(stalled, pw)
		go func() { codes <- put(pr) }()
	}
	for deadline := time.Now().Add(5 * time.Second); len(metadataSem) < cap(metadataSem); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d stalled uploads hold %d slots", cap(metadataSem), len(metadataSem))
		}
	}
	if code := put(strings.NewReader("<metadata/>")); code != 503 {
		t.Fatalf("upload with every slot busy: %d, want 503", code)
	}
	for _, pw := range stalled {
		io.WriteString(pw, "<metadata/>")
		pw.Close()
		if code := <-codes; code != 201 {
			t.Fatalf("stalled upload: %d", code)
		}
	}
	put(strings.NewReader("not xml"))
	put(strings.NewReader("<metadata>" + strings.Repeat(" ", maxUploadedMetadata) + "</metadata>"))
	if n := len(metadataSem); n != 0 {
		t.Fatalf("%d slots still taken after the uploads ended", n)
	}
}

// A slot holder must send its body within metadataReadTimeout (408), even
// one sending steadily enough for the idle timeout never to fire: two such
// clients delay another upload by that much, not until metadataWaitTimeout
// (503).
func TestSlowMetadataUploadLosesSlot(t *testing.T) {
	e := newEnv(t)
	oldRead, oldWait := metadataReadTimeout, metadataWaitTimeout
	metadataReadTimeout, metadataWaitTimeout = 500*time.Millisecond, 3*time.Second
	t.Cleanup(func() { metadataReadTimeout, metadataWaitTimeout = oldRead, oldWait })
	const path = "org/example/app/maven-metadata.xml"
	cut := iotest.ErrReader(&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded})
	if err := e.h.putMetadata(context.Background(), e.repos["releases"], path, cut); repo.StatusOf(err) != 408 {
		t.Fatalf("body cut at the deadline: %v (status %d), want 408", err, repo.StatusOf(err))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = repo.IdleBody(w, r, time.Minute) // as the server wraps every body
		defer r.Body.Close()
		e.h.Serve(w, r, e.repos["releases"], strings.TrimPrefix(r.URL.Path, "/repository/releases/"))
	}))
	t.Cleanup(srv.Close)
	for i := range cap(metadataSem) {
		c, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		fmt.Fprintf(c, "PUT /repository/releases/org/example/slow%d/maven-metadata.xml HTTP/1.1\r\nHost: x\r\nContent-Length: 100000\r\n\r\n<metadata>", i)
		go func() { // a byte every 20 ms until the connection is closed
			for {
				time.Sleep(20 * time.Millisecond)
				if _, err := io.WriteString(c, " "); err != nil {
					return
				}
			}
		}()
	}
	for deadline := time.Now().Add(5 * time.Second); len(metadataSem) < cap(metadataSem); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("slow uploads hold %d slots", len(metadataSem))
		}
	}
	start := time.Now()
	req, _ := http.NewRequest("PUT", srv.URL+"/repository/releases/"+path, strings.NewReader("<metadata/>"))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("upload behind slow ones: %d after %v, want 201", resp.StatusCode, time.Since(start))
	}
}

// cancelOnEmit cancels the request whose artifact PUT emits an event, which
// makes the metadata rebuild after it fail.
type cancelOnEmit context.CancelFunc

func (c cancelOnEmit) Emit(kind, actor, repo, subject, detail string) { c() }

// mvn deploy uploads the GA maven-metadata.xml after the artifacts. That
// upload rebuilds the document from the stored artifacts, so it repairs a
// rebuild that failed after an artifact PUT; the client's versions are still
// ignored, and a GA without artifacts still gets no document.
func TestClientGAMetadataRepairsFailedRebuild(t *testing.T) {
	e := newEnv(t)
	e.putAll(t, "releases", "org/example/app/1.0/app-1.0.pom")
	ctx, cancel := context.WithCancel(context.Background())
	e.h.Store.Events = cancelOnEmit(cancel)
	w := httptest.NewRecorder()
	const pom = "org/example/app/1.1/app-1.1.pom"
	e.h.Serve(w, httptest.NewRequestWithContext(ctx, "PUT", "/repository/releases/"+pom, strings.NewReader(fileBody(pom))), e.repos["releases"], pom)
	e.h.Store.Events = nil
	if got := versionsOf(t, e, "releases"); w.Code != 201 || got != "1.0 release=1.0" {
		t.Fatalf("PUT with a failed rebuild: %d, metadata %s", w.Code, got)
	}
	ga := func(artifact string) string {
		return fmt.Sprintf(`<metadata><groupId>org.example</groupId><artifactId>%s</artifactId><versioning><release>9.9</release>
<versions><version>1.1</version><version>9.9</version></versions></versioning></metadata>`, artifact)
	}
	for _, a := range []string{"app", "ghost"} {
		if w := e.do("PUT", "releases", "org/example/"+a+"/maven-metadata.xml", ga(a)); w.Code != 201 {
			t.Fatalf("PUT %s metadata: %d %s", a, w.Code, w.Body)
		}
	}
	if got := versionsOf(t, e, "releases"); got != "1.0,1.1 release=1.1" {
		t.Fatalf("after the GA upload: %s", got)
	}
	if w := e.do("GET", "releases", "org/example/ghost/maven-metadata.xml", ""); w.Code != 404 {
		t.Fatalf("GA without artifacts: %d %s", w.Code, w.Body)
	}
}

// L-11: GA versioning is the server's. An upload cannot add a version or a
// <release>, rename the GA or create a document for a GA without artifacts,
// and a document stored by an older build is rebuilt by the next artifact PUT.
func TestClientGAMetadataIgnored(t *testing.T) {
	e := newEnv(t)
	const path = "org/example/app/maven-metadata.xml"
	ga := func() string {
		t.Helper()
		w := e.do("GET", "releases", path, "")
		m, err := ParseMetadata(w.Body)
		if w.Code != 200 || err != nil || m.Versioning == nil {
			t.Fatalf("GA metadata: %d %v %+v", w.Code, err, m)
		}
		v := m.Versioning
		if strings.HasPrefix(v.LastUpdated, "2099") {
			t.Fatalf("client lastUpdated kept: %s", v.LastUpdated)
		}
		return fmt.Sprintf("%s:%s %s latest=%s release=%s", m.GroupID, m.ArtifactID, strings.Join(v.Versions, ","), v.Latest, v.Release)
	}
	e.putAll(t, "releases", "org/example/app/1.0/app-1.0.pom")
	release99 := `<metadata><groupId>org.example</groupId><artifactId>app</artifactId><versioning><latest>9.9</latest><release>9.9</release>
<versions><version>1.0</version><version>9.9</version></versions><lastUpdated>20991231000000</lastUpdated></versioning></metadata>`
	foreign := `<metadata><groupId>evil</groupId><artifactId>other</artifactId><versioning><versions><version>8.0</version></versions></versioning></metadata>`
	for _, p := range []string{path, "org/example/ghost/maven-metadata.xml"} {
		for _, up := range []string{release99, foreign} {
			if w := e.do("PUT", "releases", p, up); w.Code != 201 {
				t.Fatalf("PUT %s: %d %s", p, w.Code, w.Body)
			}
		}
	}
	if got := ga(); got != "org.example:app 1.0 latest=1.0 release=1.0" {
		t.Fatalf("after client uploads: %s", got)
	}
	if w := e.do("GET", "releases", "org/example/ghost/maven-metadata.xml", ""); w.Code != 404 {
		t.Fatalf("GA without artifacts: %d %s", w.Code, w.Body)
	}
	// The QA repro: through the group (Central's copy lists 0.9), [1.0,) must not resolve to 9.9.
	if got := versionsOf(t, e, "public"); got != "0.9,1.0 release=1.0" {
		t.Fatalf("group: %s", got)
	}
	e.seed(t, "releases", path, strings.Replace(release99, "org.example</groupId><artifactId>app", "evil</groupId><artifactId>other", 1))
	e.putAll(t, "releases", "org/example/app/2.0/app-2.0.pom")
	if got := ga(); got != "org.example:app 1.0,2.0 latest=2.0 release=2.0" {
		t.Fatalf("stored client document not rebuilt: %s", got)
	}
}

// Plugin-group documents still merge: <plugins> is the part of an upload the
// server keeps, also beside a GA's versioning (groupId org.example and GA
// org:example share org/example/maven-metadata.xml), which a GA upload for
// that directory rebuilds without losing them.
func TestPluginGroupMetadataMerged(t *testing.T) {
	e := newEnv(t)
	const path = "org/example/maven-metadata.xml"
	plugin := func(prefix string) string {
		return fmt.Sprintf(`<metadata><plugins><plugin><name>%[1]s</name><prefix>%[1]s</prefix><artifactId>%[1]s-maven-plugin</artifactId></plugin></plugins>
<versioning><versions><version>9.9</version></versions></versioning></metadata>`, prefix)
	}
	if w := e.do("PUT", "releases", path, plugin("a")); w.Code != 201 {
		t.Fatalf("PUT plugin a: %d %s", w.Code, w.Body)
	}
	e.putAll(t, "releases", "org/example/1.0/example-1.0.pom")
	if w := e.do("PUT", "releases", path, plugin("b")); w.Code != 201 {
		t.Fatalf("PUT plugin b: %d %s", w.Code, w.Body)
	}
	doc := func() string {
		t.Helper()
		w := e.do("GET", "releases", path, "")
		m, err := ParseMetadata(w.Body)
		if w.Code != 200 || err != nil || m.Versioning == nil {
			t.Fatalf("metadata: %d %v %+v", w.Code, err, m)
		}
		var prefixes []string
		for _, p := range m.Plugins {
			prefixes = append(prefixes, p.Prefix+"="+p.ArtifactID)
		}
		return fmt.Sprintf("%s %s:%s %s", strings.Join(prefixes, ","), m.GroupID, m.ArtifactID, strings.Join(m.Versioning.Versions, ","))
	}
	if got := doc(); got != "a=a-maven-plugin,b=b-maven-plugin org:example 1.0" {
		t.Fatalf("metadata: %s", got)
	}
	e.seed(t, "releases", "org/example/2.0/example-2.0.pom", "p") // its rebuild failed
	if w := e.do("PUT", "releases", path, `<metadata><groupId>org</groupId><artifactId>example</artifactId></metadata>`); w.Code != 201 {
		t.Fatalf("PUT GA metadata: %d %s", w.Code, w.Body)
	}
	if got := doc(); got != "a=a-maven-plugin,b=b-maven-plugin org:example 1.0,2.0" {
		t.Fatalf("after the GA upload: %s", got)
	}
}

func TestDeleteLastArtifactsDropsVersionFromGAMetadata(t *testing.T) {
	e := newEnv(t)
	e.putAll(t, "releases", "org/example/app/1.0/app-1.0.jar", "org/example/app/1.0/app-1.0.pom", "org/example/app/2.0/app-2.0.pom")
	for _, f := range []string{"app-1.0.jar", "app-1.0.jar.sha1", "app-1.0.pom"} {
		if w := e.do("DELETE", "releases", "org/example/app/1.0/"+f, ""); w.Code != 204 {
			t.Fatalf("DELETE %s: %d", f, w.Code)
		}
	}
	if got := versionsOf(t, e, "releases"); got != "2.0 release=2.0" {
		t.Fatalf("after deleting 1.0: %s", got)
	}
}

func TestDeleteLastSnapshotFilesRemovesVersionMetadata(t *testing.T) {
	e := newEnv(t)
	base := "org/example/app/1.0-SNAPSHOT/"
	e.putAll(t, "snapshots", base+"app-1.0-20200101.000000-1.jar", base+"app-1.0-20200101.000000-1.pom")
	e.do("DELETE", "snapshots", base+"app-1.0-20200101.000000-1.jar", "")
	if w := e.do("GET", "snapshots", base+"maven-metadata.xml", ""); w.Code != 200 {
		t.Fatalf("version metadata must stay while artifacts remain: %d", w.Code)
	}
	e.do("DELETE", "snapshots", base+"app-1.0-20200101.000000-1.pom", "")
	if w := e.do("GET", "snapshots", base+"maven-metadata.xml", ""); w.Code != 404 {
		t.Fatalf("version metadata must be gone: %d", w.Code)
	}
	if w := e.do("GET", "snapshots", "org/example/app/maven-metadata.xml", ""); w.Code != 404 {
		t.Fatalf("GA metadata must be gone: %d", w.Code)
	}
}

// snapshotMeta reads the version-level maven-metadata.xml of verDir.
func snapshotMeta(t *testing.T, e *env, repoName, verDir string) *Metadata {
	t.Helper()
	w := e.do("GET", repoName, verDir+"/maven-metadata.xml", "")
	if w.Code != 200 {
		t.Fatalf("%s version metadata: %d %s", verDir, w.Code, w.Body)
	}
	m, err := ParseMetadata(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	if m.Versioning == nil || m.Versioning.Snapshot == nil {
		t.Fatalf("%s: no <snapshot>: %+v", verDir, m)
	}
	return m
}

// snapshotValues lists the <snapshotVersions> as classifier:extension=value.
func snapshotValues(m *Metadata) string {
	var out []string
	for _, sv := range m.Versioning.SnapshotVersions {
		out = append(out, sv.Classifier+":"+sv.Extension+"="+sv.Value)
	}
	return strings.Join(out, ",")
}

func clientVersionMeta(verDir, ts string, build int) string {
	return fmt.Sprintf(`<metadata><groupId>org.example</groupId><artifactId>app</artifactId><version>%s</version>
<versioning><snapshot><timestamp>%s</timestamp><buildNumber>%d</buildNumber></snapshot><lastUpdated>20991231000000</lastUpdated>
<snapshotVersions><snapshotVersion><extension>jar</extension><value>bogus-%d</value><updated>20991231000000</updated></snapshotVersion></snapshotVersions></versioning></metadata>`,
		verDir[strings.LastIndexByte(verDir, '/')+1:], ts, build, build)
}

// M-15 repro 1: deleting the newest build moves the version metadata back.
func TestSnapshotMetadataAfterDeleteOfLatestBuild(t *testing.T) {
	e := newEnv(t)
	verDir := "org/example/app/1.1.0-SNAPSHOT"
	for b := 1; b <= 3; b++ {
		ts := fmt.Sprintf("20261022.01000%d", b)
		e.putAll(t, "snapshots",
			fmt.Sprintf("%s/app-1.1.0-%s-%d.jar", verDir, ts, b),
			fmt.Sprintf("%s/app-1.1.0-%s-%d.pom", verDir, ts, b))
		// mvn deploy uploads its own version document after the files
		if w := e.do("PUT", "snapshots", verDir+"/maven-metadata.xml", clientVersionMeta(verDir, ts, b)); w.Code != 201 {
			t.Fatalf("client metadata PUT: %d %s", w.Code, w.Body)
		}
	}
	m := snapshotMeta(t, e, "snapshots", verDir)
	if m.Versioning.Snapshot.BuildNumber != 3 || m.Versioning.Snapshot.Timestamp != "20261022.010003" {
		t.Fatalf("before delete: %+v", m.Versioning.Snapshot)
	}
	for _, f := range []string{"app-1.1.0-20261022.010003-3.jar", "app-1.1.0-20261022.010003-3.pom"} {
		if w := e.do("DELETE", "snapshots", verDir+"/"+f, ""); w.Code != 204 {
			t.Fatalf("DELETE %s: %d", f, w.Code)
		}
	}
	m = snapshotMeta(t, e, "snapshots", verDir)
	if m.Version != "1.1.0-SNAPSHOT" || m.GroupID != "org.example" || m.ArtifactID != "app" {
		t.Fatalf("header: %+v", m)
	}
	if m.Versioning.Snapshot.BuildNumber != 2 || m.Versioning.Snapshot.Timestamp != "20261022.010002" {
		t.Fatalf("after delete: %+v", m.Versioning.Snapshot)
	}
	if got := snapshotValues(m); got != ":jar=1.1.0-20261022.010002-2,:pom=1.1.0-20261022.010002-2" {
		t.Fatalf("snapshotVersions: %s", got)
	}
}

// M-15 repro 2: plain PUTs of timestamped files (no client metadata).
func TestSnapshotMetadataCreatedForRawPuts(t *testing.T) {
	e := newEnv(t)
	verDir := "org/example/app/2.0-SNAPSHOT"
	e.putAll(t, "snapshots", verDir+"/app-2.0-20261022.020000-1.jar", verDir+"/app-2.0-20261022.020000-1.pom",
		verDir+"/app-2.0-20261022.020000-1-sources.jar")
	m := snapshotMeta(t, e, "snapshots", verDir)
	if m.Versioning.Snapshot.BuildNumber != 1 || m.Versioning.Snapshot.Timestamp != "20261022.020000" || m.Versioning.LastUpdated == "" {
		t.Fatalf("snapshot: %+v", m.Versioning)
	}
	if got := snapshotValues(m); got != ":jar=2.0-20261022.020000-1,:pom=2.0-20261022.020000-1,sources:jar=2.0-20261022.020000-1" {
		t.Fatalf("snapshotVersions: %s", got)
	}
	for _, sv := range m.Versioning.SnapshotVersions {
		if sv.Updated != "20261022020000" {
			t.Fatalf("updated: %+v", sv)
		}
	}
}

// Decision 6 / L-11: a client version document is accepted but not stored.
func TestClientVersionMetadataIgnored(t *testing.T) {
	e := newEnv(t)
	verDir := "org/example/app/3.0-SNAPSHOT"
	e.putAll(t, "snapshots", verDir+"/app-3.0-20261022.030000-1.jar")
	if w := e.do("PUT", "snapshots", verDir+"/maven-metadata.xml", clientVersionMeta(verDir, "20991231.000000", 9)); w.Code != 201 {
		t.Fatalf("client metadata PUT: %d %s", w.Code, w.Body)
	}
	m := snapshotMeta(t, e, "snapshots", verDir)
	if m.Versioning.Snapshot.BuildNumber != 1 || strings.Contains(snapshotValues(m), "bogus") {
		t.Fatalf("client document leaked: %+v %s", m.Versioning.Snapshot, snapshotValues(m))
	}
	// Without any timestamped file there is no server document to serve.
	other := "org/example/app/4.0-SNAPSHOT"
	if w := e.do("PUT", "snapshots", other+"/maven-metadata.xml", clientVersionMeta(other, "20991231.000000", 9)); w.Code != 201 {
		t.Fatalf("client metadata PUT: %d", w.Code)
	}
	if w := e.do("GET", "snapshots", other+"/maven-metadata.xml", ""); w.Code != 404 {
		t.Fatalf("client-only version metadata: %d %s", w.Code, w.Body)
	}
}

// L-14: a timestamped snapshot version is not a release.
func TestReleaseRepoRejectsTimestampedSnapshot(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{
		"org/example/app/1.4-20260101.000000-1/app-1.4-20260101.000000-1.jar",
		"org/example/app/1.4-20260101.000000-1/maven-metadata.xml",
	} {
		if w := e.do("PUT", "releases", p, "<metadata/>"); w.Code != 400 {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	if w := e.do("PUT", "releases", "org/example/app/1.4/app-1.4.jar", "ok"); w.Code != 201 {
		t.Fatalf("plain release: %d", w.Code)
	}
}

// sbt 1.9+ publishes a plugin in two layouts into one version directory: the
// Maven one, whose file names carry the _<scala>_<sbt> cross suffix of the
// artifact directory, and, while sbtPluginPublishLegacyMavenStyle is true
// (the default), sbt's legacy one, whose file names and POM artifactId do
// not (sbt before 1.9 publishes only that one). A hosted repository takes and
// serves both, also through a group, and checks a legacy POM against the name
// without the suffix. The legacy files are no version of the GA: the GA
// document lists the Maven-layout versions, and the name without the suffix
// gets none.
func TestPutSbtPluginBothLayouts(t *testing.T) {
	e := newEnv(t)
	const ga = "com/example/sbt-foo_2.12_1.0"
	files := func(version string, names ...string) []string {
		var out []string
		for _, n := range names {
			for _, s := range []string{".pom", ".jar", "-sources.jar", "-javadoc.jar"} {
				out = append(out, ga+"/"+version+"/"+n+"-"+version+s)
			}
		}
		return out
	}
	const legacyPOM = ga + "/1.0.0/sbt-foo-1.0.0.pom"
	for body, want := range map[string]string{
		strings.Replace(fileBody(legacyPOM), ">sbt-foo<", ">sbt-foo_2.12_1.0<", 1): `declares "com.example:sbt-foo_2.12_1.0:1.0.0", want "com.example:sbt-foo:1.0.0"`,
		strings.Replace(fileBody(legacyPOM), ">com.example<", ">evil<", 1):         `declares "evil:sbt-foo:1.0.0"`,
	} {
		if w := e.do("PUT", "releases", legacyPOM, body); w.Code != 400 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("PUT %s %q: %d %s, want 400 %s", legacyPOM, body, w.Code, w.Body, want)
		}
	}
	stored := append(files("1.0.0", "sbt-foo_2.12_1.0", "sbt-foo"), files("0.9.0", "sbt-foo")...) // 0.9.0 as sbt before 1.9 publishes it
	for _, p := range stored {
		e.putAll(t, "releases", p, p+".sha1", p+".md5")
	}
	for _, p := range stored {
		for _, rp := range []string{"releases", "public"} {
			if w := e.do("GET", rp, p, ""); w.Code != 200 || w.Body.String() != fileBody(p) {
				t.Errorf("GET %s %s: %d %q", rp, p, w.Code, w.Body)
			}
			if w := e.do("GET", rp, p+".sha1", ""); w.Code != 200 || w.Body.String() != sha1hex(fileBody(p)) {
				t.Errorf("GET %s %s.sha1: %d %q", rp, p, w.Code, w.Body)
			}
		}
	}
	versions := func(when string) {
		t.Helper()
		w := e.do("GET", "releases", ga+"/maven-metadata.xml", "")
		m, err := ParseMetadata(w.Body)
		if w.Code != 200 || err != nil || m.ArtifactID != "sbt-foo_2.12_1.0" || strings.Join(m.Versioning.Versions, ",") != "1.0.0" {
			t.Fatalf("GA metadata %s: %d %v %+v", when, w.Code, err, m)
		}
		if w := e.do("GET", "releases", "com/example/sbt-foo/maven-metadata.xml", ""); w.Code != 404 {
			t.Fatalf("metadata of the name without the suffix %s: %d %s", when, w.Code, w.Body)
		}
	}
	versions("after the PUTs")
	if _, err := e.h.RebuildMetadata(context.Background(), e.repos["releases"], 0, nil); err != nil {
		t.Fatal(err)
	}
	versions("after a rebuild")
	if w := e.do("DELETE", "releases", ga+"/1.0.0/sbt-foo-1.0.0.jar", ""); w.Code != 204 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body)
	}
	versions("after a DELETE")

	// Only the legacy names of the version directory's files are taken, and
	// the version policy still applies.
	for _, p := range []string{
		ga + "/1.1.0/sbt-bar-1.1.0.jar",
		ga + "/1.1.0/sbt-foo-1.1.1.jar",
		ga + "/1.1.0/sbt-foo-1.1.0",
		ga + "/1.1.0/sbt-foo_2.12-1.1.0.jar",
		"com/example/sbt-foo_2.12/1.1.0/sbt-foo-1.1.0.jar", // a Scala suffix only
		"sbt-foo_2.12_1.0/1.1.0/sbt-foo-1.1.0.jar",         // no groupId
		ga + "/1.1.0-SNAPSHOT/sbt-foo-1.1.0-SNAPSHOT.jar",
	} {
		if w := e.do("PUT", "releases", p, "x"); w.Code != 400 {
			t.Errorf("PUT %s: %d %s, want 400", p, w.Code, w.Body)
		}
	}
	e.putAll(t, "snapshots", ga+"/1.1.0-SNAPSHOT/sbt-foo-1.1.0-SNAPSHOT.jar")
}

// doRaw is e.do for paths that would not survive URL parsing; the handler
// takes the repository path as an argument.
func (e *env) doRaw(method, repoName, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/raw", strings.NewReader(body))
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

// L-13 applies to new uploads only: assets stored by older builds under
// such names stay readable, deletable and part of the metadata.
func TestLegacyOddNamesStillServed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.putAll(t, "releases", "org/example/app/1.0/app-1.0.jar")
	legacy := []string{
		"org/example/app/2.0/app-2.0.jar;jsessionid=1",
		"org/example/app/3.0/app-3.0.jar#frag",
		"org/example/app/3.0/app-3.0.jar\x00.pom",
		"org/example/app/3.0/app-3.0-caf\xe9.jar",
	}
	if metatest.Postgres() { // PostgreSQL never held a NUL or a non-UTF-8 name
		for _, p := range legacy[2:] {
			if w := e.doRaw("GET", "releases", p, ""); w.Code != 404 {
				t.Fatalf("GET %q: %d %q, want 404", p, w.Code, w.Body)
			}
		}
		legacy = legacy[:2]
	}
	for _, p := range legacy {
		e.seed(t, "releases", p, "legacy")
	}
	for _, p := range legacy {
		if w := e.doRaw("GET", "releases", p, ""); w.Code != 200 || w.Body.String() != "legacy" {
			t.Fatalf("GET %q: %d %q", p, w.Code, w.Body)
		}
		if w := e.doRaw("HEAD", "releases", p, ""); w.Code != 200 {
			t.Fatalf("HEAD %q: %d", p, w.Code)
		}
	}
	if _, err := e.h.RebuildMetadata(ctx, e.repos["releases"], 0, nil); err != nil {
		t.Fatal(err)
	}
	if got := versionsOf(t, e, "releases"); got != "1.0,2.0,3.0 release=3.0" {
		t.Fatalf("rebuild must count legacy files: %s", got)
	}
	if w := e.doRaw("DELETE", "releases", legacy[0], ""); w.Code != 204 {
		t.Fatalf("DELETE %q: %d %s", legacy[0], w.Code, w.Body)
	}
	if got := versionsOf(t, e, "releases"); got != "1.0,3.0 release=3.0" {
		t.Fatalf("after legacy delete: %s", got)
	}
	for _, p := range []string{"org/example/app/4.0/app-4.0.jar;x=1", "org/example/app/4.0/app-4.0.jar\x00.pom", "org/example/app/4.0/app-4.0.jar?q=1", "org/example/app/4.0/app-4.0-caf\xe9.jar"} {
		if w := e.doRaw("PUT", "releases", p, "new"); w.Code != 400 {
			t.Fatalf("PUT %q: %d", p, w.Code)
		}
	}
}
