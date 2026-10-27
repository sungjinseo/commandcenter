package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"commandcenter/internal/importer"
)

const importUsage = "usage: commandcenter import nexus --server http://HOST:8081 [--token cc_… | env COMMANDCENTER_TOKEN] " +
	"--from https://nexus.example [--user NAME] [--repos a,b] [--definitions=true] [--content=true] [--blob-store default] " +
	"[--rename-prefix PREFIX] [--job ID] [--concurrency 4] [--dry-run] [--insecure-skip-verify] [--retry-failed] [--wait]; " +
	"the source password comes from the NEXUS_PASSWORD environment variable or a prompt, never from a flag"

// importPoll is how often --wait prints the progress; a var for tests.
var importPoll = 2 * time.Second

// readSecret prompts for the source password on the terminal without echo
// (secret_*.go); a var for tests.
var readSecret = readSecretTTY

// errNoTTY is what readSecret answers when there is no terminal to ask on.
var errNoTTY = errors.New("no terminal to prompt for the source password on: set the NEXUS_PASSWORD environment variable")

// interrupts delivers Ctrl-C while --wait follows the import; a var for tests.
var interrupts = func() (<-chan os.Signal, func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	return c, func() { signal.Stop(c) }
}

// errDetached ends --wait after a second Ctrl-C: the import goes on.
var errDetached = errors.New("detached: the import keeps running on the server; stop it on the import page or run the command again with the same --job")

