package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	"commandcenter/internal/format/npm"
	"commandcenter/internal/format/pypi"
	"commandcenter/internal/proxy"
)

const importUsage = "usage: commandcenter import-nexus -from NEXUS_URL -from-user NAME -from-password-file FILE -to CC_URL -to-user NAME " +
	"-to-token-file FILE (-repo NEXUS_REPO=CC_REPO ... | -all) [-docker-from URL] [-dry-run] [-concurrency N] [-skip-existing=false] " +
	"[-insecure] [-ca PEM] (NEXUS_PASSWORD and COMMANDCENTER_TOKEN stand in for the files)"

// nexusFormats maps a Nexus format to the Command Center format the import
// writes with; other formats are reported as unsupported.
var nexusFormats = map[string]string{"maven2": "maven", "npm": "npm", "pypi": "pypi", "raw": "raw", "helm": "helm", "docker": "docker", "apt": "apt"}

// Nexus REST API v1 documents, the fields the import reads.
type nexusRepo struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Type   string `json:"type"`
}

type nexusAsset struct {
	DownloadURL string `json:"downloadUrl"`
	Path        string `json:"path"`
	ContentType string `json:"contentType"`
	FileSize    int64  `json:"fileSize"` // Nexus 3.47+; 0 before (dry-run sizes only)
	Checksum    struct {
		SHA1   string `json:"sha1"`
		SHA256 string `json:"sha256"`
		MD5    string `json:"md5"`
	} `json:"checksum"`
}

type nexusComponent struct {
	Name    string       `json:"name"`
	Version string       `json:"version"`
	Assets  []nexusAsset `json:"assets"`
}

// repoMap is one -repo: a Nexus repository and the Command Center repository
// it is copied into.
type repoMap struct{ nexus, cc string }

// importer copies Nexus hosted repositories into Command Center through each
// format's own upload protocol, so every server-side validation applies.
type importer struct {
	from, to, dockerFrom *url.URL
	src, dst, dockerSrc  *http.Client
	fromAuth, toAuth     string // Authorization header values; never printed
	dryRun, skipExisting bool
	concurrency          int

	mu  sync.Mutex
	out io.Writer
}

// result is what one component's copy did.
type result struct {
	files, skipped int
	bytes          int64
	err            error
}

type tally struct {
	copied, skipped, errors int
	bytes                   int64
}

func (t *tally) add(r result) {
	switch {
	case r.err != nil:
		t.errors++
	case r.files == 0:
		t.skipped++
	default:
		t.copied++
	}
	t.bytes += r.bytes
}

