package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const backupUsage = "usage: commandcenter backup --url http://HOST:8081 [--token cc_… | env COMMANDCENTER_TOKEN] " +
	"[--subdir NAME] [--keep N] [--verify] [--deep] [--wait] [--timeout 24h]"

// backupPoll is how often --wait asks for the run's state; a var for tests.
var backupPoll = 2 * time.Second

// pollRetries is how many failed polls in a row --wait tolerates.
const pollRetries = 3

// backupCmd starts an online backup on a running server through its API
// (plan 14 Decision 3) and, with --wait, polls the run until it finished.
// The token is sent as a Bearer header and never printed.
func backupCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	base := fs.String("url", "", "the server's base URL, e.g. http://localhost:8081")
	token := fs.String("token", "", "an admin's API token; prefer the COMMANDCENTER_TOKEN environment variable, since arguments are visible in the process list")
	subdir := fs.String("subdir", "", "backup directory name under the server's backupDir (default: the date)")
	keep := fs.Int("keep", 0, "backups to keep (default 7)")
	verify := fs.Bool("verify", false, "verify the backup after writing it")
	deep := fs.Bool("deep", false, "verify by re-hashing every blob (implies --verify)")
	wait := fs.Bool("wait", false, "wait for the backup to finish and exit non-zero if it failed")
	timeout := fs.Duration("timeout", 24*time.Hour, "how long --wait waits")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%v; %s", err, backupUsage)
	}
	if fs.NArg() > 0 {
		return errors.New(backupUsage)
	}
	if *base == "" {
		return errors.New("--url is required; " + backupUsage)
	}
	u, err := url.Parse(*base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("--url must be an http(s) URL like http://localhost:8081")
	}
	if u.User != nil {
		return errors.New("--url must not contain credentials; pass the token with --token or COMMANDCENTER_TOKEN")
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

	params := map[string]any{}
	if *subdir != "" {
		params["subdir"] = *subdir
	}
	if *keep != 0 {
		params["keep"] = *keep
	}
	if *verify {
		params["verify"] = true
	}
	if *deep {
		params["deep"] = true
	}
	body, _ := json.Marshal(params)
	client := &http.Client{Timeout: 60 * time.Second}
	var started struct {
		ID string `json:"id"`
	}
	if err := apiCall(client, http.MethodPost, root+"/-/api/backup", tok, body, &started); err != nil {
		return fmt.Errorf("start the backup: %w", err)
	}
	if started.ID == "" {
		return errors.New("start the backup: the server returned no run id")
	}
	fmt.Fprintf(stdout, "backup started (run %s)\n", started.ID)
	if !*wait {
		return nil
	}
	deadline := time.Now().Add(*timeout)
	failures := 0
	for {
		var st struct {
			Status     string `json:"status"`
			Message    string `json:"message"`
			DurationMs int64  `json:"durationMs"`
		}
		if err := apiCall(client, http.MethodGet, root+"/-/api/backup/runs/"+url.PathEscape(started.ID), tok, nil, &st); err != nil {
			// A restart of a proxy or a dropped connection: retry a few times.
			if failures++; failures > pollRetries {
				return fmt.Errorf("check the backup run: %w", err)
			}
			time.Sleep(backupPoll)
			continue
		}
		failures = 0
		if st.Status != "Running" {
			if st.Status != "OK" {
				return fmt.Errorf("backup %s: %s", st.Status, st.Message)
			}
			fmt.Fprintf(stdout, "backup OK in %d ms: %s\n", st.DurationMs, st.Message)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("backup run %s still running after %s", started.ID, *timeout)
		}
		time.Sleep(backupPoll)
	}
}

// apiCall sends one JSON request with the token and decodes a 2xx answer
// into out. Errors carry the status and the server's error message only;
// the request (and its token) is never included.
func apiCall(client *http.Client, method, endpoint, token string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, endpoint, rd)
	if err != nil {
		return errors.New("invalid request URL")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s %s: %v", method, endpoint, ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		// Redact first: cutting first could leave a fragment of the token.
		msg = strings.ReplaceAll(msg, token, "[token]")
		if len(msg) > 300 {
			msg = strings.ToValidUTF8(msg[:300], "") + "…"
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("unexpected response: %v", err)
	}
	return nil
}

// stderr receives warnings; a var for tests.
var stderr io.Writer = os.Stderr

// warnPlainHTTP warns when u is plain http to a host that is not a loopback
// address: the token then crosses the network in the clear. The command still
// runs (a trusted private network is a legitimate setup).
func warnPlainHTTP(u *url.URL, w io.Writer) {
	if u.Scheme != "http" {
		return
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return
	}
	fmt.Fprintf(w, "warning: %s is plain http to a non-loopback host, so the API token is sent unencrypted; use https or an SSH tunnel unless the network is trusted\n", host)
}
