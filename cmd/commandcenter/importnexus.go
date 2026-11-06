package main

import (
	"archive/tar"
	"bytes"
	"cmp"
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
	"os/signal"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"commandcenter/internal/component"
	"commandcenter/internal/format/gitlfs"
	"commandcenter/internal/format/npm"
	"commandcenter/internal/format/pypi"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

const importUsage = "usage: commandcenter import-nexus -from NEXUS_URL -from-user NAME -from-password-file FILE -to CC_URL -to-user NAME " +
	"-to-token-file FILE (-repo NEXUS_REPO=CC_REPO ... | -all | -config | -config-only) [-update] [-passwords-out FILE] [-docker-from URL] " +
	"[-dry-run] [-concurrency N] [-skip-existing=false] [-insecure] [-ca PEM] (NEXUS_PASSWORD and COMMANDCENTER_TOKEN stand in for the files)"

// nexusFormats maps a Nexus format to the Command Center format the import
// writes with; other formats are reported as unsupported.
var nexusFormats = map[string]string{"maven2": "maven", "npm": "npm", "pypi": "pypi", "raw": "raw", "helm": "helm", "docker": "docker", "apt": "apt",
	"yum": "yum", "nuget": "nuget", "rubygems": "rubygems", "r": "r", "gitlfs": "gitlfs", "go": "go", "conda": "conda"}

// notCopied are the hosted formats both servers have whose content is not
// copied, and what to do instead: no public source says how a current Nexus
// lays them out, and a layout is not guessed. Their components are listed
// and each reported with this reason.
var notCopied = map[string]string{
	"conan":     "not copied: no public source says how a current Nexus lays out hosted Conan; move it with the client (conan download, then conan upload)",
	"cargo":     "not copied: no public source says how a current Nexus lays out hosted Cargo; publish it again with cargo publish",
	"terraform": "not copied: no public source says how a current Nexus lays out hosted Terraform; upload it again (see the guide's Terraform section)",
	"swift":     "not copied: no public source says how a current Nexus lays out hosted Swift; publish it again with swift package-registry publish",
}

// Nexus REST API v1 documents, the fields the import reads.
type nexusRepo struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Type   string `json:"type"`
}

type nexusAsset struct {
	DownloadURL  string `json:"downloadUrl"`
	Path         string `json:"path"`
	ContentType  string `json:"contentType"`
	FileSize     int64  `json:"fileSize"` // Nexus 3.47+; 0 before (dry-run sizes only)
	BlobCreated  string `json:"blobCreated"`
	LastModified string `json:"lastModified"`
	Checksum     struct {
		SHA1   string `json:"sha1"`
		SHA256 string `json:"sha256"`
		MD5    string `json:"md5"`
	} `json:"checksum"`
}

