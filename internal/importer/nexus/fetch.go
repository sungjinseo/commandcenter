package nexus

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // fallback verification only when the source gives no sha256
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"net/url"
	"strings"
)

// downloadURL resolves where to fetch an asset. A downloadUrl on the
// configured source is used as is. One naming another host (the source's
// own, possibly differently configured, base URL) is never contacted, so
// credentials cannot leave the source: the URL is rebuilt on the configured
// base from the repository and path, which keeps the base's context path.
func (c *Client) downloadURL(a Asset) string {
	if a.DownloadURL != "" {
		if u, err := url.Parse(a.DownloadURL); err == nil && u.Host != "" {
			if strings.EqualFold(u.Scheme, c.base.Scheme) && strings.EqualFold(u.Host, c.base.Host) {
				u.User, u.Fragment = nil, ""
				return u.String()
			}
			if a.Repository != "" && a.Path != "" {
				return c.repoURL(a.Repository, a.Path)
			}
			r := *c.base
			r.Path, r.RawPath, r.RawQuery = u.Path, u.RawPath, u.RawQuery
			return r.String()
		}
	}
	return c.repoURL(a.Repository, a.Path)
}

// repoURL is base/repository/{repo}/{path}, path-escaped per segment.
func (c *Client) repoURL(repo, p string) string {
	segs := strings.Split(strings.TrimLeft(p, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return c.base.String() + "/repository/" + url.PathEscape(repo) + "/" + strings.Join(segs, "/")
}

// Download streams an asset. The returned reader verifies the source's
// sha256 (sha1 when no sha256 is given) and returns ErrChecksum from the Read
// that reaches EOF on a mismatch. An asset with no checksum is not verified.
// Closing the reader releases the connection and the per-asset timeout.
func (c *Client) Download(ctx context.Context, a Asset) (io.ReadCloser, error) {
	return c.DownloadCapped(ctx, a, 0)
}

// DownloadCapped is Download with a size cap: a body larger than limit
// bytes (limit > 0) fails with ErrTooLarge, from the Content-Length or from
// the Read that passes the cap.
func (c *Client) DownloadCapped(ctx context.Context, a Asset, limit int64) (io.ReadCloser, error) {
	ctx, cancel := context.WithTimeout(ctx, c.downloadTimeout)
	raw := c.downloadURL(a)
	resp, err := c.get(ctx, "download", raw, "")
	if err != nil {
		cancel()
		return nil, err
	}
	if limit > 0 && resp.ContentLength > limit {
		resp.Body.Close()
		cancel()
		return nil, &Error{Kind: ErrTooLarge, Op: "download", URL: raw}
	}
	v := &verifyReader{rc: resp.Body, cancel: cancel, url: raw, limit: limit}
	switch {
	case a.Checksum.SHA256 != "":
		v.h, v.want = sha256.New(), strings.ToLower(a.Checksum.SHA256)
	case a.Checksum.SHA1 != "":
		v.h, v.want = sha1.New(), strings.ToLower(a.Checksum.SHA1) //nolint:gosec
	}
	return v, nil
}

type verifyReader struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	h      hash.Hash // nil: no checksum known
	want   string
	url    string
	limit  int64 // 0: none
	read   int64
	done   bool
	err    error
}

func (v *verifyReader) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	n, err := v.rc.Read(p)
	v.read += int64(n)
	if v.limit > 0 && v.read > v.limit {
		v.err = &Error{Kind: ErrTooLarge, Op: "download", URL: v.url}
		return 0, v.err
	}
	if v.h != nil {
		v.h.Write(p[:n])
	}
	if err == io.EOF {
		if v.h != nil && !v.done {
			v.done = true
			if hex.EncodeToString(v.h.Sum(nil)) != v.want {
				v.err = &Error{Kind: ErrChecksum, Op: "download", URL: v.url}
				return n, v.err
			}
		}
		return n, io.EOF
	}
	if err != nil {
		v.err = &Error{Kind: ErrNetwork, Op: "download", URL: v.url, Err: errors.New(safeErr(err))}
		return n, v.err
	}
	return n, nil
}

func (v *verifyReader) Close() error {
	err := v.rc.Close()
	v.cancel()
	return err
}

// ---- npm packument ----

// Packument is the part of an npm packument the importer applies: the
// dist-tags and which versions are deprecated (version -> message).
type Packument struct {
	DistTags   map[string]string
	Deprecated map[string]string
}

// NpmPackument fetches {source}/repository/{repo}/{name} (bounded) and keeps
// only dist-tags and each version's deprecated message.
func (c *Client) NpmPackument(ctx context.Context, repo, name string) (*Packument, error) {
	raw := c.base.String() + "/repository/" + url.PathEscape(repo) + "/" + strings.Replace(url.PathEscape(name), "/", "%2F", 1)
	b, err := c.getBounded(ctx, "GET packument", raw, "application/json", MaxPackumentBytes)
	if err != nil {
		return nil, err
	}
	var doc struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Deprecated any `json:"deprecated"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, &Error{Kind: ErrProtocol, Op: "GET packument", URL: raw, Err: errors.New("response is not a packument")}
	}
	p := &Packument{DistTags: doc.DistTags, Deprecated: map[string]string{}}
	if p.DistTags == nil {
		p.DistTags = map[string]string{}
	}
	for v, d := range doc.Versions {
		if s, ok := d.Deprecated.(string); ok && s != "" {
			p.Deprecated[v] = s
		}
	}
	return p, nil
}

// ---- cargo sparse index ----

// CargoIndexPath is the sparse-index path of a crate: "1/n", "2/nn",
// "3/n/nnn" and "ab/cd/name" (lowercased name).
func CargoIndexPath(name string) string {
	n := strings.ToLower(name)
	switch len(n) {
	case 0:
		return ""
	case 1:
		return "1/" + n
	case 2:
		return "2/" + n
	case 3:
		return "3/" + n[:1] + "/" + n
	}
	return n[:2] + "/" + n[2:4] + "/" + n
}

// IndexLine is one version line of a crate's sparse index.
type IndexLine struct {
	Raw    []byte // the JSON line as served, without the newline
	Name   string
	Vers   string
	Cksum  string // sha256 of the .crate
	Yanked bool
}

// CargoIndexLine fetches {source}/repository/{repo}/index/{prefix}/{name}
// (bounded) and returns the line whose vers equals vers. ErrNotFound when
// the file or the version is absent.
func (c *Client) CargoIndexLine(ctx context.Context, repo, name, vers string) (*IndexLine, error) {
	p := CargoIndexPath(name)
	if p == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return nil, &Error{Kind: ErrProtocol, Op: "cargo index", Err: errors.New("invalid crate name")}
	}
	raw := c.repoURL(repo, "index/"+p)
	b, err := c.getBounded(ctx, "GET cargo index", raw, "", MaxIndexBytes)
	if err != nil {
		return nil, err
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var l struct {
			Name   string `json:"name"`
			Vers   string `json:"vers"`
			Cksum  string `json:"cksum"`
			Yanked bool   `json:"yanked"`
		}
		if json.Unmarshal(line, &l) != nil || l.Vers != vers {
			continue
		}
		return &IndexLine{Raw: append([]byte(nil), line...), Name: l.Name, Vers: l.Vers, Cksum: strings.ToLower(l.Cksum), Yanked: l.Yanked}, nil
	}
	return nil, &Error{Kind: ErrNotFound, Op: "cargo index", URL: raw, Err: errors.New("version not in index")}
}
