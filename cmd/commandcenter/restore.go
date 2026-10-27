package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"commandcenter/internal/backup"
	"commandcenter/internal/config"
)

const restoreUsage = "usage: commandcenter restore --from BACKUP_DIR/NAME --data DATA_DIR [--force] [--allow-missing] [--allow-absolute-stores] [--config FILE]"

// restoreCmd restores a backup into a data directory, offline (plan 14
// Decision 4). It refuses while a server uses the data directory, and a
// non-empty directory unless --force, which moves the old contents into
// <data>/.before-restore-<time>/.
func restoreCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "the backup directory, <backupDir>/<name>")
	data := fs.String("data", "", "the data directory to restore into (must not be in use by a server)")
	force := fs.Bool("force", false, "restore into a non-empty data directory, moving its contents to .before-restore-<time> inside it")
	allowAbs := fs.Bool("allow-absolute-stores", false, "write blob stores that have absolute paths in the backup to those paths")
	cfgPath := fs.String("config", defaultConfig, "config file, read only to refuse a PostgreSQL metadata database")
	allowMissing := fs.Bool("allow-missing", false, "succeed although blobs are missing or corrupt in the backup")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%v; %s", err, restoreUsage)
	}
	if fs.NArg() > 0 || *from == "" || *data == "" {
		return errors.New(restoreUsage)
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})
	if usesPostgres(pickConfigPath(*cfgPath, explicit, legacyConfig, func(string) {})) {
		return backup.ErrPostgres
	}
	res, err := backup.Restore(context.Background(), backup.RestoreOptions{
		From: *from, Data: *data, Force: *force, AllowMissing: *allowMissing, AllowAbsoluteStores: *allowAbs, Log: stderrLog{},
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "restored backup %s into %s\n", res.Name, *data)
	fmt.Fprintf(stdout, "  blobs: %d (%d bytes)\n", res.CopiedBlobs, res.Bytes)
	names := make([]string, 0, len(res.Stores))
	for n := range res.Stores {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(stdout, "  blob store %s: %s\n", n, res.Stores[n])
	}
	if res.MovedTo != "" {
		fmt.Fprintf(stdout, "  the old contents were moved to %s (delete it when the restore is confirmed)\n", res.MovedTo)
	}
	if res.Missing > 0 {
		fmt.Fprintf(stdout, "  MISSING blobs: %d (restored anyway because of --allow-missing)\n", res.Missing)
		for _, l := range res.MissingList {
			fmt.Fprintf(stdout, "    %s\n", l)
		}
	}
	return nil
}

// usesPostgres reports whether the metadata database is PostgreSQL: the
// COMMANDCENTER_DATABASE_URL environment variable or database.url in the
// config file (a missing or unreadable file counts as SQLite, the default).
func usesPostgres(configFile string) bool {
	if os.Getenv("COMMANDCENTER_DATABASE_URL") != "" {
		return true
	}
	cfg, err := config.Load(configFile)
	return err == nil && cfg.Database.URL != ""
}

// stderrLog prints the restore's log lines to stderr.
type stderrLog struct{}

func (stderrLog) Infof(f string, a ...any)  { fmt.Fprintf(os.Stderr, f+"\n", a...) }
func (stderrLog) Warnf(f string, a ...any)  { fmt.Fprintf(os.Stderr, "warning: "+f+"\n", a...) }
func (stderrLog) Errorf(f string, a ...any) { fmt.Fprintf(os.Stderr, "error: "+f+"\n", a...) }