type nexusComponent struct {
	Group   string       `json:"group"`
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
	// statesTime says the target takes publish times from this user
	// (/-/api/me): uploads then state the time Nexus has.
	statesTime bool
	undated    *atomic.Int64 // files sent with no time, as Nexus reported none that reads as one
	// targetHas reports, after -config, whether Command Center has the
	// Nexus repository of that name (in a dry run: would have it); nil
	// without -config.
	targetHas func(name string) bool
	mig       *migration // nil without -config

	mu  *sync.Mutex
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
	cfg := fs.Bool("config", false, "first make the Nexus configuration on Command Center (blob stores, policies, repositories, roles, users, settings), "+
		"then copy the content: every hosted repository unless -repo names some. Both users must be administrators, "+
		"and -to-token-file must hold the password: an API token may not change the configuration")
	cfgOnly := fs.Bool("config-only", false, "make the configuration and copy no content")
	update := fs.Bool("update", false, "with -config: change what Command Center has when it differs from Nexus (default: report it and leave it)")
	pwOut := fs.String("passwords-out", "", "with -config: file (mode 0600) that receives the random passwords of the users made; without it nobody learns them")
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
	config := *cfg || *cfgOnly
	switch {
	case *cfgOnly && (len(maps) > 0 || *all):
		return errors.New("-config-only copies no content: leave out -repo and -all, or use -config")
	case !config && (*update || *pwOut != ""):
		return errors.New("-update and -passwords-out go with -config or -config-only")
	case *cfg && len(maps) == 0:
		*all = true
	case len(maps) == 0 && !*all && !config:
		return errors.New("-repo, -all or -config is required; " + importUsage)
	}
	im := &importer{dryRun: *dryRun, skipExisting: *skip, concurrency: max(*conc, 1), undated: &atomic.Int64{}, mu: &sync.Mutex{}, out: stdout}
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
	// An interrupt ends the copy and still closes what it opened; a second
	// one kills the tool, and the next run closes it (openMarker).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)
	var mig *migration
	if config {
		if mig, err = im.migrateConfig(ctx, *update, *pwOut); err != nil {
			return err
		}
		im.mig = mig
		if repos := mig.have["repository"]; repos != nil { // nil: the repositories could not be read
			im.targetHas = func(name string) bool { return repos[name] }
		}
	}
	failed := 0
	if !*cfgOnly {
		var me struct {
			StatesPublishTime bool `json:"statesPublishTime"`
		}
		// A failure says no; the uploads then report what is wrong.
		if im.call(ctx, im.dst, im.toAuth, http.MethodGet, im.to.String()+native+"/me", nil, nil, &me) == nil {
			im.statesTime = me.StatesPublishTime
		}
		failed, err = im.run(ctx, maps, *all)
	}
	if mig != nil {
		failed += mig.finish()
	}
	if err == nil && failed > 0 {
		err = fmt.Errorf("%d errors (see above)", failed)
	}
	return err
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
	im.printf("%s\t%s\t%s\t%s\t%d\t%d\t%s\n", repo, format, name, version, r.files, r.bytes, strings.ReplaceAll(status, "\n", "; "))
}

// run copies the content and returns how many copies failed; the error is
// one that stopped it.
func (im *importer) run(ctx context.Context, maps []repoMap, all bool) (int, error) {
	repos, err := im.nexusRepos(ctx)
	if err != nil {
		return 0, fmt.Errorf("list Nexus repositories: %w", err)
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
		if ctx.Err() != nil {
			im.printf("interrupted: the repositories from %s on were not copied\n", m.nexus)
			failed++
			break
		}
		r, ok := byName[m.nexus]
		format := nexusFormats[r.Format]
		if notCopied[r.Format] != "" {
			format = r.Format
		}
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
		case im.targetHas != nil && !im.targetHas(m.cc) && m.nexus == m.cc:
			im.printf("%s: skipped (Command Center has no such repository: see its configuration line)\n", m.nexus)
			continue
		case im.mig != nil && im.mig.sourceOffline[m.nexus]:
			im.printf("%s: error: it is offline on the Nexus server, which serves no file of it: put it online there, run again with -update, then take it offline on both\n", m.nexus)
			failed++
			continue
		}
		t, err := im.copySealed(ctx, m, format)
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
	if im.statesTime {
		im.printf("publish times: kept for every format that records one (docker has none)")
		if n := im.undated.Load(); n > 0 {
			im.printf("; %d files carry the time of this copy: Nexus reports none for them", n)
		}
		im.printf("\n")
	} else {
		im.printf("publish times: NOT kept: -to-user is not an administrator signed in with a password, so every version carries the time of this copy " +
			"(yarn 4 refuses a version younger than a day by default, and cleanup's age rules count from today)\n")
	}
	if im.dryRun {
		im.printf("dry run: nothing was written\n")
	}
	return failed, nil
}

// copySealed copies a repository the target has read-only as Nexus does
// (seal), or simply copies. Such a repository stands closed from the moment
// it is made and is opened for its own copy alone, when a dry pass finds
// something missing, so that a run has at most one open at a time and a run
// with nothing to do opens none. A marker on the target, set before it
// opens and removed once it is closed, lets the next run finish what a
// killed one left open.
func (im *importer) copySealed(ctx context.Context, m repoMap, format string) (tally, error) {
	var s *seal
	if im.mig != nil && m.nexus == m.cc {
		s = im.mig.seals[m.cc]
	}
	if s == nil {
		return im.copyRepo(ctx, m, format)
	}
	if im.dryRun {
		im.mig.line("repository", m.cc, "would stay read-only but for its copy (as Nexus has it)", nil)
		return im.copyRepo(ctx, m, format)
	}
	if !s.open {
		probe := *im
		probe.dryRun, probe.out = true, io.Discard
		t, err := probe.copyRepo(ctx, m, format)
		if err != nil || t.copied == 0 {
			return im.copyRepo(ctx, m, format) // nothing to copy: it stays closed, and the lines say what is there
		}
		if err := im.mig.open(m.cc, s); err != nil {
			im.mig.n.failed++
			im.mig.line("repository", m.cc, "error: not opened for its copy: "+err.Error(), nil)
			return im.copyRepo(ctx, m, format) // the refusals say the rest
		}
	}
	t, err := im.copyRepo(ctx, m, format)
	im.mig.close(m.cc, s)
	return t, err
}

