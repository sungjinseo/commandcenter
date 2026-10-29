// Package app wires the Command Center components together.
package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/config"
	"commandcenter/internal/events"
	"commandcenter/internal/format/apt"
	"commandcenter/internal/format/cargo"
	"commandcenter/internal/format/cocoapods"
	"commandcenter/internal/format/composer"
	"commandcenter/internal/format/conan"
	"commandcenter/internal/format/conda"
	"commandcenter/internal/format/docker"
	"commandcenter/internal/format/gitlfs"
	"commandcenter/internal/format/golang"
	"commandcenter/internal/format/helm"
	"commandcenter/internal/format/huggingface"
	"commandcenter/internal/format/maven"
	"commandcenter/internal/format/npm"
	"commandcenter/internal/format/nuget"
	"commandcenter/internal/format/p2"
	"commandcenter/internal/format/pypi"
	"commandcenter/internal/format/r"
	"commandcenter/internal/format/raw"
	"commandcenter/internal/format/rubygems"
	"commandcenter/internal/format/swift"
	"commandcenter/internal/format/terraform"
	"commandcenter/internal/format/yum"
	"commandcenter/internal/logring"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/mail"
	"commandcenter/internal/meta"
	"commandcenter/internal/metrics"
	"commandcenter/internal/nexusapi"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
	"commandcenter/internal/server"
	"commandcenter/internal/tasks"
	"commandcenter/internal/transfer"
	"commandcenter/internal/ui"
	"commandcenter/internal/vuln"
	"commandcenter/internal/webhook"
)

type App struct {
	Config   *config.Config
	DB       *meta.DB
	Store    *repo.Store
	Auth     *auth.Authenticator
	Registry *repo.Registry
	Handler  http.Handler
	Metrics  *metrics.Recorder // download and request counters; flushed by Serve
	Mail     *mail.Sender      // failure and test e-mail; Close drains its queue
	Events   *events.Recorder  // activity events; written by a goroutine that Close stops
	Deleters *cleanup.Registry // per-format component deleters used by cleanup
	// DockerKey signs the Docker registry's and Conan's bearer tokens: on
	// PostgreSQL the key every node shares (meta.DB.TokenKey), on SQLite
	// random per process, so a restart only makes clients fetch new ones.
	DockerKey []byte
	Tasks     *tasks.Service      // scheduled tasks; the scheduler runs while Serve does
	Webhooks  *webhook.Dispatcher // webhook deliveries, sent while Serve runs; Close stops it before the database closes
	Vuln      *vuln.Service       // vulnerability data and the firewall; Close waits for its index build
	// Node is this process's id in the nodes table (hostname-xxxx, new for
	// every start), in every log record ("node") and in the metrics.
	Node   string
	member *meta.Member // the node's registration: Join (AsServer, or Serve) until Serve returns
	log    *slog.Logger
}

// Version is reported by the UI; cmd/commandcenter sets it before New.
var Version = "dev"

// maxConcurrentFetches bounds simultaneous remote downloads (spec §7).
const maxConcurrentFetches = 32

// maxConns bounds the connections served at once. When it is reached,
// server.LimitConns closes an idle or slow-header one to make room; only
// while all serve requests do new ones wait in the listen backlog. Measured,
// a connection costs 14-25 KiB (idle, sending headers, or stalled in a body)
// plus about 1.6 times the header bytes it sent, at most ~40 KiB with
// maxHeaderBytes: 1024 stay within ~40 MiB of the 80 MiB GOMEMLIMIT.
const maxConns = 1024

// maxHeaderBytes bounds a request's headers (431 above it). Go's default of
// 1 MiB lets one connection cost ~1.5 MiB while it sends them.
const maxHeaderBytes = 16 << 10

// Option adjusts how New builds the App.
type Option func(*options)

type options struct {
	logs     *logring.Ring
	logLevel *slog.LevelVar
	serve    bool
}

// AsServer registers the process as a node in New, right after the
// database upgrade and before anything starts (the webhook poller among
// them): on PostgreSQL New then refuses to run beside another live server
// (meta.GuardError). The serve command passes it; Serve registers when New
// did not, and the CLI commands that run beside a server do not register.
func AsServer() Option { return func(o *options) { o.serve = true } }

// WithLogLevel lets the UI's logging API change level, the root logger's
// level, at run time.
func WithLogLevel(level *slog.LevelVar) Option { return func(o *options) { o.logLevel = level } }

// WithLogRing serves the log viewer from ring, which the caller's logger feeds
// (see logring.NewHandler).
func WithLogRing(ring *logring.Ring) Option { return func(o *options) { o.logs = ring } }