func importNexus(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("import-nexus", flag.ContinueOnError)
	from := fs.String("from", "", "Nexus base URL, such as https://nexus.example.com")
	fromUser := fs.String("from-user", "", "Nexus user (empty: anonymous)")
	fromPW := fs.String("from-password-file", "", "file holding the Nexus password (or NEXUS_PASSWORD)")
	to := fs.String("to", "", "Command Center base URL")
	toUser := fs.String("to-user", "", "Command Center user")
	toTok := fs.String("to-token-file", "", "file holding the user's API token or password (or COMMANDCENTER_TOKEN)")
	dockerFrom := fs.String("docker-from", "", "base URL of the docker connector (its own port) of the docker repository being copied")
	all := fs.Bool("all", false, "copy every hosted repository into the Command Center repository of the same name")
	dryRun := fs.Bool("dry-run", false, "list what would be copied; write nothing")
	conc := fs.Int("concurrency", 4, "components copied at once")
	skip := fs.Bool("skip-existing", true, "skip what the target already has (resumable)")
	insecure := fs.Bool("insecure", false, "do not verify TLS certificates")
	ca := fs.String("ca", "", "PEM file of CA certificates to trust")
	var maps []repoMap
	fs.Func("repo", "NEXUS_REPO=CC_REPO (repeatable; NEXUS_REPO alone keeps the name)", func(v string) error {
		n, c, ok := strings.Cut(v, "=")
		if !ok {
			c = n
		}
		if n == "" || c == "" || strings.ContainsAny(c, "/?#") {
			return fmt.Errorf("invalid -repo %q", v)
		}
		maps = append(maps, repoMap{n, c})
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *to == "" {
		return errors.New("-from and -to are required; " + importUsage)
	}
	if len(maps) == 0 && !*all {
		return errors.New("-repo or -all is required; " + importUsage)
	}
	im := &importer{dryRun: *dryRun, skipExisting: *skip, concurrency: max(*conc, 1), out: stdout}
	var err error
	if im.from, err = parseBase(*from); err != nil {
		return fmt.Errorf("-from: %w", err)
	}
	if im.to, err = parseBase(*to); err != nil {
		return fmt.Errorf("-to: %w", err)
	}
	tlsCfg, err := tlsConfig(*insecure, *ca)
	if err != nil {
		return err
	}
	if im.fromAuth, err = basicAuth(*fromUser, *fromPW, "NEXUS_PASSWORD", "-from-password-file"); err != nil {
		return err
	}
	if im.toAuth, err = basicAuth(*toUser, *toTok, "COMMANDCENTER_TOKEN", "-to-token-file"); err != nil {
		return err
	}
	im.src = proxy.NewClientFor(im.from, tlsCfg)
	im.dst = proxy.NewClientFor(im.to, tlsCfg)
	if *dockerFrom != "" {
		if im.dockerFrom, err = parseBase(*dockerFrom); err != nil {
			return fmt.Errorf("-docker-from: %w", err)
		}
		im.dockerSrc = proxy.NewClientFor(im.dockerFrom, tlsCfg)
	}
	return im.run(context.Background(), maps, *all)
}

// parseBase parses a server base URL. Credentials belong in the flags, where
// they cannot end up in an error message.
func parseBase(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q is not an http(s) URL", s)
	}
	if u.User != nil {
		return nil, errors.New("put the credentials in the -*-user and -*-file flags, not in the URL")
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = strings.TrimRight(u.Path, "/"), "", "", ""
	return u, nil
}

func tlsConfig(insecure bool, caFile string) (*tls.Config, error) {
	if !insecure && caFile == "" {
		return nil, nil
	}
	cfg := &tls.Config{InsecureSkipVerify: insecure}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = x509.NewCertPool()
		if !cfg.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("-ca %s: no certificates found", caFile)
		}
	}
	return cfg, nil
}

// basicAuth returns the Authorization value for user with the secret from
// file (its first line) or else env; "" when there is no user.
func basicAuth(user, file, env, flagName string) (string, error) {
	if user == "" {
		return "", nil
	}
	secret := os.Getenv(env)
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		secret, _, _ = strings.Cut(strings.TrimLeft(string(b), "\r\n"), "\n")
		secret = strings.TrimRight(secret, "\r")
	}
	if secret == "" {
		return "", fmt.Errorf("%s or %s is required with a user", flagName, env)
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+secret)), nil
}

func (im *importer) printf(format string, a ...any) {
	im.mu.Lock()
	defer im.mu.Unlock()
	fmt.Fprintf(im.out, format, a...)
}

// report prints one component's line.
func (im *importer) report(repo, format, name, version string, r result) {
	status := "ok"
	switch {
	case r.err != nil:
		status = "error: " + r.err.Error()
	case r.files == 0:
		status = "skipped"
	case im.dryRun:
		status = "would copy"
	}
	im.printf("%s\t%s\t%s\t%s\t%d\t%d\t%s\n", repo, format, name, version, r.files, r.bytes, status)
}