// copyRepo copies the components of one repository, concurrency at a time.
func (im *importer) copyRepo(ctx context.Context, m repoMap, format string) (tally, error) {
	var (
		mu       sync.Mutex
		t        tally
		wg       sync.WaitGroup
		sem      = make(chan struct{}, im.concurrency)
		packages = map[string][]nexusComponent{} // npm: a package at a time, its document read once
		images   = map[string]bool{}             // docker: every tag is read from the connector
	)
	emit := func(name, version string, r result) {
		im.report(m.nexus, format, name, version, r)
		mu.Lock()
		t.add(r)
		mu.Unlock()
	}
	run := func(fn func()) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn()
		}()
	}
	work := func(name, version string, fn func() result) {
		run(func() { emit(name, version, fn()) })
	}
	list := im.components
	if format == "gitlfs" {
		list = im.lfsObjects
	}
	err := list(ctx, m.nexus, func(c nexusComponent) {
		switch format {
		case "docker":
			images[c.Name] = true
			return
		case "npm":
			packages[npmName(c)] = append(packages[npmName(c)], c)
			return
		}
		work(c.Name, c.Version, func() result { return im.copyComponent(ctx, m, format, c) })
	})
	if err == nil && format == "yum" {
		err = im.yumGroups(ctx, m, work)
	}
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
	for _, name := range sortedKeys(packages) {
		run(func() {
			im.copyNpmPackage(ctx, m, name, packages[name], emit, func(err error) {
				im.printf("%s\tnpm\t%s\tdist-tags\t0\t0\terror: %v\n", m.nexus, name, err)
				mu.Lock()
				t.errors++
				mu.Unlock()
			})
		})
	}
	wg.Wait()
	return t, nil
}