func New(cfg *config.Config, log *slog.Logger, opts ...Option) (*App, error) {
	var o options
	for _, f := range opts {
		f(&o)
	}
	node := meta.NewNodeID()
	log = log.With("node", node)
	db, err := OpenDB(cfg)
	if err != nil {
		return nil, err
	}
	if err := useSecretKey(cfg, log, db); err != nil {
		db.Close()
		return nil, err
	}
	var member *meta.Member
	if o.serve {
		if member, err = join(context.Background(), db, node, log); err != nil {
			db.Close()
			return nil, err
		}
	} // db.Close leaves the registry on the error returns below
	blobs, err := openBlobStores(cfg, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	a, err := build(cfg, log, blobs, db, o.logs, o.logLevel, node)
	if err != nil {
		db.Close()
		return nil, err
	}
	a.member = member
	return a, nil
}

// join registers the process as node in the nodes table (meta.Join).
func join(ctx context.Context, db *meta.DB, node string, log *slog.Logger) (*meta.Member, error) {
	host, _ := os.Hostname()
	return db.Join(ctx, node, host, Version, log)
}

// databaseEnv holds the URL of the PostgreSQL metadata database, so that it
// can come from a Kubernetes Secret; database.url in the config file gives
// it instead.
const databaseEnv = "COMMANDCENTER_DATABASE_URL"

// OpenDB opens the metadata database: PostgreSQL at the URL in
// COMMANDCENTER_DATABASE_URL or database.url, else SQLite at
// <dataDir>/meta.db. Every command that uses the database opens it here.
// The URL is never logged or put in an error: it may hold a password.
func OpenDB(cfg *config.Config) (*meta.DB, error) {
	url := os.Getenv(databaseEnv)
	src := databaseEnv
	switch {
	case url != "" && cfg.Database.URL != "":
		return nil, fmt.Errorf("set %s or database.url, not both", databaseEnv)
	case url == "":
		url, src = cfg.Database.URL, "database.url"
	}
	target := filepath.Join(cfg.DataDir, "meta.db")
	if url != "" {
		if !meta.IsPostgresURL(url) {
			return nil, fmt.Errorf("%s must be a postgres:// or postgresql:// URL", src)
		}
		target = url
	}
	db, err := meta.Open(target)
	if err != nil {
		return nil, fmt.Errorf("open metadata db: %w", err)
	}
	if n := cfg.Database.MaxConns; n > 0 {
		db.SetMaxOpenConns(n)
	}
	return db, nil
}

// secretKeyEnv holds the key that encrypts the stored secrets; secretKeyFile
// in the config file names a file holding it instead (QA N-29).
const secretKeyEnv = "COMMANDCENTER_SECRET_KEY"

// useSecretKey gives db the secret key, if one is configured, checks that
// it is the key the database was started with (PostgreSQL: the same on
// every node, meta.DB.CheckSecretKey) and seals the secrets still stored in
// plain text. Without a key secrets stay plain text, as before, and one
// warning says so. The key is never logged.
func useSecretKey(cfg *config.Config, log *slog.Logger, db *meta.DB) error {
	key, err := secretKey(cfg)
	if err != nil {
		return err
	}
	if key != nil {
		if err := db.SetSecretKey(key); err != nil {
			return err
		}
	}
	if err := db.CheckSecretKey(context.Background()); err != nil {
		return err
	}
	if key == nil {
		log.Warn("no secret key is configured: the SMTP password, the LDAP bind password, proxy remoteAuth " +
			"credentials and, on PostgreSQL, the key that signs Docker and Conan tokens are stored in the database in plain text; set " +
			secretKeyEnv + " or secretKeyFile to encrypt them")
		return nil
	}
	n, err := db.SealSecrets(context.Background())
	if err != nil {
		return fmt.Errorf("stored secrets: %w", err)
	}
	if n > 0 {
		log.Info("encrypted the stored secrets that were plain text", "rows", n)
	}
	return nil
}

// tokenKey returns the key that signs the Docker and Conan bearer tokens:
// on PostgreSQL the one stored for every node, so a token one node issued
// verifies on another; on SQLite a random one.
func tokenKey(db *meta.DB) ([]byte, error) {
	if db.Driver() == "postgresql" {
		key, err := db.TokenKey(context.Background())
		if err != nil {
			return nil, fmt.Errorf("registry token key: %w", err)
		}
		return key, nil
	}
	key := make([]byte, 32)
	rand.Read(key)
	return key, nil
}

// secretKey returns the key in COMMANDCENTER_SECRET_KEY or in the file
// secretKeyFile names, or nil when neither is set.
func secretKey(cfg *config.Config) ([]byte, error) {
	val := os.Getenv(secretKeyEnv)
	switch {
	case val != "" && cfg.SecretKeyFile != "":
		return nil, fmt.Errorf("set %s or secretKeyFile, not both", secretKeyEnv)
	case cfg.SecretKeyFile != "":
		return readSecretKey(cfg.SecretKeyFile, "secretKeyFile")
	case val == "":
		return nil, nil
	}
	return decodeSecretKey(val, secretKeyEnv)
}

// readSecretKey reads the key in the file at path, which the setting or flag
// name gives.
func readSecretKey(path, name string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		// Without the path: it is the key itself when that was given
		// here by mistake instead of the file holding it.
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return nil, fmt.Errorf("%s: cannot read the file it names: %w", name, err)
	}
	return decodeSecretKey(string(b), name+" "+path)
}

// decodeSecretKey decodes the key val holds; src says where val is from.
func decodeSecretKey(val, src string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(val))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s must hold 32 random bytes in base64 (make them with: head -c 32 /dev/urandom | base64)", src)
	}
	return key, nil
}

// RotateSecretKey seals the stored secrets with the key in newKeyFile (the
// -new-key-file of "secret-key rotate") instead of the configured one, as
// meta.DB.RotateSecretKey does, and returns how many had the configured key
// and how many were plain text. The server must be stopped meanwhile and
// started with the new key afterwards: it keeps the key it started with.
func RotateSecretKey(cfg *config.Config, newKeyFile string) (resealed, plain int, err error) {
	newKey, err := readSecretKey(newKeyFile, "-new-key-file")
	if err != nil {
		return 0, 0, err
	}
	key, err := secretKey(cfg)
	if err != nil {
		return 0, 0, err
	}
	db, err := OpenDB(cfg)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	if key != nil {
		if err := db.SetSecretKey(key); err != nil {
			return 0, 0, err
		}
	}
	resealed, plain, err = db.RotateSecretKey(context.Background(), newKey)
	if err != nil {
		return 0, 0, fmt.Errorf("stored secrets (nothing was changed): %w", err)
	}
	return resealed, plain, nil
}