func (im *importer) run(ctx context.Context, maps []repoMap, all bool) error {
	repos, err := im.nexusRepos(ctx)
	if err != nil {
		return fmt.Errorf("list Nexus repositories: %w", err)
	}
	byName := map[string]nexusRepo{}
	for _, r := range repos {
		byName[r.Name] = r
		if all && r.Type == "hosted" {
			maps = append(maps, repoMap{r.Name, r.Name})
		}
	}
	failed := 0
	var summary []string
	for _, m := range maps {
		r, ok := byName[m.nexus]
		format := nexusFormats[r.Format]
		switch {
		case !ok:
			im.printf("%s: error: not found on the Nexus server\n", m.nexus)
			failed++
			continue
		case r.Type != "hosted":
			im.printf("%s: skipped (%s repository; only hosted repositories are imported)\n", m.nexus, r.Type)
			continue
		case format == "":
			im.printf("%s: skipped (format %s is not supported)\n", m.nexus, r.Format)
			continue
		case format == "docker" && im.dockerFrom == nil:
			im.printf("%s: skipped (docker needs -docker-from with the repository's connector URL)\n", m.nexus)
			continue
		}
		t, err := im.copyRepo(ctx, m, format)
		if err != nil {
			im.printf("%s: error: %v\n", m.nexus, err)
			failed++
		}
		failed += t.errors
		summary = append(summary, fmt.Sprintf("%s -> %s (%s): %d copied, %d skipped, %d failed, %d bytes", m.nexus, m.cc, format, t.copied, t.skipped, t.errors, t.bytes))
	}
	im.printf("summary:\n")
	for _, s := range summary {
		im.printf("  %s\n", s)
	}
	if im.dryRun {
		im.printf("dry run: nothing was written\n")
	}
	if failed > 0 {
		return fmt.Errorf("%d errors (see above)", failed)
	}
	return nil
}

// copyRepo copies the components of one repository, concurrency at a time.
func (im *importer) copyRepo(ctx context.Context, m repoMap, format string) (tally, error) {
	var (
		mu       sync.Mutex
		t        tally
		wg       sync.WaitGroup
		sem      = make(chan struct{}, im.concurrency)
		packages = map[string]bool{} // npm: dist-tags are copied once the versions are there
		images   = map[string]bool{} // docker: every tag is read from the connector
	)
	emit := func(name, version string, r result) {
		im.report(m.nexus, format, name, version, r)
		mu.Lock()
		t.add(r)
		mu.Unlock()
	}
	work := func(name, version string, fn func() result) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			emit(name, version, fn())
		}()
	}
	err := im.components(ctx, m.nexus, func(c nexusComponent) {
		switch format {
		case "docker":
			images[c.Name] = true
			return
		case "npm":
			packages[c.Name] = true
		}
		work(c.Name, c.Version, func() result { return im.copyComponent(ctx, m, format, c) })
	})
	wg.Wait()
	if err != nil {
		return t, err
	}
	for _, image := range sortedKeys(images) {
		tags, err := im.dockerTags(ctx, image)
		if err != nil {
			emit(image, "", result{err: err})
			continue
		}
		for _, tag := range tags {
			work(image, tag, func() result { return im.copyDockerTag(ctx, m.cc, image, tag) })
		}
	}
	wg.Wait()
	for _, name := range sortedKeys(packages) {
		if im.dryRun {
			break
		}
		if err := im.copyDistTags(ctx, m, name); err != nil {
			im.printf("%s\tnpm\t%s\tdist-tags\t0\t0\terror: %v\n", m.nexus, name, err)
			t.errors++
		}
	}
	return t, nil
}

func (im *importer) copyComponent(ctx context.Context, m repoMap, format string, c nexusComponent) (r result) {
	switch format {
	case "maven":
		r.err = im.copyMaven(ctx, m, c, &r)
	case "npm":
		r.err = im.copyNpm(ctx, m, c, &r)
	case "pypi":
		r.err = im.copyPypi(ctx, m, c, &r)
	case "raw":
		r.err = im.copyRaw(ctx, m, c, &r)
	case "helm":
		r.err = im.copyHelm(ctx, m, c, &r)
	case "apt":
		r.err = im.copyApt(ctx, m, c, &r)
	}
	return r
}

// Nexus listing.