func (im *importer) copyComponent(ctx context.Context, m repoMap, format string, c nexusComponent) (r result) {
	suffix := func(ext ...string) func(string) bool {
		return func(p string) bool { return hasSuffix(p, ext...) }
	}
	switch format {
	case "maven":
		r.err = im.copySame(ctx, m, c, &r, func(p string) bool { // checksums and metadata: Command Center makes its own
			return !strings.HasPrefix(path.Base(p), "maven-metadata.xml") && !hasSuffix(p, ".sha1", ".md5", ".sha256", ".sha512")
		})
	case "pypi":
		r.err = im.copyPypi(ctx, m, c, &r)
	case "raw":
		r.err = im.copySame(ctx, m, c, &r, suffix(""))
	case "helm":
		r.err = im.copyHelm(ctx, m, c, &r)
	case "apt":
		r.err = im.copyApt(ctx, m, c, &r)
	case "yum": // repodata/ is the server's to make
		r.err = im.copySame(ctx, m, c, &r, func(p string) bool { return strings.HasSuffix(p, ".rpm") && !strings.Contains("/"+p, "/repodata/") })
	case "r": // so are the PACKAGES files, which have no component
		r.err = im.copySame(ctx, m, c, &r, suffix(".tar.gz", ".tgz", ".zip"))
	case "conda": // and repodata.json
		r.err = im.copySame(ctx, m, c, &r, suffix(".conda", ".tar.bz2"))
	case "nuget":
		r.err = im.copyNuGet(ctx, m, c, &r)
	case "rubygems":
		r.err = im.copyGem(ctx, m, c, &r)
	case "go":
		r.err = im.copyGo(ctx, m, c, &r)
	case "gitlfs":
		r.err = im.copyLFS(ctx, m, c, &r)
	default:
		r.err = errors.New(notCopied[format])
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

// listAll calls fn for every item of a paged list of the Nexus repository.
func listAll[T any](ctx context.Context, im *importer, p, repo string, fn func(T)) error {
	q := url.Values{"repository": {repo}}
	for {
		var page struct {
			Items             []T    `json:"items"`
			ContinuationToken string `json:"continuationToken"`
		}
		if err := im.getJSON(ctx, im.src, im.fromAuth, im.source(p, q), &page); err != nil {
			return fmt.Errorf("list %s: %w", path.Base(p), err)
		}
		for _, it := range page.Items {
			fn(it)
		}
		if page.ContinuationToken == "" {
			return nil
		}
		q.Set("continuationToken", page.ContinuationToken)
	}
}

// components calls fn for every component of the Nexus repository.
func (im *importer) components(ctx context.Context, repo string, fn func(nexusComponent)) error {
	return listAll(ctx, im, "/service/rest/v1/components", repo, fn)
}

// lfsObjects calls fn for every file of a Git LFS repository, each as a
// component: the listing of assets has them whether or not Nexus makes them
// components, which no public source says.
func (im *importer) lfsObjects(ctx context.Context, repo string, fn func(nexusComponent)) error {
	return listAll(ctx, im, "/service/rest/v1/assets", repo, func(a nexusAsset) {
		fn(nexusComponent{Name: path.Base(a.Path), Assets: []nexusAsset{a}})
	})
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

// duplicate reports whether a refused upload says the target has it already,
// each format in its own way. Any other refusal, a 409 among them (a chart
// of another name at that file name, a .mod that differs from its zip), is
// content the target does not take, and reported as such.
func duplicate(status int, msg string) bool {
	switch status {
	case http.StatusConflict: // helm, apt, yum, r, nuget, rubygems, conda, go
		return strings.Contains(msg, "already exists")
	case http.StatusBadRequest: // pypi; maven and raw under allow_once
		return strings.Contains(msg, "File already exists") || strings.Contains(msg, "does not allow updating assets")
	case http.StatusForbidden: // npm
		return strings.Contains(msg, "cannot publish over the previously published version")
	}
	return false
}

// send makes a write request to the target, stating the publish time when
// published is set. The status is returned for 2xx and, as 409, for what the
// target has already (duplicate); anything else is an error naming the status.
func (im *importer) send(ctx context.Context, method, u string, body io.Reader, length int64, contentType, published string) (int, error) {
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
	if published != "" {
		req.Header.Set(repo.PublishTimeHeader, published)
	}
	resp, err := im.dst.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		err := apiError(resp)
		var se *statusError
		if errors.As(err, &se) && duplicate(se.status, se.body) {
			return http.StatusConflict, nil
		}
		return resp.StatusCode, err
	}
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// published is the publish time an upload states: the first of times that is
// a time, Nexus's own record of the version before its file's dates. A
// target that takes none from this user is sent none.
func (im *importer) published(times ...string) string {
	if !im.statesTime {
		return ""
	}
	for _, t := range times {
		if _, err := time.Parse(time.RFC3339, t); err == nil {
			return t
		}
	}
	im.undated.Add(1)
	return ""
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
	return im.copyTo(ctx, m, a, dst, http.MethodPut, dst, r)
}

// copyTo copies one asset by sending the file with method to dst, unless the
// target has the path probe already.
func (im *importer) copyTo(ctx context.Context, m repoMap, a nexusAsset, probe, method, dst string, r *result) error {
	if im.skipExisting {
		ok, err := im.exists(ctx, probe, nil)
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
	status, err := im.send(ctx, method, im.target(dst, nil), f, size, a.ContentType, im.published(a.BlobCreated, a.LastModified))
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

// Formats. A file that fails does not stop the others of its component: the
// component's line names every one that failed.

// copySame PUTs each file keep takes to the path Nexus has it at, the path
// the format's clients use on both servers.
func (im *importer) copySame(ctx context.Context, m repoMap, c nexusComponent, r *result, keep func(p string) bool) error {
	var errs []error
	kept := 0
	for _, a := range c.Assets {
		p := strings.TrimPrefix(a.Path, "/")
		if !keep(p) {
			continue
		}
		kept++
		if err := im.copyFile(ctx, m, a, "/repository/"+m.cc+"/"+p, r); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path.Base(p), err))
		}
	}
	if kept == 0 {
		return errNoFile(c)
	}
	return errors.Join(errs...)
}

// errNoFile says a component's files are none the format's upload takes.
func errNoFile(c nexusComponent) error {
	return fmt.Errorf("none of its %d files is one this format uploads", len(c.Assets))
}

// copyHelm PUTs the chart as <name>-<version>.tgz, whatever file name Nexus
// stored it under, and then its provenance file: the target checks that one
// against the chart, and a pair that does not match leaves the chart there
// and is reported.
func (im *importer) copyHelm(ctx context.Context, m repoMap, c nexusComponent, r *result) error {
	file := c.Name + "-" + c.Version + ".tgz"
	var errs []error
	copied := 0
	for _, ext := range []string{".tgz", ".prov"} {
		for _, a := range c.Assets {
			if !strings.HasSuffix(a.Path, ext) {
				continue
			}
			copied++
			name := file
			if ext == ".prov" {
				name += ".prov"
			}
			if err := im.copyFile(ctx, m, a, "/repository/"+m.cc+"/"+name, r); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			}
		}
	}
	if copied == 0 {
		return errNoFile(c)
	}
	return errors.Join(errs...)
}

// copyNuGet pushes the package as the body of the publish URL: the server
// reads its id and version from the .nuspec inside, so its path on Nexus
// does not matter. No public source says what that path is: the .nupkg, or
// else a component's only file (the server refuses anything else). A symbol
// package is not sent: as a body it would be taken for a package.
func (im *importer) copyNuGet(ctx context.Context, m repoMap, c nexusComponent, r *result) error {
	var pkg *nexusAsset
	for i := range c.Assets {
		if strings.HasSuffix(strings.ToLower(c.Assets[i].Path), ".nupkg") {
			pkg = &c.Assets[i]
		}
	}
	switch {
	case pkg != nil:
	case len(c.Assets) == 1 && !strings.HasSuffix(strings.ToLower(c.Assets[0].Path), ".snupkg"):
		pkg = &c.Assets[0]
	case len(c.Assets) == 1:
		return errors.New("symbol packages (.snupkg) are not supported")
	default:
		return errNoFile(c)
	}
	id, version := strings.ToLower(c.Name), strings.ToLower(c.Version)
	probe := "/repository/" + m.cc + "/v3-flatcontainer/" + id + "/" + version + "/" + id + "." + version + ".nupkg"
	return im.copyTo(ctx, m, *pkg, probe, http.MethodPut, "/repository/"+m.cc+"/api/v2/package", r)
}

// copyGem pushes each .gem as `gem push` does; the server makes the indexes
// and the gemspecs.
func (im *importer) copyGem(ctx context.Context, m repoMap, c nexusComponent, r *result) error {
	var errs []error
	copied := 0
	for _, a := range c.Assets {
		if !strings.HasSuffix(a.Path, ".gem") {
			continue
		}
		copied++
		file := path.Base(a.Path)
		if err := im.copyTo(ctx, m, a, "/repository/"+m.cc+"/gems/"+file, http.MethodPost, "/repository/"+m.cc+"/api/v1/gems", r); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", file, err))
		}
	}
	if copied == 0 {
		return errNoFile(c)
	}
	return errors.Join(errs...)
}