// importCmd starts an import from Nexus Repository 3 on a running server
// through its API (plan 15 Decision 7) and, with --wait, follows it: a
// progress line every 2 s, Ctrl-C asks whether to stop the import, and the
// report summary at the end. The token and the source password are sent in
// the request only, never printed.
func importCmd(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "nexus" {
		return errors.New(importUsage)
	}
	fs := flag.NewFlagSet("import nexus", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("server", "", "the Command Center base URL, e.g. http://localhost:8081")
	token := fs.String("token", "", "an admin's API token; prefer the COMMANDCENTER_TOKEN environment variable, since arguments are visible in the process list")
	from := fs.String("from", "", "the Nexus Repository base URL, e.g. https://nexus.example")
	user := fs.String("user", "", "the Nexus Repository user (or user-token name); the password comes from NEXUS_PASSWORD or a prompt")
	repos := fs.String("repos", "", "comma-separated source repositories (default: all)")
	defs := fs.Bool("definitions", true, "create the repository definitions")
	content := fs.Bool("content", true, "import the content of hosted repositories")
	blobStore := fs.String("blob-store", "", "the Command Center blob store for new repositories (default: default)")
	prefix := fs.String("rename-prefix", "", "a prefix for the names of the new repositories")
	job := fs.String("job", "", "the job id; the same id resumes a job (default: <source-host>-<yyyymmdd>)")
	conc := fs.Int("concurrency", 0, "parallel downloads from the source, 1-16 (default 4)")
	dry := fs.Bool("dry-run", false, "list what would be created and imported, write nothing")
	insecure := fs.Bool("insecure-skip-verify", false, "do not verify the source's TLS certificate (unsafe)")
	retry := fs.Bool("retry-failed", false, "retry the assets that failed in an earlier run of the job")
	wait := fs.Bool("wait", false, "follow the import until it ends and exit non-zero when assets failed")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("%v; %s", err, importUsage)
	}
	if fs.NArg() > 0 {
		return errors.New(importUsage)
	}
	if *server == "" || *from == "" {
		return errors.New("--server and --from are required; " + importUsage)
	}
	u, err := url.Parse(*server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("--server must be an http(s) URL like http://localhost:8081")
	}
	if u.User != nil {
		return errors.New("--server must not contain credentials; pass the token with --token or COMMANDCENTER_TOKEN")
	}
	src, err := url.Parse(*from)
	if err != nil || (src.Scheme != "http" && src.Scheme != "https") || src.Host == "" {
		return errors.New("--from must be an http(s) URL like https://nexus.example")
	}
	if src.User != nil {
		return errors.New("--from must not contain credentials; pass --user and the password through NEXUS_PASSWORD or the prompt")
	}
	warnPlainHTTP(u, stderr)
	root := strings.TrimRight(u.String(), "/")
	tok := *token
	if tok == "" {
		tok = os.Getenv("COMMANDCENTER_TOKEN")
	}
	if tok == "" {
		return errors.New("a token is required: --token or the COMMANDCENTER_TOKEN environment variable")
	}
	password := ""
	if *user != "" {
		password = os.Getenv("NEXUS_PASSWORD")
		if password == "" {
			if password, err = readSecret(fmt.Sprintf("Password of %s on %s: ", *user, src.Host)); err != nil {
				return err
			}
		}
		if password == "" {
			return errors.New("the source password is empty")
		}
	}
	if *insecure {
		fmt.Fprintln(stderr, "warning: --insecure-skip-verify: the server does not verify the source's TLS certificate, so the source password can be read by anyone who can intercept the connection")
	}

	body := map[string]any{"url": *from, "importDefinitions": *defs, "importContent": *content,
		"dryRun": *dry, "retryFailed": *retry, "insecureSkipVerify": *insecure}
	if *user != "" {
		body["username"] = *user
		body["password"] = password
	}
	if list := splitList(*repos); len(list) > 0 {
		body["repositories"] = list
	}
	if *blobStore != "" {
		body["blobStore"] = *blobStore
	}
	if *prefix != "" {
		body["renamePrefix"] = *prefix
	}
	if *job != "" {
		body["job"] = *job
	}
	if *conc != 0 {
		body["concurrency"] = *conc
	}
	raw, _ := json.Marshal(body)
	password, body = "", nil
	client := &http.Client{Timeout: 60 * time.Second}
	var started struct {
		ID     string `json:"id"`
		Job    string `json:"job"`
		DryRun bool   `json:"dryRun"`
	}
	err = apiCall(client, http.MethodPost, root+"/-/api/import/nexus", tok, raw, &started)
	clear(raw)
	if err != nil {
		return fmt.Errorf("start the import: %w", err)
	}
	if started.Job == "" {
		return errors.New("start the import: the server returned no job")
	}
	kind := "import"
	if started.DryRun {
		kind = "dry run (nothing is written)"
	}
	fmt.Fprintf(stdout, "%s started: job %s (run %s)\n", kind, started.Job, started.ID)
	if !*wait {
		fmt.Fprintf(stdout, "follow it on the import page, or with GET %s/-/api/import/jobs/%s\n", root, url.PathEscape(started.Job))
		return nil
	}
	return followImport(client, root, tok, started.Job, started.ID, stdin, stdout)
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// importState is GET /-/api/import/jobs/{job}?run=<id>.
type importState struct {
	Status   string                 `json:"status"`
	Message  string                 `json:"message"`
	Starting bool                   `json:"starting"`
	Progress *importer.ProgressView `json:"progress"`
	Report   *importer.Report       `json:"report"`
}

// followImport polls the job every importPoll and prints a progress line
// until it ends, then the report. Ctrl-C asks whether to stop the import;
// a second Ctrl-C while asking detaches.
func followImport(client *http.Client, root, tok, job, run string, stdin io.Reader, stdout io.Writer) error {
	sig, stopSig := interrupts()
	defer stopSig()
	in := bufio.NewReader(stdin)
	endpoint := root + "/-/api/import/jobs/" + url.PathEscape(job) + "?run=" + url.QueryEscape(run)
	failures := 0
	for {
		var st importState
		if err := apiCall(client, http.MethodGet, endpoint, tok, nil, &st); err != nil {
			if failures++; failures > pollRetries {
				return fmt.Errorf("check the import job: %w", err)
			}
		} else {
			failures = 0
			if st.Status != importer.StatusRunning {
				return printImportReport(stdout, job, st)
			}
			fmt.Fprintln(stdout, progressLine(st))
		}
		select {
		case <-time.After(importPoll):
		case <-sig:
			stop, err := askStop(job, in, sig)
			if err != nil {
				return err
			}
			if stop {
				var res map[string]any
				if err := apiCall(client, http.MethodPost, root+"/-/api/import/jobs/"+url.PathEscape(job)+"/stop", tok, []byte("{}"), &res); err != nil {
					fmt.Fprintf(stderr, "stop the import: %v\n", err)
				} else {
					fmt.Fprintln(stdout, "stopping the import; it resumes when started again with the same --job")
				}
			}
		}
	}
}

// askStop asks on stderr whether to stop the import and reads the answer
// from in. No answer (end of input) stops it, as Ctrl-C asked; another
// Ctrl-C while asking detaches.
func askStop(job string, in *bufio.Reader, sig <-chan os.Signal) (bool, error) {
	fmt.Fprintf(stderr, "\nstop import job %s on the server? It can be resumed later with the same --job. [y/N] (Ctrl-C again: leave it running) ", job)
	ans := make(chan string, 1)
	go func() {
		line, err := in.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			line = "y"
		}
		ans <- line
	}()
	select {
	case a := <-ans:
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "y" || a == "yes" {
			return true, nil
		}
		fmt.Fprintln(stderr, "the import goes on")
		return false, nil
	case <-sig:
		fmt.Fprintln(stderr)
		return false, errDetached
	}
}