// openBlobStores seeds the default blob store (its tree stays at
// <dataDir>/blobs, so nothing moves; its row's path is refreshed to the
// current dataDir) and opens every stored store at repo.StoreRoot, returning
// the name→store map used for routing and garbage collection. The default
// store is always present.
func openBlobStores(cfg *config.Config, db *meta.DB) (map[string]*blob.Store, error) {
	ctx := context.Background()
	if err := db.SeedBlobStore(ctx, repo.DefaultStore, cfg.DataDir); err != nil {
		return nil, fmt.Errorf("seed default blob store: %w", err)
	}
	rows, err := db.ListBlobStores(ctx)
	if err != nil {
		return nil, fmt.Errorf("list blob stores: %w", err)
	}
	stores := make(map[string]*blob.Store, len(rows))
	var groups []meta.BlobStore // opened once their members are
	for _, bs := range rows {
		if bs.Type == meta.BlobStoreGroup {
			groups = append(groups, bs)
			continue
		}
		s, err := repo.OpenStore(cfg.DataDir, bs)
		if err != nil {
			return nil, fmt.Errorf("open blob store %q at %s: %w", bs.Name, repo.StoreRoot(cfg.DataDir, bs), err)
		}
		stores[bs.Name] = s
	}
	for _, bs := range groups {
		s, err := repo.OpenGroup(cfg.DataDir, bs, func(name string) *blob.Store { return stores[name] })
		if err != nil {
			return nil, fmt.Errorf("open blob store group %q: %w", bs.Name, err)
		}
		stores[bs.Name] = s
	}
	if stores[repo.DefaultStore] == nil { // defensive: the seed guarantees it
		s, err := blob.Open(cfg.DataDir)
		if err != nil {
			return nil, fmt.Errorf("open default blob store: %w", err)
		}
		stores[repo.DefaultStore] = s
	}
	return stores, nil
}

