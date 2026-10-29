// Command commandcenter is a low-memory package repository.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"commandcenter/internal/app"
	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/logring"
	"commandcenter/internal/meta"
)

var version = "dev"

const (
	defaultConfig = "/etc/commandcenter/config.yaml"
	// legacyConfig is where the product was called repolite; it is used only
	// when defaultConfig is missing and -config was not given.
	legacyConfig = "/etc/repolite/config.yaml"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "commandcenter:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	app.Version = version
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		args = append([]string{"serve"}, args...)
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "hash-password":
		return hashPassword(stdin, stdout)
	case "token":
		return token(args[1:], stdout)
	case "user":
		return user(args[1:], stdin, stdout)
	case "secret-key":
		return secretKey(args[1:], stdout)
	case "gc":
		return gc(args[1:], stdout)
	case "healthcheck":
		return healthcheck(args[1:])
	case "migrate-db":
		return migrateDB(args[1:], stdout)
	case "import-nexus":
		return importNexus(args[1:], stdout)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	}
	return fmt.Errorf("unknown command %q (commands: serve, hash-password, token create, user reset-password, secret-key rotate, gc, migrate-db, import-nexus, healthcheck, version)", args[0])
}

// pickConfigPath returns the config file to load. A -config given explicitly
// is used as is. Otherwise, when the default path is missing and the legacy
// path exists, the legacy path is used and warn is called.
func pickConfigPath(path string, explicit bool, legacy string, warn func(msg string)) string {
	if explicit {
		return path
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if _, err := os.Stat(legacy); err == nil {
			warn("config " + path + " not found; using legacy " + legacy + " (move it to " + path + ")")
			return legacy
		}
	}
	return path
}

// loadConfig parses -config (and -user, used by token create) and loads the config.
func loadConfig(name string, args []string) (*config.Config, string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	path := fs.String("config", defaultConfig, "config file")
	user := fs.String("user", "", "user name (token create)")
	if err := fs.Parse(args); err != nil {
		return nil, "", err
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})
	p := pickConfigPath(*path, explicit, legacyConfig, func(msg string) {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Warn(msg)
	})
	cfg, err := config.Load(p)
	if err != nil {
		return nil, "", err
	}
	return cfg, *user, nil
}

func serve(args []string) error {
	cfg, _, err := loadConfig("serve", args)
	if err != nil {
		return err
	}
	// The ring feeds the log viewer; the JSON handler still writes to stdout.
	ring := logring.NewRing(logring.DefaultRecords, logring.DefaultBytes)
	level := new(slog.LevelVar) // INFO; PUT /-/api/logging changes it until the next start
	log := slog.New(logring.NewHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}), ring))
	slog.SetDefault(log)
	if os.Getenv("GOMEMLIMIT") == "" && cfg.MemoryLimitMiB > 0 {
		debug.SetMemoryLimit(int64(cfg.MemoryLimitMiB) << 20)
	}
	a, err := app.New(cfg, log, app.WithLogRing(ring), app.WithLogLevel(level), app.AsServer())
	if err != nil {
		return err
	}
	defer a.Close()
	slog.SetDefault(log.With("node", a.Node))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.Serve(ctx)
}

func hashPassword(stdin io.Reader, stdout io.Writer) error {
	pw, err := readPassword(stdin) // the API's rules, as every other password entry point
	if err != nil {
		return err
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, h)
	return nil
}

func token(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: commandcenter token create -config PATH -user NAME")
	}
	cfg, user, err := loadConfig("token create", args[1:])
	if err != nil {
		return err
	}
	if user == "" {
		return errors.New("-user is required")
	}
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return err
	}
	defer a.Close()
	_, tok, err := a.Auth.CreateToken(context.Background(), user)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, tok)
	return nil
}

func gc(args []string, stdout io.Writer) error {
	cfg, _, err := loadConfig("gc", args)
	if err != nil {
		return err
	}
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return err
	}
	defer a.Close()
	unlock, err := a.Tasks.Acquire() // not beside a compact or cleanup on any node
	if err != nil {
		return err
	}
	defer unlock()
	n, _, err := a.GC(context.Background(), time.Hour)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "removed %d unreferenced blobs\n", n)
	return nil
}

func healthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:8081/service/rest/v1/status", "status URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(*url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

const resetUsage = "usage: commandcenter user reset-password -config PATH -user NAME [-admin] (the new password is read from stdin)"

// user runs the user subcommands; reset-password is the offline break-glass
// recovery (final review I4).
func user(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "reset-password" {
		return errors.New(resetUsage)
	}
	fs := flag.NewFlagSet("user reset-password", flag.ContinueOnError)
	path := fs.String("config", defaultConfig, "config file")
	name := fs.String("user", "", "user id")
	admin := fs.Bool("admin", false, "also give the user the admin role")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("-user is required; " + resetUsage)
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})
	p := pickConfigPath(*path, explicit, legacyConfig, func(msg string) { fmt.Fprintln(os.Stderr, msg) })
	cfg, err := config.Load(p)
	if err != nil {
		return err
	}
	pw, err := readPassword(stdin)
	if err != nil {
		return err
	}
	return resetPassword(context.Background(), cfg, *name, pw, *admin, stdout)
}