// yumGroups copies the package group files of a yum repository, the one
// thing in repodata/ somebody uploaded: they are in no component. A group file
// in a repodata/ only under another name (repomd.xml may name it so) is not
// guessed at, and reported.
func (im *importer) yumGroups(ctx context.Context, m repoMap, work func(name, version string, fn func() result)) error {
	named, other := map[string]bool{}, map[string]string{}
	err := listAll(ctx, im, "/service/rest/v1/assets", m.nexus, func(a nexusAsset) {
		p := strings.TrimPrefix(a.Path, "/")
		switch {
		case component.YumComps(p):
			named[path.Dir(p)] = true
			work(p, "", func() (r result) {
				r.err = im.copyFile(ctx, m, a, "/repository/"+m.cc+"/"+p, &r)
				return r
			})
		case path.Base(path.Dir(p)) == "repodata" && hasSuffix(p, "comps.xml", "comps.xml.gz"):
			other[path.Dir(p)] = p
		}
	})
	for _, dir := range sortedKeys(other) {
		if !named[dir] {
			work(other[dir], "", func() result {
				return result{err: errors.New("not copied: a package group file under another name than comps.xml; upload it to " + dir + "/comps.xml")}
			})
		}
	}
	return err
}

// copyGo PUTs each version's .zip, after its .info, which Nexus makes on
// read: it holds the time go shows, and a zip that comes first would get one
// from the server. The .mod is not sent: the server derives it from the zip,
// and one that differs is refused.
func (im *importer) copyGo(ctx context.Context, m repoMap, c nexusComponent, r *result) error {
	var errs []error
	copied := 0
	for _, a := range c.Assets {
		stem, ok := strings.CutSuffix(strings.TrimPrefix(a.Path, "/"), ".zip")
		if !ok {
			continue
		}
		copied++
		info := nexusAsset{Path: stem + ".info", BlobCreated: a.BlobCreated, LastModified: a.LastModified}
		var se *statusError
		err := im.copyFile(ctx, m, info, "/repository/"+m.cc+"/"+info.Path, r)
		if err != nil && !(errors.As(err, &se) && se.method == http.MethodGet && se.status == http.StatusNotFound) {
			errs = append(errs, fmt.Errorf("%s: %w", path.Base(info.Path), err))
		}
		if err := im.copyFile(ctx, m, a, "/repository/"+m.cc+"/"+stem+".zip", r); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path.Base(stem)+".zip", err))
		}
	}
	if copied == 0 {
		return errNoFile(c)
	}
	return errors.Join(errs...)
}