func build(cfg *config.Config, log *slog.Logger, blobs map[string]*blob.Store, db *meta.DB, logs *logring.Ring, logLevel *slog.LevelVar, node string) (*App, error) {
	hasStore := func(name string) bool { return blobs[name] != nil } // before the map is shared
	defs, err := loadRepositories(cfg, log, db, hasStore)
	if err != nil {
		return nil, err
	}
	store := &repo.Store{Blobs: blobs, DB: db, Now: time.Now}
	recorder := metrics.New(db)
	store.Downloads = recorder.Download
	fetcher := proxy.New(store, proxy.NewClient(), maxConcurrentFetches)
	fetcher.Stats = recorder
	resolver := &resolve.Resolver{Store: store, Fetcher: fetcher}

	authn, err := loadAccess(cfg, log, db)
	if err != nil {
		return nil, err
	}
	// baseURL and maxUploadMiB are read per request from the live snapshot.
	settings := authn.Snapshot().Settings()
	// The outbound HTTP settings and the trust store apply to every client
	// of the proxy package (and LDAP, SMTP) on every settings change.
	authn.OnSettings = proxy.SetSettings
	proxy.SetSettings(settings)
	baseURL := func() string { return strings.TrimRight(authn.Snapshot().Settings().BaseURL, "/") }
	goH, mavenH, rawH := golang.New(store, resolver), maven.New(store, resolver), raw.New(store, resolver)
	goH.Auth, mavenH.Auth, rawH.Auth = authn, authn, authn // overwrites need Edit
	npmH, pypiH := npm.New(store, resolver, authn, baseURL), pypi.New(store, fetcher, baseURL)
	cargoH := cargo.New(store, fetcher, authn, baseURL)
	helmH := helm.New(store, fetcher, authn)
	yumH := yum.New(store, resolver, authn)
	nugetH := nuget.New(store, fetcher, authn, baseURL)
	aptH := apt.New(store, resolver, authn)
	composerH := composer.New(store, fetcher, authn, baseURL)
	condaH := conda.New(store, resolver, authn)
	gemsH := rubygems.New(store, fetcher, authn)
	gitlfsH := gitlfs.New(store, authn, baseURL)
	rH := r.New(store, resolver, authn)
	p2H := p2.New(store, fetcher, baseURL) // proxy and group only: its proxies use the generic deleter
	swiftH := swift.New(store, fetcher, authn, baseURL)
	podsH := cocoapods.New(store, fetcher, baseURL)
	hfH := huggingface.New(store, fetcher)
	terraformH := terraform.New(store, fetcher, authn, baseURL)
	dockerKey, err := tokenKey(db)
	if err != nil {
		return nil, err
	}
	dockerH := docker.New(store, docker.NewFetcher(store, maxConcurrentFetches), authn, nil, baseURL, dockerKey) // Repos: the registry, below
	dockerH.MaxUpload = func() int64 { return int64(authn.Snapshot().Settings().MaxUploadMiB) << 20 }
	conanH := conan.New(store, conan.NewFetcher(store, maxConcurrentFetches), authn, baseURL, dockerKey)
	authn.Bearers = append(authn.Bearers, conanH.VerifyToken) // Conan login tokens
	handlers := map[string]repo.Handler{"go": goH, "maven": mavenH, "npm": npmH, "pypi": pypiH, "cargo": cargoH, "docker": dockerH, "raw": rawH, "helm": helmH, "nuget": nugetH, "yum": yumH, "apt": aptH, "composer": composerH, "conda": condaH, "rubygems": gemsH, "gitlfs": gitlfsH, "r": rH, "conan": conanH, "p2": p2H, "swift": swiftH, "cocoapods": podsH, "huggingface": hfH, "terraform": terraformH}
	// Hosted deletes go through the format handlers, which share their locks
	// with uploads; proxies of every format use the generic deleter.
	deleters := cleanup.NewRegistry(store)
	deleters.Register("go", "hosted", goH)
	deleters.Register("maven", "hosted", mavenH)
	deleters.Register("npm", "hosted", npmH)
	deleters.Register("pypi", "hosted", pypiH)
	deleters.Register("cargo", "hosted", cargoH)   // the UI's delete only: cleanup skips Cargo hosted
	deleters.Register("docker", "hosted", dockerH) // the UI's delete only: cleanup skips docker too
	deleters.Register("raw", "hosted", rawH)
	deleters.Register("helm", "hosted", helmH)
	deleters.Register("yum", "hosted", yumH)
	deleters.Register("nuget", "hosted", nugetH)
	deleters.Register("apt", "hosted", aptH)
	deleters.Register("composer", "hosted", composerH)
	deleters.Register("conda", "hosted", condaH)
	deleters.Register("rubygems", "hosted", gemsH)
	deleters.Register("gitlfs", "hosted", gitlfsH)
	deleters.Register("r", "hosted", rH)
	deleters.Register("conan", "hosted", conanH)
	deleters.Register("swift", "hosted", swiftH)
	deleters.Register("terraform", "hosted", terraformH)
	for _, r := range defs {
		if handlers[r.Format] == nil {
			return nil, fmt.Errorf("repository %q: format %q is not implemented yet", r.Name, r.Format)
		}
	}
	taskSvc := &tasks.Service{DB: db, Deleters: deleters, Maven: mavenH, Metrics: recorder, Log: log, Store: store,
		Rebuilders: map[string]repo.MetadataRebuilder{"apt": aptH, "yum": yumH, "helm": helmH, "conda": condaH}}
	if err := taskSvc.Seed(context.Background()); err != nil {
		return nil, err
	}
	if taskSvc.Transfer, err = newTransfer(cfg, db, store, handlers, map[string]transfer.RebuildFunc{
		"maven": func(ctx context.Context, rp *repo.Repository, logf func(string, ...any)) error {
			res, err := mavenH.RebuildMetadata(ctx, rp, 0, logf)
			if err == nil && (res.Failed > 0 || res.Truncated) {
				err = fmt.Errorf("%d maven-metadata.xml documents failed (truncated: %v): run repository.rebuild-metadata", res.Failed, res.Truncated)
			}
			return err
		},
		"npm": npmH.RebuildMetadata, "helm": helmH.RebuildMetadata, "yum": yumH.RebuildMetadata, "apt": aptH.RebuildMetadata,
		"conda": condaH.RebuildMetadata, "r": rH.RebuildMetadata, "rubygems": gemsH.RebuildMetadata,
	}); err != nil {
		return nil, err
	}

	// The registry's snapshot is the stored one, with each repository's gen.
	// On PostgreSQL other nodes write the database too (Cluster).
	registry := repo.NewRegistry(db, defs)
	registry.Cluster = db.Driver() == "postgresql"
	if err := registry.Reload(context.Background()); err != nil {
		return nil, fmt.Errorf("load repositories: %w", err)
	}
	store.CheckGen = true

	// Started only after the checks above can no longer fail, so an error return
	// never leaves the writer running against a database New is about to close.
	evts := events.New(db)
	// Every event goes through the webhook dispatcher to the recorder; Serve
	// starts sending the deliveries.
	hooks := &webhook.Dispatcher{DB: db, Next: evts, Repos: registry, Log: log}
	if err := hooks.Start(); err != nil {
		evts.Close()
		return nil, err
	}
	store.Events = hooks
	mailer := mail.New(log, hooks)
	taskSvc.Mail = mailer

	registry.Policies = cleanup.PolicyLookup(db)
	dockerH.Repos = registry
	// Routing rules: loaded once here, reloaded by the UI after a change,
	// and applied by both fetchers before any remote request.
	rules, err := db.ListRoutingRules(context.Background())
	if err == nil {
		err = registry.SetRoutingRules(rules)
	}
	if err != nil {
		return nil, fmt.Errorf("load routing rules: %w", err)
	}
	fetcher.RouteAllowed, dockerH.Fetcher.RouteAllowed = registry.RouteAllowed, registry.RouteAllowed
	for _, rp := range registry.Snapshot().Sorted {
		if rp.RoutingRule != "" && registry.Route(rp.RoutingRule) == nil {
			log.Warn("repository names a routing rule that does not exist; every path goes to the remote until the rule is created or the repository edited",
				"repository", rp.Name, "routingRule", rp.RoutingRule)
		}
	}
	// Route each repository's blob I/O to the store it names; an unknown or
	// empty name resolves to the default store.
	store.StoreFor = func(name string) string {
		if rp := registry.Get(name); rp != nil {
			return repo.StoreOf(rp.Repository)
		}
		return ""
	}
	registry.HasStore = store.Routable // a group member's blobs are routed by its group
	// A repository naming a missing store cannot read or write blobs; GC
	// keeps its blobs everywhere. Say so once per repository; do not refuse
	// to start.
	for _, name := range repo.OrphanRepos(registry.Snapshot(), store.HasStore) {
		log.Error("repository names a blob store that does not exist; its blobs cannot be read or written until the store is restored",
			"repository", name, "blobStore", repo.StoreOf(registry.Get(name).Repository))
	}
	if err := restoreMoves(db, store, registry, log); err != nil {
		hooks.Close()
		evts.Close()
		return nil, err
	}
	store.Live = func(rp *repo.Repository) bool {
		cur := registry.Get(rp.Name)
		return cur != nil && cur.Gen == rp.Gen
	}
	// A registry write on PostgreSQL validates against the blob stores and
	// users, roles and settings as every node left them.
	var storeErr string
	registry.Refresh = func(ctx context.Context) error {
		if err := store.SyncStores(ctx, cfg.DataDir); err != nil && err.Error() != storeErr {
			log.Error("could not open a blob store another node created or changed; it stays unusable on this node", "err", logsafe.Err(err))
			storeErr = err.Error() // once per failure: Refresh runs every few seconds; the registry lock is held
		} else if err == nil {
			storeErr = ""
		}
		return authn.Reload(ctx)
	}
	store.Lookup = registry.Get // contentDisposition on downloads
	authn.AnonymousRead = registry.AnonymousRead
	authn.RepoFormat = func(name string) string {
		if rp := registry.Get(name); rp != nil {
			return rp.Format
		}
		return ""
	}
	warnUnknownRoleRepos(log, authn.Snapshot(), registry.Snapshot())
	// A repository delete removed the roles' privileges on it (QA N-25). The
	// registry's write lock is still held, so the live snapshot drops them
	// before a repository of that name can be created again.
	registry.OnDelete = func(ctx context.Context, name string, roles []string) {
		store.EndMove(name) // its move row went with the repository
		if len(roles) == 0 {
			return
		}
		if err := authn.Reload(context.Background()); err != nil { // the delete is done, even if its client is gone
			log.Warn("could not reload users, roles and settings after a repository delete; the removed privileges stay in force until the next reload",
				"repository", name, "err", logsafe.Err(err))
		}
		db.Notify(context.Background(), "access") // the other nodes drop them too
		log.Warn("removed the privileges on a deleted repository from roles", "repository", name, "roles", strings.Join(roles, ", "))
		actor := auth.FromContext(ctx).Owner()
		for _, id := range roles {
			hooks.Emit("role.update", actor, name, id, "removed repo:"+name)
		}
	}

	authn.Log = log // redacted LDAP failures, sign-in throttling
	// Crowd goes through the proxy settings and the trust store, and may
	// reach the class of address its URL resolves to.
	authn.CrowdHTTP = func(u *url.URL) *http.Client { return proxy.NewClientFor(u, nil) }
	if logLevel != nil { // PUT /-/api/logging on another node
		db.Subscribe("loglevel", func(arg string) {
			var l slog.Level
			if l.UnmarshalText([]byte(arg)) == nil {
				logLevel.Set(l)
			}
		})
	}
	authn.Sessions = auth.NewSessions(db, authn.SessionIdle)
	if db.Driver() == "postgresql" { // the nodes share the sign-in throttle and the SSO identities
		authn.Shared = db
	}
	a := &App{Config: cfg, DB: db, Store: store, Auth: authn, Registry: registry, Metrics: recorder, Events: evts, Mail: mailer, Deleters: deleters, DockerKey: dockerKey, Webhooks: hooks, Node: node, log: log}
	taskSvc.Repos, taskSvc.Events = registry, hooks
	taskSvc.Transfer.Repos, taskSvc.Transfer.Events = registry, hooks
	taskSvc.GC = func(ctx context.Context, store string) (int, int64, error) {
		if store == "" {
			return a.GC(ctx, time.Hour)
		}
		return a.GCStore(ctx, store, time.Hour)
	}
	a.Tasks = taskSvc
	// Vulnerability data and the proxy firewall, asked before every file a
	// format handler serves.
	vulnSvc := &vuln.Service{DB: db, Repos: registry, Events: hooks, TempDir: cfg.DataDir, Log: log}
	store.Firewall = vulnSvc.Firewall
	taskSvc.Vuln, a.Vuln = vulnSvc, vulnSvc
	webUI := &ui.UI{
		Repos:    registry,
		Auth:     authn,
		DB:       db,
		Store:    store,
		BaseURL:  baseURL,
		Version:  Version,
		Node:     node,
		Started:  time.Now(),
		Tasks:    a.Tasks,
		Log:      log,
		Events:   hooks,
		Mail:     mailer,
		Logs:     logs,
		Metrics:  recorder,
		LogLevel: logLevel,

		Config:     cfg,
		DataDir:    cfg.DataDir,
		FetchSlots: fetcher.Slots,
		Remote:     fetcher.Remote,
		Deleters:   deleters,
		Webhooks:   hooks,
		Vuln:       vulnSvc,
	}
	// MaxUpload is only the fallback for a Server without Auth; the live
	// maxUploadMiB setting is what is enforced.
	nexus := &nexusapi.Handler{Repos: registry, Auth: authn, DB: db, BaseURL: baseURL, UI: webUI}
	// Staging selects components with the search API's matching and moves
	// them with the format handlers that can take one in.
	webUI.FindComponents = nexus.Find
	webUI.Importers = map[string]repo.ComponentImporter{"maven": mavenH, "npm": npmH, "raw": rawH, "pypi": pypiH, "nuget": nugetH, "helm": helmH, "yum": yumH}
	srv := &server.Server{Registry: registry, Handlers: handlers, Auth: authn, Log: log,
		MaxUpload: int64(settings.MaxUploadMiB) << 20, UI: webUI, Nexus: nexus, Requests: recorder.Request, Docker: dockerH}
	srv.TerraformDiscovery = func() []byte {
		return terraform.RootDiscovery(registry.Snapshot().Sorted, authn.Snapshot().Settings().TerraformDefaultRepository, baseURL())
	}
	a.Handler = srv.Handler()
	return a, nil
}

