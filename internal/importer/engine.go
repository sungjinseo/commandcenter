package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"commandcenter/internal/format/cargo"
	"commandcenter/internal/format/maven"
	"commandcenter/internal/format/npm"
	"commandcenter/internal/format/pypi"
	"commandcenter/internal/importer/nexus"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Logger receives the run log lines (tasks.RunLog).
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

type discard struct{}

func (discard) Infof(string, ...any)  {}
func (discard) Warnf(string, ...any)  {}
func (discard) Errorf(string, ...any) {}

// ErrNeedPassword refuses a run whose source password is not in memory: a
// "run now" of the stored task, or a resume after a restart. Passwords are
// never stored, so the import must be started again with it.
var ErrNeedPassword = errors.New("the source password is not available (it is never stored): start the import again from the import page or the CLI with the password")

// ErrRunning refuses a second run while one is in progress.
var ErrRunning = errors.New("an import is already running")

// Engine runs imports. One run at a time: the import.nexus task holds the
// scheduler's run slot.
type Engine struct {
	DB     *meta.DB
	Repos  *repo.Registry
	Maven  *maven.Handler
	Npm    *npm.Handler
	Pypi   *pypi.Handler
	Cargo  *cargo.Handler
	Events repo.EventSink // repo.create events; nil records none

	// HasStore reports whether a blob store exists; nil skips the check.
	HasStore func(string) bool
	// MaxUpload is the server's upload limit in bytes (maxUploadMiB): the cap
	// of a download whose size the source does not list. Nil or 0: none.
	MaxUpload func() int64
	Log       *slog.Logger // panics of a run; nil discards
	Now       func() time.Time

	// Test hooks.
	clientOpts     func(*nexus.Options)
	afterAsset     func(path string) // after each downloaded asset is handled
	ingestOverride func(ctx context.Context, rp *repo.Repository, a nexus.Asset, in io.Reader) (repo.IngestResult, error)

	mu   sync.Mutex
	cur  *Progress
	dry  map[string]*Report // the last dry runs, by job
	dryQ []string
}

const maxDryRuns = 16

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Normalize validates p against the engine (blob stores).
func (e *Engine) Normalize(p Params) (Params, []string) { return p.Normalize(e.now(), e.HasStore) }

// Running returns the live progress of the job in progress.
func (e *Engine) Running() (ProgressView, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cur == nil {
		return ProgressView{}, false
	}
	return e.cur.View(), true
}

// DryRun returns the report of the last dry run of job, kept in memory.
func (e *Engine) DryRun(job string) (*Report, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.dry[job]
	return r, ok
}

// DryRuns returns the dry-run reports kept in memory, newest first.
func (e *Engine) DryRuns() []*Report {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Report, 0, len(e.dryQ))
	for i := len(e.dryQ) - 1; i >= 0; i-- {
		out = append(out, e.dry[e.dryQ[i]])
	}
	return out
}

func (e *Engine) keepDryRun(r *Report) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dry == nil {
		e.dry = map[string]*Report{}
	}
	if _, ok := e.dry[r.Job]; ok {
		for i, j := range e.dryQ {
			if j == r.Job {
				e.dryQ = append(e.dryQ[:i], e.dryQ[i+1:]...)
				break
			}
		}
	}
	e.dry[r.Job] = r
	e.dryQ = append(e.dryQ, r.Job)
	for len(e.dryQ) > maxDryRuns {
		delete(e.dry, e.dryQ[0])
		e.dryQ = e.dryQ[1:]
	}
}

func (e *Engine) client(p Params, cred Credentials) (*nexus.Client, error) {
	o := nexus.Options{BaseURL: p.URL, Username: p.Username, Password: cred.Password, InsecureSkipVerify: p.InsecureSkipVerify}
	if e.clientOpts != nil {
		e.clientOpts(&o)
	}
	return nexus.New(o)
}