// copyLFS PUTs a Git LFS object under its oid, the sha256 of its content,
// which the server checks: as Nexus reports it, or as the file is named.
func (im *importer) copyLFS(ctx context.Context, m repoMap, c nexusComponent, r *result) error {
	a := c.Assets[0]
	oid := strings.ToLower(a.Checksum.SHA256)
	if !gitlfs.ValidOID(oid) {
		oid = path.Base(a.Path)
	}
	if !gitlfs.ValidOID(oid) {
		return fmt.Errorf("%s: its sha256 is not reported and its name is not one", a.Path)
	}
	return im.copyFile(ctx, m, a, "/repository/"+m.cc+"/info/lfs/objects/"+oid, r)
}

// copyApt PUTs each package to the path Nexus has it at
// (pool/<letter>/<package>/<file>.deb): Command Center reads its control
// file and stores it at its own pool path, and generates dists/. A hosted
// apt repository answers the Nexus path too, so -skip-existing finds what
// an earlier run copied.
func (im *importer) copyApt(ctx context.Context, m repoMap, c nexusComponent, r *result) error {
	var errs []error
	copied := 0
	for _, a := range c.Assets {
		p := strings.TrimPrefix(a.Path, "/")
		if !strings.HasSuffix(p, ".deb") {
			continue
		}
		copied++
		if !strings.HasPrefix(p, "pool/") {
			p = "pool/" + path.Base(p)
		}
		if err := im.copyFile(ctx, m, a, "/repository/"+m.cc+"/"+p, r); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path.Base(p), err))
		}
	}
	if copied == 0 {
		return errNoFile(c)
	}
	return errors.Join(errs...)
}

// maxPypiSignature is what the target takes as a gpg_signature.
const maxPypiSignature = 64 << 10

// copyPypi uploads each file of the component, with its signature when
// Nexus holds one: `<file>.asc` is another asset of the component, and an
// upload is the only moment the target takes it. Its core metadata sidecar
// is the server's to make.
func (im *importer) copyPypi(ctx context.Context, m repoMap, c nexusComponent, r *result) error {
	project := pypi.Normalize(c.Name)
	sigs := map[string]nexusAsset{}
	for _, a := range c.Assets {
		if file, ok := strings.CutSuffix(path.Base(a.Path), ".asc"); ok {
			sigs[file] = a
		}
	}
	var errs []error
	copied := 0
	for _, a := range c.Assets {
		file := path.Base(a.Path)
		if hasSuffix(file, ".asc", ".metadata") {
			continue
		}
		copied++
		sig, signed := sigs[file]
		delete(sigs, file)
		if err := im.copyPypiFile(ctx, m, c, a, sig, signed, project, r); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", file, err))
		}
	}
	for _, file := range sortedKeys(sigs) {
		errs = append(errs, fmt.Errorf("%s.asc: its file is not in this component", file))
	}
	if copied == 0 && len(errs) == 0 {
		return errNoFile(c)
	}
	return errors.Join(errs...)
}