func (im *importer) nexusRepos(ctx context.Context) ([]nexusRepo, error) {
	var repos []nexusRepo
	err := im.getJSON(ctx, im.src, im.fromAuth, im.source("/service/rest/v1/repositories", nil), &repos)
	var se *statusError
	if errors.As(err, &se) && (se.status == http.StatusUnauthorized || se.status == http.StatusForbidden) {
		return nil, fmt.Errorf("%w (check -from-user and its password)", err)
	}
	return repos, err
}

// components calls fn for every component of the Nexus repository, page by page.
func (im *importer) components(ctx context.Context, repo string, fn func(nexusComponent)) error {
	q := url.Values{"repository": {repo}}
	for {
		var page struct {
			Items             []nexusComponent `json:"items"`
			ContinuationToken string           `json:"continuationToken"`
		}
		if err := im.getJSON(ctx, im.src, im.fromAuth, im.source("/service/rest/v1/components", q), &page); err != nil {
			return fmt.Errorf("list components: %w", err)
		}
		for _, c := range page.Items {
			fn(c)
		}
		if page.ContinuationToken == "" {
			return nil
		}
		q.Set("continuationToken", page.ContinuationToken)
	}
}

// source is a URL under -from; target one under -to.
func (im *importer) source(p string, q url.Values) string { return join(im.from, p, q) }
func (im *importer) target(p string, q url.Values) string { return join(im.to, p, q) }

func join(base *url.URL, p string, q url.Values) string {
	u := *base
	u.Path = base.Path + p
	u.RawQuery = q.Encode()
	return u.String()
}

// download is the URL an asset is read from: its downloadUrl's path on the
// -from host (Nexus's base URL setting may name a host that is not
// reachable from here), or /repository/<repo>/<path> when there is none.
func (im *importer) download(m repoMap, a nexusAsset) string {
	if u, err := url.Parse(a.DownloadURL); err == nil && u.Path != "" {
		return join(im.from, strings.TrimPrefix(u.Path, im.from.Path), nil)
	}
	return im.source("/repository/"+m.nexus+"/"+strings.TrimPrefix(a.Path, "/"), nil)
}

// HTTP helpers.

type statusError struct {
	method, path string
	status       int
	body         string
}

func (e *statusError) Error() string {
	s := fmt.Sprintf("%s %s: HTTP %d", e.method, e.path, e.status)
	if e.body != "" {
		s += ": " + e.body
	}
	return s
}

func newStatusError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	return &statusError{resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, strings.Join(strings.Fields(string(b)), " ")}
}

// open makes a request and returns its response when the status is 2xx.
func open(ctx context.Context, c *http.Client, auth, method, u string, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, newStatusError(resp)
	}
	return resp, nil
}