func progressLine(st importState) string {
	p := st.Progress
	if p == nil {
		return time.Now().Format("15:04:05") + "  starting"
	}
	cur := p.Repository
	if cur == "" {
		cur = "-"
	}
	return fmt.Sprintf("%s  repositories %d/%d  assets %d/%d (imported %d, skipped %d, failed %d)  %s  current: %s",
		time.Now().Format("15:04:05"), p.RepositoriesDone, p.RepositoriesTotal,
		p.AssetsDone+p.AssetsSkipped+p.AssetsFailed, p.AssetsTotal, p.AssetsDone, p.AssetsSkipped, p.AssetsFailed,
		humanBytes(p.Bytes), cur)
}

// printImportReport prints the summary of a finished job and returns an
// error when it failed, stopped or has failed assets.
func printImportReport(w io.Writer, job string, st importState) error {
	r := st.Report
	if r == nil {
		msg := st.Message
		if msg == "" {
			msg = "no report"
		}
		return fmt.Errorf("import job %s %s: %s", job, st.Status, msg)
	}
	fmt.Fprintf(w, "import job %s: %s — %s\n", job, r.Status, r.Summary())
	fmt.Fprintf(w, "%-24s %-24s %-14s %-12s %9s %9s %9s %9s\n", "SOURCE", "TARGET", "FORMAT/TYPE", "DEFINITION", "IMPORTED", "SKIPPED", "FAILED", "SIZE")
	for _, rr := range r.Repositories {
		c := rr.Counts
		imp, skip := c.Imported, c.Skipped+c.AlreadyDone
		if r.DryRun {
			imp, skip = c.WouldImport, c.WouldSkip+c.AlreadyDone
		}
		fmt.Fprintf(w, "%-24s %-24s %-14s %-12s %9d %9d %9d %9s\n", rr.Source, rr.Target, rr.Format+"/"+rr.Type, rr.Definition,
			imp, skip, c.Failed+c.NotRetried, humanBytes(c.Bytes))
		for _, e := range rr.Errors {
			fmt.Fprintf(w, "    error: %s\n", e)
		}
		if rr.Omitted > 0 {
			fmt.Fprintf(w, "    … and %d more errors\n", rr.Omitted)
		}
		for _, n := range rr.Notes {
			fmt.Fprintf(w, "    note: %s\n", n)
		}
	}
	for _, x := range r.Unsupported {
		if x.Note != "" {
			fmt.Fprintf(w, "not imported: %s (%d): %s\n    %s\n", x.Format, x.Count, strings.Join(x.Repositories, ", "), x.Note)
			continue
		}
		fmt.Fprintf(w, "unsupported in this version: %s (%d): %s\n", x.Format, x.Count, strings.Join(x.Repositories, ", "))
	}
	if len(r.ProxiesNeedingCredentials) > 0 {
		fmt.Fprintf(w, "proxies needing credentials: %s\n", strings.Join(r.ProxiesNeedingCredentials, ", "))
	}
	for _, c := range r.Conflicts {
		fmt.Fprintf(w, "conflict: %s\n", c)
	}
	if r.ConflictsOmitted > 0 {
		fmt.Fprintf(w, "… and %d more conflicts\n", r.ConflictsOmitted)
	}
	for _, x := range r.Warnings {
		fmt.Fprintf(w, "warning: %s\n", x)
	}
	for _, x := range r.NotImported {
		fmt.Fprintf(w, "not imported: %s\n", x)
	}
	if r.Message != "" {
		fmt.Fprintf(w, "message: %s\n", r.Message)
	}
	failed := r.Totals.Failed + r.Totals.NotRetried
	switch {
	case r.Status == importer.StatusStopped:
		return fmt.Errorf("import job %s stopped; run the command again with --job %s to resume", job, job)
	case failed > 0:
		return fmt.Errorf("import job %s: %d assets failed; --retry-failed retries them", job, failed)
	case r.Status != importer.StatusOK && r.Status != importer.StatusDryRun:
		return fmt.Errorf("import job %s %s", job, r.Status)
	}
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
