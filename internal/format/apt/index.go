package apt

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/pgp"
	"commandcenter/internal/repo"
)

// loadRows reads every package row of a hosted repository, in the
// transaction that changed them.
func loadRows(ctx context.Context, tx *meta.Tx, repoName string) ([]row, error) {
	names, err := tx.ListPackageNames(ctx, repoName)
	if err != nil {
		return nil, err
	}
	var rows []row
	for _, name := range names {
		pkgs, err := tx.ListPackages(ctx, repoName, name)
		if err != nil {
			return nil, err
		}
		for _, p := range pkgs {
			r, err := parseRow(p.Attrs)
			if err != nil {
				return nil, fmt.Errorf("package %s %s: %w", p.Name, p.Version, err)
			}
			rows = append(rows, r)
		}
	}
	return rows, nil
}

// build writes the dists/<distribution>/ files of rows: a Packages and
// Packages.gz per architecture (packages of architecture "all" listed in
// every one), the Release that lists them with their sizes and checksums
// and, when rp has a signing key, its detached (Release.gpg) and cleartext
// (InRelease) signatures. Paths are relative to the repository. The same
// rows give the same bytes, except for Release's Date.
func build(rp *repo.Repository, rows []row, now time.Time) (map[string][]byte, error) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Package != rows[j].Package {
			return rows[i].Package < rows[j].Package
		}
		if rows[i].Version != rows[j].Version {
			return rows[i].Version < rows[j].Version
		}
		return rows[i].Architecture < rows[j].Architecture
	})
	archs := map[string]bool{}
	for _, r := range rows {
		archs[r.Architecture] = true
	}
	var archList []string
	for a := range archs {
		archList = append(archList, a)
	}
	sort.Strings(archList)

	dist, comp := "dists/"+rp.Distribution+"/", rp.Component
	files := map[string][]byte{}
	var sums []string // the Release file list, one line without its hash per file
	type listed struct{ path, body string }
	var list []listed
	for _, arch := range archList {
		var sb strings.Builder
		for _, r := range rows {
			if r.Architecture == arch || r.Architecture == "all" {
				sb.WriteString(r.stanza() + "\n")
			}
		}
		p := comp + "/binary-" + arch + "/Packages"
		list = append(list, listed{p, sb.String()}, listed{p + ".gz", string(gzipBytes([]byte(sb.String())))})
	}
	for _, l := range list {
		files[dist+l.path] = []byte(l.body)
		sums = append(sums, fmt.Sprintf(" %%s %16d %s\n", len(l.body), l.path))
	}
	var rel strings.Builder
	rel.WriteString("Origin: " + rp.Name + "\nLabel: " + rp.Name + "\nSuite: " + rp.Distribution + "\nCodename: " + rp.Distribution +
		"\nDate: " + now.UTC().Format(time.RFC1123) + "\nArchitectures: " + strings.Join(archList, " ") + "\nComponents: " + comp +
		"\nDescription: Command Center apt repository " + rp.Name + "\n")
	for _, h := range []struct {
		name string
		sum  func([]byte) string
	}{
		{"MD5Sum", func(b []byte) string { s := md5.Sum(b); return hex.EncodeToString(s[:]) }},
		{"SHA1", func(b []byte) string { s := sha1.Sum(b); return hex.EncodeToString(s[:]) }},
		{"SHA256", func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }},
	} {
		rel.WriteString(h.name + ":\n")
		for i, l := range list {
			rel.WriteString(fmt.Sprintf(sums[i], h.sum([]byte(l.body))))
		}
	}
	release := []byte(rel.String())
	files[dist+"Release"] = release
	if rp.SigningKey != "" {
		key, err := pgp.ParsePrivateKey(rp.SigningKey)
		if err != nil {
			return nil, fmt.Errorf("repository %q: signingKey: %w", rp.Name, err)
		}
		sig, err := key.DetachSign(release, now)
		if err != nil {
			return nil, err
		}
		in, err := key.ClearSign(release, now)
		if err != nil {
			return nil, err
		}
		files[dist+"Release.gpg"], files[dist+"InRelease"] = []byte(sig), []byte(in)
	}
	return files, nil
}

// contentType is the media type of a repository path: what apt and
// browsers expect for the metadata and the packages.
func contentType(p string) string {
	switch base := p[strings.LastIndex(p, "/")+1:]; {
	case strings.HasSuffix(base, ".deb"):
		return ctDeb
	case strings.HasSuffix(base, ".gz"):
		return "application/gzip"
	case strings.HasSuffix(base, ".xz"):
		return "application/x-xz"
	case strings.HasSuffix(base, ".bz2"):
		return "application/x-bzip2"
	case base == "Release.gpg":
		return "application/pgp-signature"
	case base == "Release", base == "InRelease", base == "Packages", base == "Sources", strings.HasPrefix(base, "Contents-"), strings.HasPrefix(base, "Translation-"):
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}