// loadRepositories imports cfg.Repositories into the database on the first
// start, then returns the stored definitions (plan 9 Decision 1).
func loadRepositories(cfg *config.Config, log *slog.Logger, db *meta.DB, hasStore func(string) bool) ([]config.Repository, error) {
	ctx := context.Background()
	done, err := db.RepositoriesImported(ctx)
	if err != nil {
		return nil, fmt.Errorf("load repositories: %w", err)
	}
	// The section only matters for the import; afterwards a problem in it
	// is part of the "ignored" warning, not a startup failure.
	if !done && cfg.RepositoriesErr != nil {
		return nil, cfg.RepositoriesErr
	}
	if !done {
		var errs []error
		for _, r := range cfg.Repositories {
			if r.Type != "group" && !hasStore(repo.StoreOf(r)) {
				errs = append(errs, fmt.Errorf("repository %q: unknown blob store %q", r.Name, repo.StoreOf(r)))
			}
		}
		if err := errors.Join(errs...); err != nil {
			return nil, fmt.Errorf("import repositories: %w", err)
		}
	}
	imported, err := db.ImportRepositories(ctx, cfg.Repositories)
	if err != nil {
		return nil, fmt.Errorf("import repositories: %w", err)
	}
	if imported {
		log.Info("imported repositories from the config file into the metadata database", "count", len(cfg.Repositories))
	} else if len(cfg.Repositories) > 0 || cfg.RepositoriesErr != nil {
		file := cfg.Path
		if file == "" {
			file = "config file"
		}
		attrs := []any{"file", file}
		if cfg.RepositoriesErr != nil {
			attrs = append(attrs, "sectionError", cfg.RepositoriesErr.Error())
		}
		log.Warn("the repositories section of "+file+" is ignored: repositories were imported on the first start "+
			"and are now managed in the web UI", attrs...)
	}
	defs, err := db.ListRepositories(ctx)
	if err != nil {
		return nil, fmt.Errorf("load repositories: %w", err)
	}
	var plain []string // proxies whose remote URL holds credentials the secret key does not seal
	// Older builds stored negative durations and groups listing a member
	// twice; serve them (0-length cache, members once) instead of refusing
	// to start. Only a create or update is held to the new rules (C1).
	for i := range defs {
		if notes := config.NormalizeStored(&defs[i]); len(notes) > 0 {
			log.Warn("stored repository definition has values this version no longer accepts; "+
				"they are used as noted until the repository is edited",
				"repo", defs[i].Name, "notes", strings.Join(notes, "; "))
		}
		if u, err := url.Parse(defs[i].Remote); err == nil && (u.User != nil || u.RawQuery != "") {
			plain = append(plain, defs[i].Name)
		}
	}
	if len(plain) > 0 && db.HasSecretKey() { // names only: the URL holds the secret
		log.Warn("the remote URL of these proxies holds a user name, password or query, which the secret key does "+
			"not encrypt: it is stored in meta.db in plain text; give a user name and password in remoteAuth instead",
			"repos", strings.Join(plain, ", "))
	}
	if err := config.ValidateRepositories(defs); err != nil {
		return nil, fmt.Errorf("stored repositories are invalid: %w", err)
	}
	return defs, nil
}