func (im *importer) getJSON(ctx context.Context, c *http.Client, auth, u string, v any) error {
	resp, err := open(ctx, c, auth, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(v)
}

// exists reports whether the target answers HEAD p with 200 (404: no).
func (im *importer) exists(ctx context.Context, p string, hdr http.Header) (bool, error) {
	resp, err := open(ctx, im.dst, im.toAuth, http.MethodHead, im.target(p, nil), hdr)
	var se *statusError
	if errors.As(err, &se) && se.status == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	return true, nil
}

// send makes a write request to the target. The status is returned for 2xx
// and 409 (already there, which allow_once answers); anything else is an
// error naming the status.
func (im *importer) send(ctx context.Context, method, u string, body io.Reader, length int64, contentType string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, err
	}
	req.ContentLength = length
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if im.toAuth != "" {
		req.Header.Set("Authorization", im.toAuth)
	}
	resp, err := im.dst.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusConflict {
		return resp.StatusCode, newStatusError(resp)
	}
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// fetch downloads u to a temporary file, checks it against the sha256 Nexus
// reports (when it does) and returns the file rewound, with its size. The
// caller removes it with discard. A file, not a pipe: the checksum is
// verified before anything reaches the target, and the npm document needs
// the tarball twice.
func fetch(ctx context.Context, c *http.Client, auth, u, wantSHA256 string) (*os.File, int64, error) {
	resp, err := open(ctx, c, auth, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp("", "import-nexus-*")
	if err != nil {
		return nil, 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if err == nil && wantSHA256 != "" && !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), wantSHA256) {
		err = errors.New("downloaded content does not match the sha256 Nexus reports")
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		discard(f)
		return nil, 0, err
	}
	return f, n, nil
}

func discard(f *os.File) {
	f.Close()
	os.Remove(f.Name())
}

// copyFile copies one asset to the target path dst with a PUT of the file.
func (im *importer) copyFile(ctx context.Context, m repoMap, a nexusAsset, dst string, r *result) error {
	if im.skipExisting {
		ok, err := im.exists(ctx, dst, nil)
		if err != nil {
			return err
		}
		if ok {
			r.skipped++
			return nil
		}
	}
	if im.dryRun {
		r.files++
		r.bytes += a.FileSize
		return nil
	}
	f, size, err := fetch(ctx, im.src, im.fromAuth, im.download(m, a), a.Checksum.SHA256)
	if err != nil {
		return err
	}
	defer discard(f)
	status, err := im.send(ctx, http.MethodPut, im.target(dst, nil), f, size, a.ContentType)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		r.skipped++
		return nil
	}
	r.files++
	r.bytes += size
	return nil
}

// Formats.

// copyMaven PUTs every file of the component at its path. Checksum files and
// maven-metadata.xml are left out: Command Center generates them.
func (im *importer) copyMaven(ctx context.Context, m repoMap, c nexusComponent, r *result) error {
	for _, a := range c.Assets {
		p := strings.TrimPrefix(a.Path, "/")
		base := path.Base(p)
		if strings.HasPrefix(base, "maven-metadata.xml") || hasSuffix(p, ".sha1", ".md5", ".sha256", ".sha512") {
			continue
		}
		if err := im.copyFile(ctx, m, a, "/repository/"+m.cc+"/"+p, r); err != nil {
			return fmt.Errorf("%s: %w", base, err)
		}
	}
	return nil
}

func (im *importer) copyRaw(ctx context.Context, m repoMap, c nexusComponent, r *result) error {
	for _, a := range c.Assets {
		p := strings.TrimPrefix(a.Path, "/")
		if err := im.copyFile(ctx, m, a, "/repository/"+m.cc+"/"+p, r); err != nil {
			return fmt.Errorf("%s: %w", path.Base(p), err)
		}
	}
	return nil
}

// copyHelm PUTs the chart archive as <name>-<version>.tgz; .prov and other
// sidecars have no upload in Command Center.
func (im *importer) copyHelm(ctx context.Context, m repoMap, c nexusComponent, r *result) error {
	for _, a := range c.Assets {
		if !strings.HasSuffix(a.Path, ".tgz") {
			continue
		}
		file := path.Base(a.Path)
		if err := im.copyFile(ctx, m, a, "/repository/"+m.cc+"/"+file, r); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
	}
	return nil
}

// copyApt PUTs each package to the path Nexus has it at
// (pool/<letter>/<package>/<file>.deb): Command Center reads its control
// file and stores it at its own pool path, and generates dists/. A hosted
// apt repository answers the Nexus path too, so -skip-existing finds what
// an earlier run copied.
func (im *importer) copyApt(ctx context.Context, m repoMap, c nexusComponent, r *result) error {
	for _, a := range c.Assets {
		p := strings.TrimPrefix(a.Path, "/")
		if !strings.HasSuffix(p, ".deb") {
			continue
		}
		if !strings.HasPrefix(p, "pool/") {
			p = "pool/" + path.Base(p)
		}
		if err := im.copyFile(ctx, m, a, "/repository/"+m.cc+"/"+p, r); err != nil {
			return fmt.Errorf("%s: %w", path.Base(p), err)
		}
	}
	return nil
}