// readPassword reads one line from stdin and applies the API's rules.
func readPassword(stdin io.Reader) (string, error) {
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	pw := strings.TrimRight(line, "\r\n")
	if pw == "" {
		return "", errors.New("empty password (type the password and press Enter)")
	}
	return pw, auth.CheckNewPassword(pw)
}

// resetPassword opens the metadata database directly, runs the one-time
// config import if it has not happened yet, and applies
// meta.ResetLocalPassword. It prints every change. A running server need not
// be stopped: it reloads users, roles and settings every few seconds.
func resetPassword(ctx context.Context, cfg *config.Config, name, pw string, admin bool, stdout io.Writer) error {
	if !meta.ValidUserID(name) {
		return fmt.Errorf("invalid user id %q", name)
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	db, err := app.OpenDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	done, err := db.AccessImported(ctx)
	if err != nil {
		return err
	}
	if !done {
		// The reset must not be clobbered by (or block) the first start's import.
		if accessErr := errors.Join(cfg.AccessErr, cfg.SettingsErr); accessErr != nil {
			return accessErr
		}
		if _, err := db.ImportAccess(ctx, cfg); err != nil {
			return fmt.Errorf("import users, roles and settings: %w", err)
		}
		fmt.Fprintln(stdout, "imported users, roles and settings from the config file")
	}
	changes, err := db.ResetLocalPassword(ctx, name, hash, admin)
	if err != nil {
		return err
	}
	for _, c := range changes {
		fmt.Fprintln(stdout, c)
	}
	fmt.Fprintln(stdout, "done; sign in as "+name+" (a running server applies the change within a few seconds)")
	return nil
}

const rotateUsage = "usage: commandcenter secret-key rotate -config PATH -new-key-file FILE (stop the server first: it keeps " +
	"the key it started with; the current key comes from COMMANDCENTER_SECRET_KEY or secretKeyFile, as for serve; " +
	"then start the server with the new key)"

// secretKey runs the secret-key subcommands; rotate seals the stored secrets
// with a new key, offline like user reset-password.
func secretKey(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "rotate" {
		return errors.New(rotateUsage)
	}
	fs := flag.NewFlagSet("secret-key rotate", flag.ContinueOnError)
	path := fs.String("config", defaultConfig, "config file")
	newKeyFile := fs.String("new-key-file", "", "file holding the new key, 32 random bytes in base64 (required; "+
		"stop the server first and start it with this key afterwards)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *newKeyFile == "" {
		return errors.New("-new-key-file is required; " + rotateUsage)
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})
	p := pickConfigPath(*path, explicit, legacyConfig, func(msg string) { fmt.Fprintln(os.Stderr, msg) })
	cfg, err := config.Load(p)
	if err != nil {
		return err
	}
	resealed, plain, err := app.RotateSecretKey(cfg, *newKeyFile)
	if err != nil {
		return err
	}
	// Counts only: never a key or a secret.
	fmt.Fprintf(stdout, "re-encrypted %d stored secrets with the new key and encrypted %d that were plain text\n", resealed, plain)
	fmt.Fprintln(stdout, "done; start the server with the new key in COMMANDCENTER_SECRET_KEY or secretKeyFile")
	return nil
}

const migrateUsage = "usage: commandcenter migrate-db -config PATH -to TARGET (TARGET: the postgres:// URL of an empty database, " +
	"or the path of a new SQLite file for the reverse direction; stop the server first; the source is the database the " +
	"config names, as for serve)"

// migrateDB copies the metadata database the config names into the empty
// database -to names, offline like user reset-password. Secrets copy as
// stored, so the same secret key serves the target. Neither URL is ever
// printed.
func migrateDB(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("migrate-db", flag.ContinueOnError)
	path := fs.String("config", defaultConfig, "config file")
	to := fs.String("to", "", "target database: a postgres:// URL or an SQLite file path (required; must be empty)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == "" {
		return errors.New("-to is required; " + migrateUsage)
	}
	if strings.Contains(*to, "://") && !meta.IsPostgresURL(*to) {
		return errors.New("-to must be a postgres:// or postgresql:// URL, or the path of an SQLite file; " + migrateUsage)
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})
	p := pickConfigPath(*path, explicit, legacyConfig, func(msg string) { fmt.Fprintln(os.Stderr, msg) })
	cfg, err := config.Load(p)
	if err != nil {
		return err
	}
	src, err := app.OpenDB(cfg)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := meta.Open(*to) // its errors never hold the URL
	if err != nil {
		return fmt.Errorf("open target database: %w", err)
	}
	defer dst.Close()
	counts, err := meta.Copy(context.Background(), src, dst)
	if err != nil {
		return fmt.Errorf("migrate (nothing was written): %w", err)
	}
	var total int64
	for _, c := range counts {
		fmt.Fprintf(stdout, "%-18s %d\n", c.Table, c.Rows)
		total += c.Rows
	}
	fmt.Fprintf(stdout, "done; copied %d rows in %d tables; point database.url (or COMMANDCENTER_DATABASE_URL) at the target "+
		"and start the server with the same secret key\n", total, len(counts))
	return nil
}
