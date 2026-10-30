package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"commandcenter/internal/app"
	"commandcenter/internal/config"
	"commandcenter/internal/ui"
)

const configUsage = "usage: commandcenter config export -config PATH [-o FILE] [-format yaml|json] [-include S,...] [-exclude S,...] " +
	"[-secrets omit|encrypted -passphrase-file FILE] [-passwords]\n" +
	"       commandcenter config apply -config PATH -f FILE|- [-dry-run] [-prune] [-include S,...] [-exclude S,...] [-passphrase-file FILE]"

// passphraseEnv holds the secrets passphrase when -passphrase-file is not given.
const passphraseEnv = "COMMANDCENTER_CONFIG_PASSPHRASE"

// configCmd exports or applies the configuration document. Like token
// create and gc it opens the metadata database itself (beside running
// servers, or with none), and goes through the same handlers as the REST
// API (POST /-/api/configuration), whose notifications make every running
// node apply each change. Blob stores it creates are opened on this host:
// run it where the servers' data directory is mounted.
func configCmd(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "export" && args[0] != "apply" {
		return errors.New(configUsage)
	}
	fs := flag.NewFlagSet("config "+args[0], flag.ContinueOnError)
	path := fs.String("config", defaultConfig, "config file")
	out := fs.String("o", "", "export: write the document to this file (default: standard output)")
	format := fs.String("format", "yaml", "export: yaml or json")
	include := fs.String("include", "", "sections to export or apply, comma separated (default: all)")
	exclude := fs.String("exclude", "", "sections to leave out, comma separated")
	secrets := fs.String("secrets", "omit", "export: omit, or encrypted (with -passphrase-file)")
	passFile := fs.String("passphrase-file", "", "file holding the secrets passphrase (default: "+passphraseEnv+")")
	passwords := fs.Bool("passwords", false, "export: include the local users' password hashes")
	file := fs.String("f", "", "apply: the document (YAML or JSON), - for standard input")
	dryRun := fs.Bool("dry-run", false, "apply: print the plan, change nothing")
	prune := fs.Bool("prune", false, "apply: delete the objects a section of the document does not list")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})
	opts := ui.ConfigOptions{Format: *format, Include: splitList(*include), Exclude: splitList(*exclude), Passwords: *passwords,
		DryRun: *dryRun, Prune: *prune, RequestID: "cli-config"}
	switch *secrets {
	case "omit":
	case "encrypted":
		opts.Secrets = true
	default:
		return errors.New(`-secrets must be "omit" or "encrypted"`)
	}
	opts.Passphrase = os.Getenv(passphraseEnv)
	if *passFile != "" {
		b, err := os.ReadFile(*passFile)
		if err != nil {
			return fmt.Errorf("-passphrase-file: %w", err)
		}
		opts.Passphrase = strings.TrimRight(string(b), "\r\n")
	}
	var doc []byte
	if args[0] == "apply" {
		var err error
		switch *file {
		case "":
			return errors.New("-f is required; " + configUsage)
		case "-":
			doc, err = io.ReadAll(stdin)
		default:
			doc, err = os.ReadFile(*file)
		}
		if err != nil {
			return err
		}
	}
	p := pickConfigPath(*path, explicit, legacyConfig, func(msg string) { fmt.Fprintln(os.Stderr, msg) })
	cfg, err := config.Load(p)
	if err != nil {
		return err
	}
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return err
	}
	defer a.Close()
	ctx := context.Background()
	if args[0] == "export" {
		b, err := a.ExportConfig(ctx, opts)
		if err != nil {
			return err
		}
		if *out == "" {
			_, err = stdout.Write(b)
			return err
		}
		return os.WriteFile(*out, b, 0o600) // it may hold password hashes or encrypted secrets
	}
	res, err := a.ApplyConfig(ctx, doc, opts)
	if err != nil {
		return err
	}
	printPlan(stdout, res)
	if !res.OK {
		return errors.New(res.Error)
	}
	return nil
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

// printPlan prints every step that changes something, its fields (never a
// secret's value) and, after an apply, its status.
func printPlan(w io.Writer, res *ui.ConfigResult) {
	for _, st := range res.Steps {
		if st.Action == "unchanged" {
			continue
		}
		line := st.Action + " " + st.Section
		if st.Key != "" {
			line += " " + st.Key
		}
		if st.Status != "" {
			line += " [" + st.Status + "]"
		}
		fmt.Fprintln(w, line)
		for _, ch := range st.Changes {
			if ch.Secret {
				fmt.Fprintf(w, "    %s: (secret changed)\n", ch.Field)
				continue
			}
			fmt.Fprintf(w, "    %s: %s -> %s\n", ch.Field, ch.From, ch.To)
		}
		for _, x := range st.Warnings {
			fmt.Fprintln(w, "    warning: "+x)
		}
		if st.Error != "" {
			fmt.Fprintln(w, "    error: "+st.Error)
		}
	}
	for _, x := range res.Warnings {
		fmt.Fprintln(w, "warning: "+x)
	}
	verb := "applied"
	if res.DryRun {
		verb = "planned"
	}
	fmt.Fprintf(w, "%s: %d to create, %d to update, %d to delete, %d unchanged\n", verb,
		res.Summary["create"], res.Summary["update"], res.Summary["delete"], res.Summary["unchanged"])
	if !res.OK {
		fmt.Fprintf(w, "failed: %s; rolled back: %t\n", res.Error, res.RolledBack)
	}
}