// copyPypi uploads each file as twine does: a multipart form with the
// metadata fields first and the file as "content". Command Center reads
// the file's own METADATA/PKG-INFO, so only name and version are sent.
func (im *importer) copyPypi(ctx context.Context, m repoMap, c nexusComponent, r *result) error {
	project := pypi.Normalize(c.Name)
	for _, a := range c.Assets {
		file := path.Base(a.Path)
		if im.skipExisting {
			ok, err := im.exists(ctx, "/repository/"+m.cc+"/packages/"+project+"/"+file, nil)
			if err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			if ok {
				r.skipped++
				continue
			}
		}
		if im.dryRun {
			r.files++
			r.bytes += a.FileSize
			continue
		}
		f, size, err := fetch(ctx, im.src, im.fromAuth, im.download(m, a), a.Checksum.SHA256)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		fields := [][2]string{{":action", "file_upload"}, {"protocol_version", "1"}, {"name", c.Name}, {"version", c.Version},
			{"filetype", map[bool]string{true: "bdist_wheel", false: "sdist"}[strings.HasSuffix(file, ".whl")]},
			{"sha256_digest", a.Checksum.SHA256}, {"md5_digest", a.Checksum.MD5}}
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		go func() {
			defer discard(f)
			pw.CloseWithError(func() error {
				for _, kv := range fields {
					if kv[1] == "" {
						continue
					}
					if err := mw.WriteField(kv[0], kv[1]); err != nil {
						return err
					}
				}
				part, err := mw.CreateFormFile("content", file)
				if err != nil {
					return err
				}
				if _, err := io.Copy(part, f); err != nil {
					return err
				}
				return mw.Close()
			}())
		}()
		status, err := im.send(ctx, http.MethodPost, im.target("/repository/"+m.cc+"/", nil), pr, -1, mw.FormDataContentType())
		pr.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if status == http.StatusConflict {
			r.skipped++
			continue
		}
		r.files++
		r.bytes += size
	}
	return nil
}

// copyNpm publishes the version as npm does: a document whose version
// manifest is the tarball's package.json and whose _attachments entry
// carries the tarball in base64, streamed from the temporary file.
func (im *importer) copyNpm(ctx context.Context, m repoMap, c nexusComponent, r *result) error {
	var tgz *nexusAsset
	for i := range c.Assets {
		if strings.HasSuffix(c.Assets[i].Path, ".tgz") {
			tgz = &c.Assets[i]
		}
	}
	if tgz == nil {
		return errors.New("no tarball asset")
	}
	name, version := c.Name, c.Version
	if im.skipExisting {
		ok, err := im.exists(ctx, "/repository/"+m.cc+"/"+name+"/"+version, nil)
		if err != nil {
			return err
		}
		if ok {
			r.skipped++
			return nil
		}
	}
	if im.dryRun {
		r.files++
		r.bytes += tgz.FileSize
		return nil
	}
	f, size, err := fetch(ctx, im.src, im.fromAuth, im.download(m, *tgz), tgz.Checksum.SHA256)
	if err != nil {
		return err
	}
	defer discard(f)
	pkg, err := packageJSON(f)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	file := npm.TarballFile(name, version)
	dist := map[string]any{"tarball": im.target("/repository/"+m.cc+"/"+name+"/-/"+file, nil)}
	if tgz.Checksum.SHA1 != "" {
		dist["shasum"] = tgz.Checksum.SHA1
	}
	pkg["name"], pkg["version"], pkg["dist"] = name, version, dist
	head, err := json.Marshal(map[string]any{"_id": name, "name": name, "versions": map[string]any{version: pkg}})
	if err != nil {
		return err
	}
	fileKey, _ := json.Marshal(file)
	prefix := string(head[:len(head)-1]) + `,"_attachments":{` + string(fileKey) + `:{"content_type":"application/octet-stream","length":` +
		strconv.FormatInt(size, 10) + `,"data":"`
	const suffix = `"}}}`
	pr, pw := io.Pipe()
	go func() {
		enc := base64.NewEncoder(base64.StdEncoding, pw)
		_, err := io.Copy(enc, f)
		if err == nil {
			err = enc.Close()
		}
		pw.CloseWithError(err)
	}()
	defer pr.Close()
	body := io.MultiReader(strings.NewReader(prefix), pr, strings.NewReader(suffix))
	length := int64(len(prefix)) + int64(base64.StdEncoding.EncodedLen(int(size))) + int64(len(suffix))
	status, err := im.send(ctx, http.MethodPut, im.target("/repository/"+m.cc+"/"+name, nil), body, length, "application/json")
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		r.skipped++
		return nil
	}
	r.files++
	r.bytes += size
	return nil
}

