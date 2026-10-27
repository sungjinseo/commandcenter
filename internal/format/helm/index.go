package helm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

// maxIndex bounds an index.yaml read back from the store (bitnami's is
// ~30 MB; a hostile upstream could send anything). maxIndexChunk bounds
// what of it is decoded at once (see forEachEntry): one chart version, or
// the part outside the entries block; yaml.v3 needs ~15x a document's size
// while decoding it, so the memory of reading an index is a few x this,
// not of the index.
const (
	maxIndex      = 256 << 20
	maxIndexChunk = 2 << 20
)

// indexFile is index.yaml.
type indexFile struct {
	APIVersion string             `yaml:"apiVersion"`
	Entries    map[string][]entry `yaml:"entries"`
	Generated  time.Time          `yaml:"generated"`
}

func newIndex(now time.Time) *indexFile {
	return &indexFile{APIVersion: "v1", Entries: map[string][]entry{}, Generated: now.UTC()}
}

func (ix *indexFile) marshal() ([]byte, error) { return marshal(ix) }

// marshal is yaml.Marshal with the 2-space indent of helm repo index.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// parseIndex reads an index.yaml whole (tests and small indexes); the
// server streams them (forEachEntry, indexWriter).
func parseIndex(r io.Reader) (*indexFile, error) {
	entries := map[string][]entry{}
	hdr, err := forEachEntry(r, func(name string, e entry) error {
		entries[name] = append(entries[name], e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	hdr.Entries = entries
	return hdr, nil
}

// forEachEntry reads an index.yaml one chart version at a time, as helm repo
// index lays the file out (block style: the charts under `entries:`, one key
// each, every version one `- ` item), so a bitnami-sized index never sits in
// memory whole: fn gets each version with its chart's name, in file order.
// What is outside the entries block (apiVersion, generated; or entries in
// flow style, as the `entries: {}` of an empty index) is decoded whole and
// returned, so an index in another layout is still read, within
// maxIndexChunk. A line the layout does not explain is decoded with the
// version it follows, where yaml.v3 reports it.
func forEachEntry(r io.Reader, fn func(name string, e entry) error) (*indexFile, error) {
	br := bufio.NewReaderSize(io.LimitReader(r, maxIndex), maxIndexChunk)
	var head, chunk bytes.Buffer
	var name string
	inEntries := false
	keyIndent, itemIndent := -1, -1
	flush := func() error {
		if chunk.Len() == 0 {
			return nil
		}
		var vs []entry
		err := yaml.Unmarshal(chunk.Bytes(), &vs)
		chunk.Reset()
		if err != nil {
			return err
		}
		for _, e := range vs {
			if e != nil {
				if err := fn(name, e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("a line is longer than %d MiB", maxIndexChunk>>20)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if len(line) > 0 {
			trim := bytes.TrimSpace(line)
			indent := len(line) - len(bytes.TrimLeft(line, " "))
			item := len(trim) > 0 && trim[0] == '-' && (len(trim) == 1 || trim[1] == ' ' || trim[1] == '\t')
			switch {
			case len(trim) == 0 || trim[0] == '#': // blank or comment: part of what is being read
				if inEntries {
					chunk.Write(line)
				} else {
					head.Write(line)
				}
			case !inEntries:
				if rest, ok := bytes.CutPrefix(trim, []byte("entries:")); ok && indent == 0 && (len(rest) == 0 || rest[0] == ' ' || rest[0] == '\t') && (len(bytes.TrimSpace(rest)) == 0 || bytes.TrimSpace(rest)[0] == '#') {
					inEntries, keyIndent, itemIndent = true, -1, -1
				} else {
					head.Write(line)
				}
			case indent == 0: // the entries block ended (generated after it)
				if err := flush(); err != nil {
					return nil, err
				}
				inEntries = false
				head.Write(line)
			case keyIndent < 0 || indent == keyIndent && !item: // a chart's key, with its versions inline when in flow style
				if err := flush(); err != nil {
					return nil, err
				}
				keyIndent, itemIndent = indent, -1
				var kv map[string][]entry
				if err := yaml.Unmarshal(trim, &kv); err != nil || len(kv) != 1 {
					return nil, fmt.Errorf("entries is not a mapping of chart names at %q", trim)
				}
				for k, vs := range kv {
					name = k
					for _, e := range vs {
						if err := fn(name, e); err != nil {
							return nil, err
						}
					}
				}
			case item && (itemIndent < 0 || indent == itemIndent): // a version
				if err := flush(); err != nil {
					return nil, err
				}
				itemIndent = indent
				chunk.Write(line)
			default:
				chunk.Write(line)
			}
			if chunk.Len() > maxIndexChunk || head.Len() > maxIndexChunk {
				return nil, fmt.Errorf("a chart version, or the index outside its entries, is larger than %d MiB", maxIndexChunk>>20)
			}
		}
		if err != nil {
			break
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	var hdr indexFile
	if err := yaml.Unmarshal(head.Bytes(), &hdr); err != nil {
		return nil, err
	}
	for name, vs := range hdr.Entries {
		for _, e := range vs {
			if err := fn(name, e); err != nil {
				return nil, err
			}
		}
	}
	hdr.Entries = nil
	return &hdr, nil
}

// indexWriter writes an index.yaml a chart version at a time, in the layout
// of marshal (apiVersion, entries, generated), so a derived index (a proxy's
// rewritten upstream, a group's merge) is never held in memory whole. A
// chart's versions must come together, as forEachEntry gives them.
type indexWriter struct {
	w    io.Writer
	last string          // the chart whose versions are being written
	seen map[string]bool // every chart written
}

func (iw *indexWriter) add(name string, e entry) error {
	if iw.seen == nil {
		iw.seen = map[string]bool{}
		if _, err := io.WriteString(iw.w, "apiVersion: v1\nentries:\n"); err != nil {
			return err
		}
	}
	if name != iw.last && iw.seen[name] {
		return fmt.Errorf("chart %q is listed twice", name)
	}
	iw.seen[name] = true
	b, err := marshal(map[string][]entry{name: {e}}) // "<name>:\n" then the version
	if err != nil {
		return err
	}
	if name == iw.last {
		b = b[bytes.IndexByte(b, '\n')+1:]
	}
	iw.last = name
	out := make([]byte, 0, len(b)+2*bytes.Count(b, []byte("\n")))
	for len(b) > 0 { // nested under entries
		line, rest, _ := bytes.Cut(b, []byte("\n"))
		b = rest
		out = append(append(append(out, "  "...), line...), '\n')
	}
	_, err = iw.w.Write(out)
	return err
}

func (iw *indexWriter) close(generated time.Time) error {
	if iw.seen == nil {
		if _, err := io.WriteString(iw.w, "apiVersion: v1\nentries: {}\n"); err != nil {
			return err
		}
	}
	b, err := marshal(map[string]time.Time{"generated": generated.UTC()})
	if err != nil {
		return err
	}
	_, err = iw.w.Write(b)
	return err
}

// sortVersions orders a chart's versions newest first, as helm repo index does.
func sortVersions(vs []entry) {
	sort.SliceStable(vs, func(i, j int) bool {
		a, okA := semver.Parse(vs[i].str("version"))
		b, okB := semver.Parse(vs[j].str("version"))
		if okA != okB {
			return okA
		}
		return okA && semver.Compare(a, b) > 0
	})
}

// buildHosted generates a hosted repository's index.yaml from its rows, each
// row's Attrs being its entry (publish): urls relative to the repository,
// so the file resolves against whichever repository URL (its own or a
// group's) served the index.
func buildHosted(ctx context.Context, tx *meta.Tx, repoName string, now time.Time) ([]byte, error) {
	ix := newIndex(now)
	names, err := tx.ListPackageNames(ctx, repoName)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		pkgs, err := tx.ListPackages(ctx, repoName, name)
		if err != nil {
			return nil, err
		}
		for _, p := range pkgs {
			var e entry
			if err := json.Unmarshal([]byte(p.Attrs), &e); err != nil {
				return nil, err
			}
			ix.Entries[name] = append(ix.Entries[name], e)
		}
		sortVersions(ix.Entries[name])
	}
	return ix.marshal()
}

// upstream is what a proxy knows of its remote index: for each chart file
// the proxy serves, the remote URL and digest to fetch it with, and the
// chart names, for the owner rule. Keyed by the sha256 of the index it was
// read from, so a new upstream index replaces it (parseUpstream).
type upstream struct {
	sha    string
	charts map[string]upstreamChart // by ChartFile
	names  map[string]bool
}

type upstreamChart struct{ url, digest string }

// add turns one entry of a proxy's upstream index into the one the proxy
// serves: the chart gets one relative URL, its file here, so clients
// download through the proxy; what the remote URL was is kept in the
// table. An entry without a usable name, version or URL is dropped (false).
func (up *upstream) add(rp *repo.Repository, name string, e entry) bool {
	urls, _ := e["urls"].([]any)
	if len(urls) == 0 {
		return false
	}
	first, _ := urls[0].(string)
	ref, err := url.Parse(first)
	file := ChartFile(e.str("name"), e.str("version"))
	if first == "" || err != nil || e.str("name") != name || !fileRE.MatchString(file) {
		return false
	}
	up.charts[file] = upstreamChart{url: rp.RemoteURL.ResolveReference(ref).String(), digest: e.str("digest")}
	up.names[name] = true
	e["urls"] = []any{file}
	return true
}

// merge writes a group's index to w: for each chart name, the versions of
// the first member (srcs, each one's own index.yaml) that has it (the
// owner), as cargo and docker groups answer. The members' urls are already
// relative, and a file resolves against the group's URL to the group's
// download, which applies the same owner rule.
func (h *Handler) merge(srcs []meta.Asset, w io.Writer, now time.Time) error {
	iw := &indexWriter{w: w}
	owner := map[string]int{} // chart name -> index in srcs
	for i, a := range srcs {
		f, err := h.Store.Open(a)
		if err != nil {
			return err
		}
		_, err = forEachEntry(f, func(name string, e entry) error {
			if o, ok := owner[name]; ok && o != i {
				return nil
			}
			owner[name] = i
			return iw.add(name, e)
		})
		f.Close()
		if err != nil {
			return err
		}
	}
	return iw.close(now)
}

// fingerprint identifies the sources a derived index was made from.
func fingerprint(srcs []meta.Asset) string {
	h := sha256.New()
	for _, a := range srcs {
		io.WriteString(h, a.BlobSHA256+"\n")
	}
	return "derived:" + indexFormatVersion + ":" + hex.EncodeToString(h.Sum(nil))
}

// indexFormatVersion is mixed into every derived fingerprint; bump it when
// rewrite or merge changes what they produce from the same sources.
const indexFormatVersion = "1"

// indexAsset returns rp's index.yaml: a hosted repository's, which its
// writes keep up to date, or for a proxy or group one derived from its
// sources (a proxy's cached upstream index.yaml; each online member's own
// index.yaml, nested groups in place) that is stored under rp and reused
// until a source changes, as resolve.Merged does. Stale is set when a
// proxy's upstream could not be reached and its expired copy was used.
func (h *Handler) indexAsset(ctx context.Context, rp *repo.Repository) (proxy.Result, error) {
	var srcs []meta.Asset
	stale := false
	switch rp.Type {
	case "hosted":
		a, err := h.Store.Get(ctx, rp.Name, indexKey)
		return proxy.Result{Asset: a}, err
	case "proxy":
		res, err := h.fetchUpstream(ctx, rp)
		if err != nil {
			return proxy.Result{}, err
		}
		srcs, stale = []meta.Asset{res.Asset}, res.Stale
	case "group":
		for _, m := range rp.OnlineMembers() {
			res, err := h.indexAsset(ctx, m)
			if errors.Is(err, repo.ErrNotFound) && m.Type == "hosted" {
				continue // nothing was published to it yet (a proxy's 404 is its upstream's: a failure)
			}
			if err != nil {
				return proxy.Result{}, memberFailure(rp, m, err)
			}
			srcs = append(srcs, res.Asset)
			stale = stale || res.Stale
		}
	}
	fp := fingerprint(srcs)
	unlock := h.locks.Lock(rp.Name)
	defer unlock()
	if a, err := h.Store.Get(ctx, rp.Name, indexKey); err == nil && a.RemoteETag == fp {
		return proxy.Result{Asset: a, Stale: stale}, nil
	}
	// The derived index is streamed into the store: a goroutine writes it
	// while Put reads, so neither the sources nor the result sit in memory.
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		bw := bufio.NewWriterSize(pw, 64<<10) // a pipe write is a goroutine handoff
		var err error
		if rp.Type == "proxy" {
			_, err = h.parseUpstream(rp, srcs[0], bw)
		} else {
			err = h.merge(srcs, bw, h.Now())
		}
		if err == nil {
			err = bw.Flush()
		}
		pw.CloseWithError(err)
		done <- err
	}()
	a, err := h.Store.Put(ctx, rp.Name, indexKey, ctYAML, pr, func(a *meta.Asset) { a.RemoteETag = fp })
	pr.Close() // after a failed Put, so the writer stops
	if werr := <-done; werr != nil && !errors.Is(werr, io.ErrClosedPipe) {
		return proxy.Result{}, werr
	}
	return proxy.Result{Asset: a, Stale: stale}, err
}

func (h *Handler) fetchUpstream(ctx context.Context, rp *repo.Repository) (proxy.Result, error) {
	return h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: upstreamKey, Remote: indexKey, NotHTML: true}, proxy.Metadata)
}

// parseUpstream reads a proxy's cached upstream index (a), writing the
// index the proxy serves (rewritten, see upstream.add) to w and remembering
// the upstream table for the downloads of charts not cached yet (upstreamFor
// reuses it while the cached index is the same content). An unreadable
// upstream index is a 502.
func (h *Handler) parseUpstream(rp *repo.Repository, a meta.Asset, w io.Writer) (*upstream, error) {
	f, err := h.Store.Open(a)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	up := &upstream{sha: a.BlobSHA256, charts: map[string]upstreamChart{}, names: map[string]bool{}}
	iw := &indexWriter{w: w}
	var werr error // w's, not the index's
	hdr, err := forEachEntry(f, func(name string, e entry) error {
		if !up.add(rp, name, e) {
			return nil
		}
		werr = iw.add(name, e)
		return werr
	})
	if err == nil {
		generated := hdr.Generated
		if generated.IsZero() {
			generated = h.Now()
		}
		werr = iw.close(generated)
		err = werr
	}
	if err != nil {
		if werr != nil {
			return nil, werr
		}
		return nil, repo.Errorf(http.StatusBadGateway, "upstream index.yaml of %q is not valid YAML: %v", rp.Name, err)
	}
	h.upstreams.Store(rp.Name, up)
	return up, nil
}

// upstreamFor returns the upstream table of the proxy's current index.
func (h *Handler) upstreamFor(ctx context.Context, rp *repo.Repository) (*upstream, error) {
	res, err := h.fetchUpstream(ctx, rp)
	if err != nil {
		return nil, err
	}
	if up, ok := h.upstreams.Load(rp.Name); ok && up.(*upstream).sha == res.Asset.BlobSHA256 {
		return up.(*upstream), nil
	}
	return h.parseUpstream(rp, res.Asset, io.Discard)
}

// owns reports whether rp has chart name: a hosted repository by its rows,
// a proxy by its upstream index, a group by any online member.
func (h *Handler) owns(ctx context.Context, rp *repo.Repository, name string) (bool, error) {
	switch rp.Type {
	case "hosted":
		st, err := h.Store.DB.PackageStamp(ctx, rp.Name, name)
		return st.Count > 0, err
	case "proxy":
		up, err := h.upstreamFor(ctx, rp)
		if err != nil {
			return false, err
		}
		return up.names[name], nil
	}
	for _, m := range rp.OnlineMembers() {
		if ok, err := h.owns(ctx, m, name); ok || err != nil {
			return ok, err
		}
	}
	return false, nil
}