// Run imports per p (already normalized) and returns the report, which is
// stored with the job unless p.DryRun. The error is nil only when every
// step succeeded; a stopped run returns the context's error.
func (e *Engine) Run(ctx context.Context, p Params, cred Credentials, actor string, log Logger) (*Report, error) {
	if log == nil {
		log = discard{}
	}
	if p.Username != "" && cred.Password == "" {
		return nil, ErrNeedPassword
	}
	prog := &Progress{Job: p.Job, DryRun: p.DryRun, Started: e.now()}
	e.mu.Lock()
	if e.cur != nil {
		e.mu.Unlock()
		return nil, ErrRunning
	}
	e.cur = prog
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.cur = nil
		e.mu.Unlock()
	}()

	rep := newReport(p.Job, logsafe.URL(p.URL), p.DryRun, prog.Started)
	paramsJSON, _ := json.Marshal(p) // Params has no password field
	job := meta.ImportJob{Job: p.Job, Source: rep.Source, Params: string(paramsJSON), Status: StatusRunning, Actor: actor, Started: rep.Started}
	if !p.DryRun {
		if err := e.DB.PutImportJob(ctx, job); err != nil {
			return nil, fmt.Errorf("record the import job: %w", err)
		}
	}
	mode := "import"
	if p.DryRun {
		mode = "dry run (nothing is written)"
	}
	log.Infof("job %s: %s from %s (definitions: %v, content: %v, blob store %q, concurrency %d)",
		p.Job, mode, rep.Source, p.ImportDefinitions, p.ImportContent, p.BlobStore, p.Concurrency)
	if p.InsecureSkipVerify {
		log.Warnf("TLS certificate verification of the source is disabled (insecureSkipVerify)")
	}

	err := e.run(ctx, p, cred, actor, log, prog, rep)

	rep.Finished = e.now()
	for _, rr := range rep.Repositories {
		rep.Totals.add(rr.Counts)
	}
	switch {
	case ctx.Err() != nil:
		rep.Status = StatusStopped
		if err == nil {
			err = ctx.Err()
		}
	case err != nil:
		rep.Status = StatusFailed
	case p.DryRun:
		rep.Status = StatusDryRun // earlier failures are a preview here, not a failure
	case rep.Totals.Failed > 0 || rep.Totals.NotRetried > 0:
		rep.Status = StatusFailed
		err = fmt.Errorf("%d assets failed (see the report of job %s)", rep.Totals.Failed+rep.Totals.NotRetried, p.Job)
	default:
		rep.Status = StatusOK
	}
	if err != nil {
		rep.Message = logsafe.URLs(err.Error())
	}
	log.Infof("%s", rep.Summary())
	if p.DryRun {
		e.keepDryRun(rep)
		return rep, err
	}
	b, _ := json.Marshal(rep)
	job.Status, job.Finished, job.Report = rep.Status, rep.Finished, string(b)
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if perr := e.DB.PutImportJob(wctx, job); perr != nil {
		log.Errorf("could not store the report of job %s: %v", p.Job, perr)
		if err == nil {
			err = perr
		}
	}
	return rep, err
}

func (e *Engine) run(ctx context.Context, p Params, cred Credentials, actor string, log Logger, prog *Progress, rep *Report) error {
	c, err := e.client(p, cred)
	if err != nil {
		return err
	}
	settings, err := c.RepositorySettings(ctx)
	if err != nil {
		return err
	}
	plans := e.plan(settings, p, rep, log)
	prog.ReposTotal.Store(int64(len(plans)))
	if p.ImportDefinitions {
		e.definitions(ctx, plans, p, actor, rep, log)
	} else {
		for _, pl := range plans {
			e.checkExisting(pl, rep)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var failed []string
	for _, pl := range plans {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.ImportContent && pl.contentFormat() && pl.ok {
			pl.rr.Content = true
			prog.setCurrent(pl.src.Name)
			if err := e.importRepo(ctx, c, p, pl, actor, log, prog, rep); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				pl.rr.addError(logsafe.Err(err))
				log.Errorf("%s: %v", pl.src.Name, err)
				failed = append(failed, pl.src.Name)
			}
		}
		prog.ReposDone.Add(1)
	}
	prog.setCurrent("")
	if len(failed) > 0 {
		return fmt.Errorf("the content of %d repositories could not be imported: %s", len(failed), joinCapped(failed, 10))
	}
	return nil
}

func joinCapped(list []string, n int) string {
	if len(list) <= n {
		return fmt.Sprint(list)
	}
	return fmt.Sprint(list[:n]) + fmt.Sprintf(" and %d more", len(list)-n)
}

// SourceRepo is one source repository as the test connection lists it.
type SourceRepo struct {
	Name         string `json:"name"`
	Format       string `json:"format"`
	Type         string `json:"type"`
	Supported    bool   `json:"supported"` // the definition can be imported
	Content      bool   `json:"content"`   // the content can be imported (hosted maven, npm, pypi, cargo)
	TargetFormat string `json:"targetFormat,omitempty"`
}

// Test connects to the source with the given credentials and lists its
// repositories with what can be imported. It writes nothing.
func (e *Engine) Test(ctx context.Context, p Params, cred Credentials) ([]SourceRepo, error) {
	c, err := e.client(p, cred)
	if err != nil {
		return nil, err
	}
	list, err := c.Repositories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SourceRepo, 0, len(list))
	for _, r := range list {
		f := mapFormat(r.Format)
		out = append(out, SourceRepo{Name: r.Name, Format: r.Format, Type: r.Type, Supported: f != "" && knownType(r.Type),
			Content: r.Type == "hosted" && contentFormats[f], TargetFormat: f})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