// loadAccess imports cfg's users, roles and settings into the database on the
// first start (plan 12), then opens the Authenticator over the database. On
// later starts the config keys are ignored with one warning naming them.
func loadAccess(cfg *config.Config, log *slog.Logger, db *meta.DB) (*auth.Authenticator, error) {
	ctx := context.Background()
	done, err := db.AccessImported(ctx)
	if err != nil {
		return nil, fmt.Errorf("load users, roles and settings: %w", err)
	}
	// The users and roles sections only matter for the import; afterwards a
	// problem in them is part of the "ignored" warning, not a startup failure.
	if accessErr := errors.Join(cfg.AccessErr, cfg.SettingsErr); !done && accessErr != nil {
		return nil, accessErr
	}
	imported, err := db.ImportAccess(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("import users, roles and settings: %w", err)
	}
	if imported {
		log.Info("imported users, roles and settings from the config file into the metadata database", "users", len(cfg.Users), "roles", len(cfg.Roles))
	} else {
		var keys []string
		for _, k := range config.AccessKeys {
			if cfg.Present[k] {
				keys = append(keys, k)
			}
		}
		if len(keys) > 0 {
			file := cfg.Path
			if file == "" {
				file = "config file"
			}
			attrs := []any{"file", file, "keys", strings.Join(keys, ",")}
			if accessErr := errors.Join(cfg.AccessErr, cfg.SettingsErr); accessErr != nil {
				attrs = append(attrs, "sectionError", accessErr.Error())
			}
			log.Warn("the "+strings.Join(keys, ", ")+" keys of "+file+" are ignored: users, roles and settings were imported "+
				"on the first start and are now managed in the web UI", attrs...)
		}
	}
	authn, err := auth.Open(ctx, db, db)
	if err != nil {
		return nil, fmt.Errorf("load users, roles and settings: %w", err)
	}
	// A placeholder hash (the config example's) fails every login of its
	// user without a word; say so on every start until it is reset (QA L-2).
	users, err := db.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}
	for _, u := range users {
		if u.Source == meta.SourceLocal && u.Status == meta.StatusActive && !auth.HashUsable(u.PasswordHash) {
			log.Warn("local user cannot sign in with a password: its stored password hash is not a bcrypt hash; "+
				"set a new password in the web UI or with \"commandcenter user reset-password\"", "user", u.ID)
		}
	}
	// Older builds stored a baseURL with user info, query or fragment (QA
	// N-24); it is served as is until changed. The value is not logged: it
	// may hold a password.
	if err := meta.ValidateBaseURL(authn.Snapshot().Settings().BaseURL); err != nil {
		log.Warn("the stored baseURL is no longer accepted; change it in the general settings", "err", err.Error())
	}
	return authn, nil
}

// warnUnknownRoleRepos logs roles with a privilege on a repository the
// registry lacks, as an older version or the config import may have left.
func warnUnknownRoleRepos(log *slog.Logger, a *auth.Snapshot, s *repo.Snapshot) {
	for _, role := range a.Roles() {
		for _, p := range role.Privileges {
			name, ok := strings.CutPrefix(p.Target, "repo:")
			if !ok || s.ByName[name] != nil {
				continue
			}
			log.Warn("role grants privileges on a repository that does not exist", "role", role.ID, "repository", name)
		}
	}
}

// shutdownTimeout bounds how long Serve drains requests once ctx is cancelled.
// docker-compose.yml's stop_grace_period must be longer, or Docker kills the
// process mid-drain (TestComposeStopGracePeriod).
const shutdownTimeout = 30 * time.Second

// drainDelay is how long a node on PostgreSQL keeps serving after SIGTERM
// while its readiness (GET /service/rest/v1/status/writable) answers 503,
// so load balancers and Kubernetes endpoints stop sending it requests
// before it stops taking connections. The image has no shell for a preStop
// sleep. SQLite, one instance, does not wait. A var for tests.
var drainDelay = 5 * time.Second