// copyPypiFile uploads one file as twine does: a multipart form with the
// metadata fields first and the file as "content". Command Center reads the
// file's own METADATA/PKG-INFO, so only name and version are sent. A signature
// the target would refuse is left out, and reported: the file goes without it.
func (im *importer) copyPypiFile(ctx context.Context, m repoMap, c nexusComponent, a, sig nexusAsset, signed bool, project string, r *result) error {
	file := path.Base(a.Path)
	base := "/repository/" + m.cc + "/packages/" + project + "/" + file
	if im.skipExisting {
		ok, err := im.exists(ctx, base, nil)
		if err != nil {
			return err
		}
		if ok {
			r.skipped++
			if signed {
				if has, err := im.exists(ctx, base+".asc", nil); err == nil && !has {
					return errors.New("the target has the file without its signature, which can be added only by replacing the file (writePolicy allow, -skip-existing=false)")
				}
			}
			return nil
		}
	}
	if im.dryRun {
		if err := pypi.CheckUpload(c.Name, c.Version, file); err != nil {
			return fmt.Errorf("would be refused: %w", err)
		}
		r.files++
		r.bytes += a.FileSize
		return nil
	}
	var signature []byte
	var left error
	if signed {
		f, size, err := fetch(ctx, im.src, im.fromAuth, im.download(m, sig), sig.Checksum.SHA256)
		if err != nil { // not without it: a later run could not add it
			return fmt.Errorf("its signature: %w", err)
		}
		defer discard(f)
		if size > maxPypiSignature {
			left = fmt.Errorf("left out %s.asc: over %d KiB", file, maxPypiSignature>>10)
		} else if signature, err = io.ReadAll(f); err != nil {
			return fmt.Errorf("its signature: %w", err)
		} else if !bytes.HasPrefix(bytes.TrimSpace(signature), []byte("-----BEGIN PGP SIGNATURE-----")) {
			signature, left = nil, fmt.Errorf("left out %s.asc: not an ASCII-armored signature", file)
		}
	}
	f, size, err := fetch(ctx, im.src, im.fromAuth, im.download(m, a), a.Checksum.SHA256)
	if err != nil {
		return err
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
			if signature != nil {
				part, err := mw.CreateFormFile("gpg_signature", file+".asc")
				if err == nil {
					_, err = part.Write(signature)
				}
				if err != nil {
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
	status, err := im.send(ctx, http.MethodPost, im.target("/repository/"+m.cc+"/", nil), pr, -1, mw.FormDataContentType(), im.published(a.BlobCreated, a.LastModified))
	pr.Close()
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		r.skipped++
		return left
	}
	r.files++
	r.bytes += size
	return left
}

// npmName is a component's package name: Nexus has a scope in its group,
// without the "@".
func npmName(c nexusComponent) string {
	if c.Group != "" {
		return "@" + strings.TrimPrefix(c.Group, "@") + "/" + c.Name
	}
	return c.Name
}

// npmDocument is what the import reads of a package's document on Nexus.
type npmDocument struct {
	DistTags map[string]string         `json:"dist-tags"`
	Versions map[string]map[string]any `json:"versions"`
	Time     map[string]string         `json:"time"`
}

// copyNpmPackage copies the versions of one package, each with what was
// published of it in the Nexus document, then its dist-tags. A document that
// cannot be read leaves each version what its tarball says, and no tags
// (tagErr says so). In a dry run it names what the target's publish would
// refuse, as far as that shows without the tarball.
func (im *importer) copyNpmPackage(ctx context.Context, m repoMap, name string, comps []nexusComponent, emit func(name, version string, r result), tagErr func(error)) {
	var doc npmDocument
	docErr := im.getJSON(ctx, im.src, im.fromAuth, im.source("/repository/"+m.nexus+"/"+name, nil), &doc)
	known := true // a name the target has is not checked as a new one
	if im.dryRun && npm.CheckNewName(name) != nil {
		known, _ = im.exists(ctx, "/repository/"+m.cc+"/"+name, nil)
	}
	for _, c := range comps {
		var r result
		if im.dryRun {
			r.err = npmRefusal(name, c.Version, known)
		}
		if r.err == nil {
			r.err = im.copyNpm(ctx, m, name, c, doc, &r)
		}
		emit(name, c.Version, r)
	}
	switch {
	case docErr != nil:
		tagErr(fmt.Errorf("its document cannot be read, so versions carry what their tarballs say and it has no tags: %w", docErr))
	case im.dryRun:
		for _, tag := range sortedKeys(doc.DistTags) {
			if err := npm.CheckNewTag(tag); err != nil {
				var stored map[string]string // what the target has is not checked again
				im.getJSON(ctx, im.dst, im.toAuth, im.target("/repository/"+m.cc+"/-/package/"+name+"/dist-tags", nil), &stored)
				if _, ok := stored[tag]; !ok {
					tagErr(fmt.Errorf("tag %s would be refused: %w", tag, err))
				}
			}
		}
	default:
		for _, tag := range sortedKeys(doc.DistTags) {
			b, _ := json.Marshal(doc.DistTags[tag])
			u := im.target("/repository/"+m.cc+"/-/package/"+name+"/dist-tags/"+tag, nil)
			if _, err := im.send(ctx, http.MethodPut, u, bytes.NewReader(b), int64(len(b)), "application/json", ""); err != nil {
				tagErr(fmt.Errorf("tag %s: %w", tag, err))
			}
		}
	}
}

// npmRefusal is why the target's publish would refuse this version, as far
// as that shows without its tarball; nil for none.
func npmRefusal(name, version string, known bool) error {
	err := npm.CheckVersion(version)
	if !known {
		err = cmp.Or(npm.CheckNewName(name), err)
	}
	if err != nil {
		return fmt.Errorf("would be refused: %w", err)
	}
	return nil
}

// copyNpm publishes the version as npm does: a document whose version
// manifest is what Nexus has for it (else its tarball's package.json) and
// whose _attachments entry carries the tarball in base64, streamed from the
// temporary file. The server sets dist's tarball, shasum and integrity
// again, after checking the ones sent against the file.
func (im *importer) copyNpm(ctx context.Context, m repoMap, name string, c nexusComponent, doc npmDocument, r *result) error {
	var tgz *nexusAsset
	for i := range c.Assets {
		if strings.HasSuffix(c.Assets[i].Path, ".tgz") {
			tgz = &c.Assets[i]
		}
	}
	if tgz == nil {
		return errors.New("no tarball asset")
	}
	version := c.Version
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
	pkg := doc.Versions[version]
	if pkg == nil {
		if pkg, err = packageJSON(f); err != nil {
			return err
		}
		pkg["name"], pkg["version"] = name, version
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	file := npm.TarballFile(name, version)
	dist, _ := pkg["dist"].(map[string]any)
	if dist == nil {
		dist = map[string]any{}
		if tgz.Checksum.SHA1 != "" {
			dist["shasum"] = tgz.Checksum.SHA1
		}
	}
	dist["tarball"] = im.target("/repository/"+m.cc+"/"+name+"/-/"+file, nil)
	pkg["dist"] = dist
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
	status, err := im.send(ctx, http.MethodPut, im.target("/repository/"+m.cc+"/"+name, nil), body, length, "application/json", im.published(doc.Time[version], tgz.BlobCreated, tgz.LastModified))
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
	status, err := im.send(ctx, http.MethodPut, u, bytes.NewReader(body), int64(len(body)), ct, "")
	if err != nil {
		return fmt.Errorf("manifest %s: %w", ref, err)
	}
	if status == http.StatusConflict { // same bytes get a 201: this tag is another manifest's (allow_once)
		return fmt.Errorf("manifest %s: the tag is another manifest's on the target", ref)
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
	if _, err := im.send(ctx, http.MethodPost, u, resp.Body, resp.ContentLength, "application/octet-stream", ""); err != nil {
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