// packageJSON returns the package.json of an npm tarball (the entry under
// its top directory, as npm extracts it).
func packageJSON(f io.Reader) (map[string]any, error) {
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("tarball: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("tarball has no package.json in its top directory")
		}
		if err != nil {
			return nil, fmt.Errorf("tarball: %w", err)
		}
		if _, rest, ok := strings.Cut(h.Name, "/"); !ok || path.Clean("/"+rest) != "/package.json" || h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return nil, fmt.Errorf("tarball: %w", err)
		}
		var pkg map[string]any
		if err := json.Unmarshal(bytes.TrimPrefix(b, []byte("\uFEFF")), &pkg); err != nil || pkg == nil {
			return nil, fmt.Errorf("tarball: package.json: %v", err)
		}
		return pkg, nil
	}
}

// copyDistTags sets the package's dist-tags on the target as the Nexus
// packument has them, once every version is there.
func (im *importer) copyDistTags(ctx context.Context, m repoMap, name string) error {
	var doc struct {
		DistTags map[string]string `json:"dist-tags"`
	}
	if err := im.getJSON(ctx, im.src, im.fromAuth, im.source("/repository/"+m.nexus+"/"+name, nil), &doc); err != nil {
		return err
	}
	for _, tag := range sortedKeys(doc.DistTags) {
		b, _ := json.Marshal(doc.DistTags[tag])
		u := im.target("/repository/"+m.cc+"/-/package/"+name+"/dist-tags/"+tag, nil)
		if _, err := im.send(ctx, http.MethodPut, u, bytes.NewReader(b), int64(len(b)), "application/json"); err != nil {
			return fmt.Errorf("tag %s: %w", tag, err)
		}
	}
	return nil
}

// Docker: the Nexus docker connector (-docker-from) is read with the
// registry API and the target written with it: blobs that the target does
// not have are uploaded, child manifests of an index first, then the
// manifest under its tag.

var manifestAccept = http.Header{"Accept": {strings.Join([]string{
	"application/vnd.oci.image.index.v1+json", "application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.docker.distribution.manifest.v2+json",
}, ", ")}}

func (im *importer) dockerTags(ctx context.Context, image string) ([]string, error) {
	var list struct {
		Tags []string `json:"tags"`
	}
	if err := im.getJSON(ctx, im.dockerSrc, im.fromAuth, join(im.dockerFrom, "/v2/"+image+"/tags/list", nil), &list); err != nil {
		return nil, fmt.Errorf("tags of %s: %w", image, err)
	}
	return list.Tags, nil
}