// Serve runs the HTTP server until ctx is cancelled, then drains requests for
// up to shutdownTimeout. The node is registered (see AsServer) for as long:
// on cancel it first turns unready, for drainDelay on PostgreSQL, and its row
// goes after everything else stopped.
func (a *App) Serve(ctx context.Context) error {
	if a.member == nil {
		m, err := join(ctx, a.DB, a.Node, a.log)
		if err != nil {
			return err
		}
		a.member = m
	}
	defer a.member.Leave(context.Background())
	srv := newHTTPServer(a.Handler)
	// The counters are flushed periodically; the final flush runs on every way
	// out, after the server stopped taking requests.
	mctx, stopMetrics := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		secs := a.Config.MetricsFlushSeconds
		if secs <= 0 {
			secs = 30
		}
		a.Metrics.Run(mctx, time.Duration(secs)*time.Second)
	}()
	defer func() {
		stopMetrics()
		<-runDone
		fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.Metrics.Flush(fctx); err != nil {
			a.log.Warn("could not flush metrics on shutdown", "err", logsafe.Err(err))
		}
	}()
	// What another process or node changed applies at once (PostgreSQL) or
	// within accessReloadEvery; the loop ends before Close closes the database.
	rctx, stopReload := context.WithCancel(ctx)
	reloadDone := make(chan struct{})
	go func() {
		defer close(reloadDone)
		a.follow(rctx)
	}()
	defer func() {
		stopReload()
		<-reloadDone
	}()
	// The scheduler stops (cancelling a run in progress) before the final
	// metrics flush and before Close closes the database. Webhook
	// deliveries are sent by a serving process only: a CLI command beside
	// it stores the ones its events make, and the server sends them.
	a.Tasks.Start(ctx)
	defer a.Tasks.Stop()
	a.Webhooks.Deliver()
	ln, err := net.Listen("tcp", a.Config.Listen)
	if err != nil {
		return err
	}
	ln = server.LimitConns(srv, ln, maxConns)
	errc := make(chan error, 1)
	go func() {
		a.log.Info("listening", "addr", a.Config.Listen, "database", a.DB.Driver())
		errc <- srv.Serve(ln)
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	a.log.Info("shutting down")
	a.member.Drain(context.Background())
	if a.DB.Driver() == "postgresql" && drainDelay > 0 {
		a.log.Info("draining: readiness answers 503 so that load balancers stop sending requests here", "delay", drainDelay.String())
		time.Sleep(drainDelay)
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close()
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newHTTPServer is the HTTP server Serve runs h on.
func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}

// accessReloadEvery is how often Serve reloads users, roles and settings, so
// a change another process wrote ("commandcenter user reset-password" next
// to a running server, QA N-2) applies without a restart. A reload reads
// three small tables; polling SQLite's data_version would need a connection
// of its own and changes on every write the server itself makes anyway.
// On PostgreSQL it is the safety net of the notifications between nodes,
// and covers the repositories, routing rules, blob stores and webhooks too.
var accessReloadEvery = 5 * time.Second

// The cache topics of meta.DB.Notify: a node that changed what the others
// cache sends one after the commit, and every node re-reads it.
var followTopics = []string{"access", "registry", "routing", "blobstores", "webhooks"}

// follow keeps this node's caches in step with the database until ctx
// ends: every accessReloadEvery, and on PostgreSQL also at once when a
// node sends a topic's notification. A failed reload keeps what is loaded
// and is logged once per run of failures. (The log level, cleanup policy,
// flush and task stop notifications have their own subscribers.)
func (a *App) follow(ctx context.Context) {
	cluster := a.DB.Driver() == "postgresql"
	var mu sync.Mutex
	pending := map[string]bool{}
	kick := make(chan struct{}, 1)
	if cluster {
		for _, topic := range followTopics {
			a.DB.Subscribe(topic, func(string) {
				mu.Lock()
				pending[topic] = true
				mu.Unlock()
				select {
				case kick <- struct{}{}:
				default:
				}
			})
		}
	}
	tick := time.NewTicker(accessReloadEvery)
	defer tick.Stop()
	failing := map[string]bool{}
	for {
		todo := map[string]bool{"access": true}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if cluster {
				todo["registry"], todo["webhooks"] = true, true
			}
		case <-kick:
			mu.Lock()
			todo, pending = pending, map[string]bool{}
			mu.Unlock()
		}
		for _, job := range []struct {
			name, what string
			run        func(context.Context) error
		}{
			{"access", "users, roles and settings", a.Auth.Reload},
			// one Resync re-reads the repositories, routing rules and blob stores
			{"registry", "repositories, routing rules and blob stores", a.Registry.Resync},
			{"webhooks", "webhooks", a.Webhooks.Reload},
		} {
			if !todo[job.name] && (job.name != "registry" || !todo["routing"] && !todo["blobstores"]) {
				continue
			}
			jctx, cancel := context.WithTimeout(ctx, accessReloadEvery)
			err := job.run(jctx)
			cancel()
			if err != nil && !failing[job.name] && ctx.Err() == nil {
				a.log.Warn("could not reload the "+job.what+" from the database; the loaded ones stay in force", "err", logsafe.Err(err))
			}
			failing[job.name] = err != nil
		}
	}
}

// GC runs garbage collection over every blob store and returns the total
// number of blobs removed and the bytes they held. A store whose collection
// fails does not stop the others; the errors are joined, each naming its store.
func (a *App) GC(ctx context.Context, minAge time.Duration) (int, int64, error) {
	total, freed := 0, int64(0)
	var errs []error
	for _, name := range a.Store.StoreNames() {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if a.Store.GroupOf(name) != "" {
			continue // collected with its group
		}
		n, b, err := a.GCStore(ctx, name, minAge)
		total, freed = total+n, freed+b
		if err != nil {
			errs = append(errs, fmt.Errorf("blob store %q: %w", name, err))
		}
	}
	return total, freed, errors.Join(errs...)
}

// gcScope returns the repositories whose assets keep blobs of a named store
// (not the default one, see GCStore) alive: those repo.StoreOf assigns to it,
// plus every repository whose store cannot be resolved (defense in depth: GC
// never deletes for such a repository, whichever store its blobs ended up in).
// For a group, the repositories that name one of its members count too
// (none should: only a config file or a race could make one).
func (a *App) gcScope(storeName string) []string {
	snap := a.Registry.Snapshot()
	out := append(repo.ReposForStore(snap, storeName), repo.OrphanRepos(snap, a.Store.HasStore)...)
	if bs := a.Store.StoreByName(storeName); bs != nil {
		if cfg, ok := bs.Group(); ok {
			for _, m := range cfg.Members {
				out = append(out, repo.ReposForStore(snap, m.Name)...)
			}
		}
	}
	return append(out, a.Store.Moving(storeName)...) // a move keeps its blobs in both stores
}