// readManifest reads a manifest from the source registry: its bytes, media
// type and digest.
func (im *importer) readManifest(ctx context.Context, image, ref string) ([]byte, string, string, error) {
	resp, err := open(ctx, im.dockerSrc, im.fromAuth, http.MethodGet, join(im.dockerFrom, "/v2/"+image+"/manifests/"+ref, nil), manifestAccept)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", "", err
	}
	ct, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	sum := sha256.Sum256(body)
	return body, strings.TrimSpace(ct), "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (im *importer) copyDockerTag(ctx context.Context, cc, image, tag string) (r result) {
	body, ct, digest, err := im.readManifest(ctx, image, tag)
	if err != nil {
		return result{err: err}
	}
	if im.skipExisting {
		resp, err := open(ctx, im.dst, im.toAuth, http.MethodHead, im.target("/v2/"+cc+"/"+image+"/manifests/"+tag, nil), manifestAccept)
		var se *statusError
		if err != nil && !(errors.As(err, &se) && se.status == http.StatusNotFound) {
			return result{err: err}
		}
		if err == nil {
			resp.Body.Close()
			if resp.Header.Get("Docker-Content-Digest") == digest {
				r.skipped++
				return r
			}
		}
	}
	r.err = im.copyManifest(ctx, cc, image, tag, body, ct, &r)
	return r
}

func (im *importer) copyManifest(ctx context.Context, cc, image, ref string, body []byte, ct string, r *result) error {
	var m struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
		Config *struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return fmt.Errorf("manifest %s: %w", ref, err)
	}
	if m.MediaType != "" {
		ct = m.MediaType
	}
	if ct == "" {
		return fmt.Errorf("manifest %s has no media type", ref)
	}
	byDigest := strings.HasPrefix(ref, "sha256:")
	if byDigest && im.skipExisting {
		ok, err := im.exists(ctx, "/v2/"+cc+"/"+image+"/manifests/"+ref, manifestAccept)
		if err != nil {
			return err
		}
		if ok {
			r.skipped++
			return nil
		}
	}
	for _, child := range m.Manifests {
		cb, cct, _, err := im.readManifest(ctx, image, child.Digest)
		if err != nil {
			return err
		}
		if err := im.copyManifest(ctx, cc, image, child.Digest, cb, cct, r); err != nil {
			return err
		}
	}
	blobs := m.Layers
	if m.Config != nil {
		blobs = append(blobs, *m.Config)
	}
	for _, b := range blobs {
		if err := im.copyBlob(ctx, cc, image, b.Digest, b.Size, r); err != nil {
			return err
		}
	}
	if im.dryRun {
		r.files++
		r.bytes += int64(len(body))
		return nil
	}
	u := im.target("/v2/"+cc+"/"+image+"/manifests/"+ref, nil)
	status, err := im.send(ctx, http.MethodPut, u, bytes.NewReader(body), int64(len(body)), ct)
	if err != nil {
		return fmt.Errorf("manifest %s: %w", ref, err)
	}
	if status == http.StatusConflict { // the tag is held by another manifest (allow_once)
		r.skipped++
		return nil
	}
	r.files++
	r.bytes += int64(len(body))
	return nil
}

// copyBlob uploads a blob the target does not have, in one request
// (POST ?digest=), streamed from the source registry.
func (im *importer) copyBlob(ctx context.Context, cc, image, digest string, size int64, r *result) error {
	ok, err := im.exists(ctx, "/v2/"+cc+"/"+image+"/blobs/"+digest, nil)
	if err != nil {
		return err
	}
	if ok {
		r.skipped++
		return nil
	}
	if im.dryRun {
		r.files++
		r.bytes += size
		return nil
	}
	resp, err := open(ctx, im.dockerSrc, im.fromAuth, http.MethodGet, join(im.dockerFrom, "/v2/"+image+"/blobs/"+digest, nil), nil)
	if err != nil {
		return fmt.Errorf("blob %s: %w", digest, err)
	}
	defer resp.Body.Close()
	u := im.target("/v2/"+cc+"/"+image+"/blobs/uploads/", url.Values{"digest": {digest}})
	if _, err := im.send(ctx, http.MethodPost, u, resp.Body, resp.ContentLength, "application/octet-stream"); err != nil {
		return fmt.Errorf("blob %s: %w", digest, err)
	}
	r.files++
	r.bytes += max(resp.ContentLength, 0)
	return nil
}

func hasSuffix(s string, suffixes ...string) bool {
	for _, x := range suffixes {
		if strings.HasSuffix(s, x) {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