// restoreMoves routes the repositories whose change of blob store a
// restart interrupted as during the move, so blobs written to the target
// stay readable until the move's task runs again and finishes it.
func restoreMoves(db *meta.DB, store *repo.Store, registry *repo.Registry, log *slog.Logger) error {
	moves, err := db.ListBlobStoreMoves(context.Background())
	if err != nil {
		return fmt.Errorf("load blob store moves: %w", err)
	}
	for _, m := range moves {
		store.BeginMove(m.Repo, m.Source, m.Target, "interrupted")
		log.Warn("a repository's move to another blob store is unfinished: it reads from both stores and writes to the new one; run its repository.change-blobstore task to finish it",
			"repository", m.Repo, "from", m.Source, "to", m.Target)
	}
	return nil
}

// GCStore removes blobs that no asset in the store's own repositories
// references and that are older than minAge, deleting only from that store's
// *blob.Store, and returns how many it removed and the bytes they held.
// "In use" is scoped to the store's repositories (gcScope) so that a blob
// shared by name across stores (dedup is per store) is never wrongly removed.
// The default store keeps every blob any asset references: a build without
// named stores (an unsupported downgrade) writes all repositories' blobs
// there, and those may be the only copies (QA N-30).
// The age guard protects uploads written but not yet recorded; the ModTime and
// in-use re-check immediately before removal protects uploads that raced the
// walk. A group is collected member by member, each copy of a blob on its
// own age, with the group's scope; a member is not collected on its own.
func (a *App) GCStore(ctx context.Context, storeName string, minAge time.Duration) (int, int64, error) {
	if storeName == "" {
		storeName = repo.DefaultStore
	}
	bs := a.Store.StoreByName(storeName)
	if bs == nil {
		return 0, 0, fmt.Errorf("unknown blob store %q", storeName)
	}
	if g := a.Store.GroupOf(storeName); g != "" {
		return 0, 0, fmt.Errorf("blob store %q is a member of the group %q: compact the group", storeName, g)
	}
	// The walk below sees only blobs; temp files a killed process left are swept here.
	if n, err := bs.SweepTmp(); err != nil {
		a.log.Warn("could not sweep stale temp files", "blobStore", storeName, "removed", n, "err", logsafe.Err(err))
	} else if n > 0 {
		a.log.Info("removed stale temp files", "blobStore", storeName, "removed", n)
	}
	inUse := func(sha string) (bool, error) { return a.DB.BlobInUse(ctx, sha) }
	if storeName != repo.DefaultStore {
		repos := a.gcScope(storeName)
		inUse = func(sha string) (bool, error) { return a.DB.BlobInUseInRepos(ctx, sha, repos) }
	}
	cutoff := time.Now().Add(-minAge)
	cfg, isGroup := bs.Group()
	if !isGroup {
		return gcWalk(bs, inUse, cutoff)
	}
	removed, freed := 0, int64(0)
	var errs []error
	for _, m := range cfg.Members {
		ms := a.Store.StoreByName(m.Name)
		if ms == nil {
			errs = append(errs, fmt.Errorf("member %q is not open", m.Name))
			continue
		}
		ms.SweepTmp() // best effort, as above: a member's commits across filesystems spool there
		n, b, err := gcWalk(ms, inUse, cutoff)
		removed, freed = removed+n, freed+b
		if err != nil {
			errs = append(errs, fmt.Errorf("member %q: %w", m.Name, err))
		}
	}
	return removed, freed, errors.Join(errs...)
}

// gcWalk removes the blobs of bs that are older than cutoff and not inUse.
func gcWalk(bs *blob.Store, inUse func(string) (bool, error), cutoff time.Time) (int, int64, error) {
	removed, freed := 0, int64(0)
	err := bs.Walk(func(sha string, mod time.Time) error {
		if mod.After(cutoff) {
			return nil
		}
		used, err := inUse(sha)
		if err != nil || used {
			return err
		}
		// Re-check right before removal: a concurrent upload may have refreshed
		// the blob or recorded an asset since the walk saw it.
		info, err := bs.Stat(sha)
		if errors.Is(err, blob.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.ModTime.After(cutoff) {
			return nil
		}
		used, err = inUse(sha)
		if err != nil || used {
			return err
		}
		if err := bs.Remove(sha); err != nil {
			return err
		}
		removed, freed = removed+1, freed+info.Size
		return nil
	})
	return removed, freed, err
}

// Close stops the event writer (it drains its queue first) and closes the database.
func (a *App) Close() error {
	a.Mail.Close() // may still record a failed send as an event
	a.Webhooks.Close()
	a.Vuln.Close()
	a.Events.Close()
	return a.DB.Close()
}

// newTransfer builds repository export and import: under the export root in
// COMMANDCENTER_EXPORT_ROOT or exportRoot (an absolute path; neither
// disables them), with the formats' upload locks and index rebuilds.
func newTransfer(cfg *config.Config, db *meta.DB, store *repo.Store, handlers map[string]repo.Handler, rebuild map[string]transfer.RebuildFunc) (*transfer.Service, error) {
	root := os.Getenv(transfer.RootEnv)
	switch {
	case root != "" && cfg.ExportRoot != "":
		return nil, fmt.Errorf("set %s or exportRoot, not both", transfer.RootEnv)
	case root == "":
		root = cfg.ExportRoot
	}
	if root != "" && !filepath.IsAbs(root) {
		return nil, fmt.Errorf("exportRoot (%s) must be an absolute path", root)
	}
	lockers := map[string]transfer.Locker{}
	for format, h := range handlers {
		if l, ok := h.(transfer.Locker); ok {
			lockers[format] = l
		}
	}
	return &transfer.Service{DB: db, Store: store, Root: root, Version: Version, Lockers: lockers, Rebuild: rebuild}, nil
}
