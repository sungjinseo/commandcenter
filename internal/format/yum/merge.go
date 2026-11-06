package yum

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
	"commandcenter/internal/xz"
	"commandcenter/internal/zstd"
)

// A group with mergeMembers answers GET <dir>repodata/repomd.xml with the
// merged metadata of the members that have that file, so that one baseurl
// shows the packages of all of them. A path only one member has is that
// member's, unchanged; RPMs are found by path as in any group.
//
// Every GET looks up the members' repomd.xml (a proxy's through its own
// metadataMaxAge) and compares their fingerprint with the merged copy stored
// in the group's namespace, so all nodes answer alike and a member's change
// shows at once. Only a change merges again, one at a time per path across
// the nodes, detached from the request: dnf gives up on a server silent for
// its timeout (30 seconds), asks a few times, and fails; the merge goes on, so
// whoever asks next finds it done. A request with a previous merge to answer
// with waits mergeWait at most.
//
// A merged index is complete or it is not served. A member that fails, or
// whose metadata this server cannot read, fails the merge (logged, and not
// tried again for mergeRetry): the group then answers with the previous
// merged copy and a Warning, or with a 502 when there is none.
//
// The merge streams. primary is read twice, filelists and other once, one
// <package> at a time, and none is held: a package of a real primary.xml runs
// to a megabyte (a kernel's provides), a document to hundreds. What it keeps
// is a hash per package and path.

const (
	repomdFile   = "repodata/repomd.xml"
	mergeTimeout = 30 * time.Minute
	mergeRetry   = time.Minute
	mergeWorkers = 2 // merges at once on a node: each takes a CPU and a few MiB
	mergeBuf     = 64 << 10
	maxRepomd    = 4 << 20
	maxTag       = 64 << 10
	maxHead      = 4 << 20 // a package up to its <format>
	// ponytail: a merge remembers up to 100 bytes per package; at this many it
	// is refused rather than risk the memory limit. Spill to a file if groups
	// of millions of packages appear.
	maxMergePackages = 500_000
)

// mergeWait is how long a request waits for a merge when it can answer with
// the previous merged copy instead; a var for tests.
var mergeWait = 10 * time.Second

// mergedFile answers a repodata file of group rp with mergeMembers: repomd.xml
// merged, the merged data files from the group's namespace, the rest from the
// first member that has it. A member's signature does not sign a merge.
func (h *Handler) mergedFile(ctx context.Context, rp *repo.Repository, p string) (proxy.Result, error) {
	base := path.Base(p)
	dir, direct := strings.CutSuffix(p, "repodata/"+base)
	direct = direct && (dir == "" || strings.HasSuffix(dir, "/"))
	switch {
	case direct && base == "repomd.xml":
		return h.mergedRepomd(ctx, rp, dir)
	case direct && strings.HasPrefix(base, "repomd.xml."):
		switch members, _, err := h.membersAt(ctx, rp, dir); {
		case err != nil:
			return proxy.Result{}, err
		case len(members) > 1:
			return proxy.Result{}, repo.ErrNotFound
		}
	default:
		switch a, err := h.Store.Get(ctx, rp.Name, p); {
		case err == nil:
			return proxy.Result{Asset: a}, nil
		case !errors.Is(err, repo.ErrNotFound):
			return proxy.Result{}, err
		}
	}
	return h.Resolver.LookupTarget(ctx, rp, proxy.Target{Key: p, Remote: p, NotHTML: true}, proxy.Metadata)
}

// mergeMember is a member with dir's repomd.xml.
type mergeMember struct {
	repo   *repo.Repository
	repomd meta.Asset
}

// membersAt looks up dir's repomd.xml in every online leaf of rp, in order.
// One that does not have it is left out, as it does not use that path. One
// that fails is an error, a remote that refuses too (DeniedIsFailure): leaving
// it out would be a partial index. stale is set when a proxy's copy is one its
// remote could not confirm.
func (h *Handler) membersAt(ctx context.Context, rp *repo.Repository, dir string) (members []mergeMember, stale bool, err error) {
	key := dir + repomdFile
	for _, m := range resolve.Leaves(rp) {
		res, err := h.Resolver.LookupTarget(ctx, m, proxy.Target{Key: key, Remote: key, NotHTML: true, DeniedIsFailure: true}, proxy.Metadata)
		switch {
		case err == nil:
			members = append(members, mergeMember{m, res.Asset})
			stale = stale || res.Stale
		case repo.StatusOf(err) != http.StatusNotFound:
			return nil, false, fmt.Errorf("member %q: %w", m.Name, err)
		}
	}
	return members, stale, nil
}

// mergeFingerprint names the members and what they said, in order: a merged
// copy stands while it is the same.
func mergeFingerprint(members []mergeMember) string {
	h := sha256.New()
	for _, m := range members {
		fmt.Fprintf(h, "%s\n%s\n", m.repo.Name, m.repomd.BlobSHA256)
	}
	return "group:yum1:" + hex.EncodeToString(h.Sum(nil))
}

// mergedRepomd answers dir's repomd.xml of group rp (see the top of this file).
func (h *Handler) mergedRepomd(ctx context.Context, rp *repo.Repository, dir string) (proxy.Result, error) {
	stored, err := h.Store.Get(ctx, rp.Name, dir+repomdFile)
	merged := err == nil
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return proxy.Result{}, err
	}
	fallback := func(cause error) (proxy.Result, error) {
		if merged {
			return proxy.Result{Asset: stored, Stale: true}, nil
		}
		return proxy.Result{}, cause
	}
	members, stale, err := h.membersAt(ctx, rp, dir)
	if err != nil {
		slog.Warn("yum group cannot read a member's repodata", "group", rp.Name, "dir", dir, "err", logsafe.Err(err))
		return fallback(err)
	}
	switch len(members) {
	case 0:
		return proxy.Result{}, repo.ErrNotFound
	case 1:
		return proxy.Result{Asset: members[0].repomd, Stale: stale}, nil
	}
	fp := mergeFingerprint(members)
	if merged && stored.RemoteETag == fp {
		return proxy.Result{Asset: stored, Stale: stale}, nil
	}
	failed := repo.Errorf(http.StatusBadGateway, "the members of group %q cannot be merged at %q: see the server's log", rp.Name, "/"+dir)
	if h.failedRecently(rp, dir, fp) {
		return fallback(failed)
	}
	ch := h.merges.DoChan(rp.Name+"\x00"+strconv.FormatUint(rp.Gen, 10)+"\x00"+dir+"\x00"+fp, func() (any, error) {
		return h.merge(context.WithoutCancel(ctx), rp, dir)
	})
	var wait <-chan time.Time
	if merged {
		t := time.NewTimer(mergeWait)
		defer t.Stop()
		wait = t.C
	}
	select {
	case r := <-ch:
		if r.Err != nil {
			return fallback(failed)
		}
		return proxy.Result{Asset: r.Val.(meta.Asset), Stale: stale}, nil
	case <-wait:
		return proxy.Result{Asset: stored, Stale: true}, nil
	case <-ctx.Done():
		return proxy.Result{}, ctx.Err()
	}
}

// failedMerge is the last failed merge of a path on this node: only saves
// work, so that nodes may differ on it.
type failedMerge struct {
	fp string
	at time.Time
}

func (h *Handler) failedRecently(rp *repo.Repository, dir, fp string) bool {
	h.failMu.Lock()
	defer h.failMu.Unlock()
	f, ok := h.failed[rp.Name+"\x00"+dir]
	return ok && f.fp == fp && h.Now().Sub(f.at) < mergeRetry
}

// merge makes dir's merged metadata under the lock that keeps other nodes
// from doing the same, unless one has done it meanwhile. It looks at the
// members again: they may have changed while it waited.
func (h *Handler) merge(ctx context.Context, rp *repo.Repository, dir string) (a meta.Asset, err error) {
	fp := ""
	defer func() { // the input is a remote's, and this is not a request's goroutine
		if p := recover(); p != nil {
			err = fmt.Errorf("merge panicked: %v", p)
		}
		h.failMu.Lock()
		defer h.failMu.Unlock()
		if err != nil {
			slog.Error("yum group merge failed", "group", rp.Name, "dir", dir, "err", logsafe.Err(err))
			h.failed[rp.Name+"\x00"+dir] = failedMerge{fp, h.Now()}
			return
		}
		delete(h.failed, rp.Name+"\x00"+dir)
	}()
	ctx, cancel := context.WithTimeout(ctx, mergeTimeout)
	defer cancel()
	unlock, err := h.Store.Lock(ctx, "yum\x00merge\x00"+rp.Name+"\x00"+dir)
	if err != nil {
		return meta.Asset{}, err
	}
	defer unlock()
	members, _, err := h.membersAt(ctx, rp, dir)
	if err != nil {
		return meta.Asset{}, err
	}
	switch len(members) {
	case 0:
		return meta.Asset{}, repo.ErrNotFound
	case 1:
		return members[0].repomd, nil
	}
	fp = mergeFingerprint(members)
	stored, err := h.Store.Get(ctx, rp.Name, dir+repomdFile)
	if err == nil && stored.RemoteETag == fp {
		return stored, nil
	}
	t0 := time.Now()
	inputs, carried, err := h.mergeInputs(ctx, rp, dir, members) // what a proxy lacks comes from its remote here
	if err != nil {
		return meta.Asset{}, err
	}
	select {
	case h.mergeSlots <- struct{}{}:
		defer func() { <-h.mergeSlots }()
	case <-ctx.Done():
		return meta.Asset{}, ctx.Err()
	}
	written, st, err := h.mergeDocs(ctx, rp, dir, inputs)
	if err != nil {
		return meta.Asset{}, err
	}
	data := make([]repoData, len(written))
	for i, d := range written {
		data[i] = d.repoData
	}
	sums, err := h.Store.WriteBlob(rp.Name, strings.NewReader(Repomd(data, h.Now(), carried...)), nil)
	if err != nil {
		return meta.Asset{}, err
	}
	a = h.Store.NewAsset(rp.Name, dir+repomdFile, ctXML, sums)
	a.RemoteETag = fp
	// The previous data files stay until the next merge: a client that has
	// just read the previous repomd.xml still gets them.
	keep := map[string]bool{a.Path: true}
	if stored.BlobSHA256 != "" {
		if doc, err := h.readRepomd(stored); err == nil {
			for _, d := range doc.Data {
				keep[dir+d.Location.Href] = true
			}
		}
	}
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		for _, d := range written {
			keep[dir+d.file()] = true
			if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, dir+d.file(), ctGzip, d.sums)); err != nil {
				return err
			}
		}
		if err := tx.PutAsset(ctx, a); err != nil {
			return err
		}
		old, err := tx.ListAssets(ctx, rp.Name, dir+"repodata/")
		if err != nil {
			return err
		}
		for _, o := range old {
			if !keep[o.Path] {
				if _, err := tx.DeleteAsset(ctx, rp.Name, o.Path); err != nil && !errors.Is(err, meta.ErrNotFound) {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return meta.Asset{}, err
	}
	slog.Info("yum group merged", "group", rp.Name, "dir", dir, "members", len(members), "packages", st.packages,
		"duplicates", st.duplicates, "seconds", time.Since(t0).Round(time.Millisecond).Seconds())
	return a, nil
}

// mergeStats counts what a merge did with the packages it read.
type mergeStats struct{ packages, duplicates int }

// mergeInput is what a merge knows of one member.
type mergeInput struct {
	repo  *repo.Repository
	files map[string]meta.Asset // primary, filelists, other
	drop  []int                 // the packages of its primary to leave out, by place, ascending
	gone  map[mergeHash]bool    // their pkgids, for its filelists and other
}

// mergedDoc is a data file a merge stored.
type mergedDoc struct {
	repoData
	sums blob.Sums
}

// mergeHash stands for a package's name or file in memory.
type mergeHash [16]byte

func mergeKey(parts ...string) mergeHash {
	h := sha256.New()
	for _, s := range parts {
		io.WriteString(h, s)
		h.Write([]byte{0})
	}
	var k mergeHash
	copy(k[:], h.Sum(nil))
	return k
}

// mergeInputs reads the members' repomd.xml and finds their documents. It
// also returns the <data> elements a merge passes on: each of group (comps),
// updateinfo and modules comes from the first member that lists it, whole, and
// is found by path, not merged. A member's other types describe that member
// alone and are left out, above all the sqlite and zchunk forms of the merged
// documents, which a client would prefer.
func (h *Handler) mergeInputs(ctx context.Context, rp *repo.Repository, dir string, members []mergeMember) ([]*mergeInput, []string, error) {
	var inputs []*mergeInput
	var carried []string
	taken := map[string]bool{}
	for _, mm := range members {
		doc, err := h.readRepomd(mm.repomd)
		if err != nil {
			return nil, nil, fmt.Errorf("member %q: its repomd.xml: %w", mm.repo.Name, err)
		}
		in := &mergeInput{repo: mm.repo, files: map[string]meta.Asset{}, gone: map[mergeHash]bool{}}
		here := map[string]bool{}
		for _, d := range doc.Data {
			if docs[d.Type].root != "" {
				a, err := h.memberFile(ctx, mm.repo, dir, d)
				if err != nil {
					return nil, nil, fmt.Errorf("member %q: its %s: %w", mm.repo.Name, d.Type, err)
				}
				in.files[d.Type] = a
				continue
			}
			family, form, _ := strings.Cut(d.Type, "_")
			if !carriedTypes[family] || form == "zck" || form == "db" || taken[family] {
				continue
			}
			if _, ok := groupPath(dir, d.Location.Href); !ok || d.Location.Base != "" || strings.Count(d.Inner, "<location") != 1 {
				slog.Warn("yum group merge leaves out a file the group cannot answer", "group", rp.Name, "member", mm.repo.Name, "type", d.Type)
				continue
			}
			var w xw
			w.WriteString("  <data")
			w.attr("type", d.Type)
			w.WriteString(">" + d.Inner + "</data>\n")
			carried = append(carried, w.String())
			here[family] = true
		}
		for family := range here {
			taken[family] = true
		}
		if _, ok := in.files["primary"]; !ok {
			return nil, nil, fmt.Errorf("member %q: its repomd.xml lists no primary", mm.repo.Name)
		}
		inputs = append(inputs, in)
	}
	return inputs, carried, nil
}

// mergeDocs writes the merged primary, filelists and other of inputs.
func (h *Handler) mergeDocs(ctx context.Context, rp *repo.Repository, dir string, inputs []*mergeInput) ([]mergedDoc, mergeStats, error) {
	var st mergeStats
	seen := map[mergeHash]int32{}
	for i, in := range inputs {
		if err := h.scanMember(ctx, dir, in, int32(i), seen, &st); err != nil {
			return nil, st, fmt.Errorf("member %q: its primary: %w", in.repo.Name, err)
		}
	}
	var out []mergedDoc
	for _, typ := range []string{"primary", "filelists", "other"} {
		d, err := h.writeDoc(ctx, rp, typ, dir, st.packages, inputs)
		if err != nil {
			return nil, st, fmt.Errorf("%s: %w", typ, err)
		}
		out = append(out, d)
	}
	return out, st, nil
}

// carriedTypes are the repomd.xml types (with any compression after a "_")
// that mergeInputs passes on from the first member that has them.
var carriedTypes = map[string]bool{"group": true, "updateinfo": true, "modules": true}

// scanMember decides over the packages of in's primary, member i of a merge,
// by what the members before it hold (seen): the first member wins a package
// (name, arch, epoch, version and release), and a file's path, because the
// group serves a path from the first member that has it. A member's own
// duplicates stay, as its own repomd.xml has them.
func (h *Handler) scanMember(ctx context.Context, dir string, in *mergeInput, i int32, seen map[mergeHash]int32, st *mergeStats) error {
	r, done, err := h.openDoc(ctx, in.files["primary"])
	if err != nil {
		return err
	}
	defer done()
	conflicts := 0
	err = scanPrimary(r, func(n int, p *primaryHead) error {
		if p.Name == "" || p.Arch == "" || p.Version.Ver == "" || p.Checksum == "" || p.Location.Href == "" {
			return fmt.Errorf("package %d (%q) lacks a name, arch, version, checksum or location", n, p.Name)
		}
		file, ok := groupPath(dir, p.Location.Href)
		if !ok {
			return fmt.Errorf("package %s is at %q, which the group cannot answer", p.Name, p.Location.Href)
		}
		pkg := mergeKey("package", p.Name, p.Arch, cmp.Or(p.Version.Epoch, "0"), p.Version.Ver, p.Version.Rel)
		place := mergeKey("file", file)
		first, have := seen[pkg]
		firstAt, atHave := seen[place]
		if have && first != i || atHave && firstAt != i {
			if !have || first == i {
				conflicts++
			}
			in.drop = append(in.drop, n)
			in.gone[mergeKey(strings.TrimSpace(p.Checksum))] = true
			st.duplicates++
			return nil
		}
		seen[pkg], seen[place] = i, i
		st.packages++
		if n >= maxMergePackages || st.packages > maxMergePackages {
			return fmt.Errorf("more than %d packages", maxMergePackages)
		}
		return nil
	})
	if conflicts > 0 {
		slog.Warn("yum group merge leaves out packages at paths an earlier member answers", "member", in.repo.Name, "dir", dir, "packages", conflicts)
	}
	return err
}

// repomdXML is what a merge reads of a repomd.xml.
type repomdXML struct {
	Data []repomdData `xml:"data"`
}

type repomdData struct {
	Type     string `xml:"type,attr"`
	Checksum struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	} `xml:"checksum"`
	Location struct {
		Href string `xml:"href,attr"`
		Base string `xml:"base,attr"`
	} `xml:"location"`
	Inner string `xml:",innerxml"`
}

func (h *Handler) readRepomd(a meta.Asset) (*repomdXML, error) {
	f, err := h.Store.Open(a)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var doc repomdXML
	if err := xml.NewDecoder(io.LimitReader(f, maxRepomd)).Decode(&doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// memberFile finds the file d names in member m, which must be what the
// member's repomd.xml says.
func (h *Handler) memberFile(ctx context.Context, m *repo.Repository, dir string, d repomdData) (meta.Asset, error) {
	p, ok := groupPath(dir, d.Location.Href)
	if !ok {
		return meta.Asset{}, fmt.Errorf("it is at %q, which the group cannot answer", d.Location.Href)
	}
	res, err := h.Resolver.LookupTarget(ctx, m, proxy.Target{Key: p, Remote: p, NotHTML: true}, proxy.Metadata)
	if err != nil {
		return meta.Asset{}, fmt.Errorf("%s: %w", p, err)
	}
	if d.Checksum.Type == "sha256" && !strings.EqualFold(strings.TrimSpace(d.Checksum.Value), res.Asset.BlobSHA256) {
		return meta.Asset{}, fmt.Errorf("%s is not what repomd.xml says", p)
	}
	return res.Asset, nil
}

// groupPath is the path in the repository of what href names from dir, as a
// client resolves it against its baseurl. ok is false when the group cannot
// answer that: an absolute URL or path, or one above the repository.
func groupPath(dir, href string) (string, bool) {
	if href == "" || strings.HasPrefix(href, "/") || strings.Contains(href, "://") {
		return "", false
	}
	p := path.Clean(dir + href)
	return p, p != "." && checkPath(p) == nil
}

// ctxReader stops reading when ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// openDoc opens a member's document, decompressed; done closes it.
func (h *Handler) openDoc(ctx context.Context, a meta.Asset) (r io.Reader, done func(), err error) {
	f, err := h.Store.Open(a)
	if err != nil {
		return nil, nil, err
	}
	if r, err = decompressed(ctxReader{ctx, f}); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("%s: %w", a.Path, err)
	}
	return r, func() { f.Close() }, nil
}

// decompressed reads r according to its first bytes. A window larger than the
// decoders' default of 8 MiB (xz -7 and above) is refused: it would be held
// for the whole document, and the error says so.
func decompressed(r io.Reader) (io.Reader, error) {
	br := bufio.NewReaderSize(r, mergeBuf)
	magic, err := br.Peek(6)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		return gzip.NewReader(br)
	case bytes.HasPrefix(magic, []byte("BZh")):
		return bzip2.NewReader(br), nil
	case bytes.HasPrefix(magic, []byte{0xfd, '7', 'z', 'X', 'Z', 0}):
		return xz.NewReader(br, xz.Options{})
	case bytes.HasPrefix(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return zstd.NewReader(br, zstd.Options{})
	}
	return br, nil
}

// writeDoc stores the merged document typ: every member's packages, those
// scanMember left out skipped, gzip compressed as they go.
func (h *Handler) writeDoc(ctx context.Context, rp *repo.Repository, typ, dir string, n int, inputs []*mergeInput) (mergedDoc, error) {
	pr, pw := io.Pipe()
	defer pw.CloseWithError(errors.New("merge stopped")) // has no effect after a Close below
	type stored struct {
		sums blob.Sums
		err  error
	}
	ch := make(chan stored, 1)
	go func() {
		sums, err := h.Store.WriteBlob(rp.Name, pr, nil)
		pr.CloseWithError(cmp.Or(err, io.ErrClosedPipe)) // stops the writer if the store gave up
		ch <- stored{sums, err}
	}()
	open := sha256.New()
	size := &countWriter{}
	gz := gzip.NewWriter(pw) // no name or mtime: the same XML gives the same bytes
	w := bufio.NewWriterSize(io.MultiWriter(gz, open, size), mergeBuf)
	err := h.fillDoc(ctx, w, typ, dir, n, inputs)
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = gz.Close()
	}
	pw.CloseWithError(err)
	res := <-ch
	if err == nil {
		err = res.err
	}
	if err != nil {
		return mergedDoc{}, err
	}
	return mergedDoc{repoData{typ: typ, sum: res.sums.SHA256, openSum: hex.EncodeToString(open.Sum(nil)), size: res.sums.Size, openSize: size.n}, res.sums}, nil
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func (h *Handler) fillDoc(ctx context.Context, w *bufio.Writer, typ, dir string, n int, inputs []*mergeInput) error {
	w.WriteString(docStart(typ, n))
	for _, in := range inputs {
		a, ok := in.files[typ]
		if !ok { // a member without filelists or other: as in its own repomd.xml
			continue
		}
		r, done, err := h.openDoc(ctx, a)
		if err != nil {
			return fmt.Errorf("member %q: %w", in.repo.Name, err)
		}
		based, err := copyPackages(r, w, typ, dir, in.skip(typ))
		done()
		if err != nil {
			return fmt.Errorf("member %q: %s: %w", in.repo.Name, a.Path, err)
		}
		if based > 0 {
			slog.Warn("yum group merge removes xml:base: these packages are looked for at the member's own path", "member", in.repo.Name, "dir", dir, "packages", based)
		}
	}
	w.WriteString("</" + docs[typ].root + ">\n")
	return nil
}

// skip decides, for copyPackages, over the packages of in's document typ:
// primary by their place, the others by their pkgid.
func (in *mergeInput) skip(typ string) func(n int, start []byte) (bool, error) {
	if typ == "primary" {
		next := 0
		return func(n int, _ []byte) (bool, error) {
			if next < len(in.drop) && in.drop[next] == n {
				next++
				return true, nil
			}
			return false, nil
		}
	}
	return func(_ int, start []byte) (bool, error) {
		if len(in.gone) == 0 {
			return false, nil
		}
		attrs, err := tagAttrs(start)
		if err != nil {
			return false, err
		}
		for _, a := range attrs {
			if a.Name.Local == "pkgid" {
				return in.gone[mergeKey(a.Value)], nil
			}
		}
		return false, errors.New("a package without pkgid")
	}
}

// primaryHead is what decides over a package of primary.xml, all of it before
// its <format> in the schema's order.
type primaryHead struct {
	Name    string `xml:"name"`
	Arch    string `xml:"arch"`
	Version struct {
		Epoch string `xml:"epoch,attr"`
		Ver   string `xml:"ver,attr"`
		Rel   string `xml:"rel,attr"`
	} `xml:"version"`
	Checksum string `xml:"checksum"`
	Location struct {
		Href string `xml:"href,attr"`
	} `xml:"location"`
}

// headBuffer holds a package up to its <format>.
type headBuffer struct{ bytes.Buffer }

func (b *headBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxHead {
		return 0, fmt.Errorf("a package with more than %d bytes before its <format>", maxHead)
	}
	return b.Buffer.Write(p)
}

// scanPrimary calls visit with the head of every package of a primary.xml, n
// counting them. The rest of a package is not kept.
func scanPrimary(r io.Reader, visit func(n int, p *primaryHead) error) error {
	s, err := newDocScanner(r, "primary")
	if err != nil {
		return err
	}
	head := &headBuffer{}
	for n := 0; ; n++ {
		start, ok, err := s.next()
		if !ok {
			return err
		}
		head.Reset()
		head.Buffer.Write(start)
		stop, err := s.copyTo(head, "format", "/package", "package")
		if err != nil {
			return err
		}
		if stop == "format" {
			if stop, err = s.copyTo(io.Discard, "/package", "package"); err != nil {
				return err
			}
		}
		if stop == "package" {
			return errors.New("a <package> inside a <package>")
		}
		if _, err := s.tag(); err != nil {
			return err
		}
		head.Buffer.WriteString("</package>")
		var p primaryHead
		if err := xml.Unmarshal(head.Bytes(), &p); err != nil {
			return fmt.Errorf("package %d: %w", n, err)
		}
		if err := visit(n, &p); err != nil {
			return err
		}
	}
}

// copyPackages copies the packages of a document of type typ to w, but those
// skip refuses, as the member wrote them. A <location> has its xml:base
// removed (based counts those), since the group, not that base, serves the
// file; one the group cannot answer is an error.
func copyPackages(r io.Reader, w io.Writer, typ, dir string, skip func(n int, start []byte) (bool, error)) (based int, err error) {
	s, err := newDocScanner(r, typ)
	if err != nil {
		return 0, err
	}
	for n := 0; ; n++ {
		start, ok, err := s.next()
		if !ok {
			return based, err
		}
		drop, err := skip(n, start)
		if err != nil {
			return based, err
		}
		out := w
		if drop {
			out = io.Discard
		}
		if _, err := out.Write(start); err != nil {
			return based, err
		}
		for stop := ""; stop != "/package"; {
			if stop, err = s.copyTo(out, "location", "/package", "package"); err != nil {
				return based, err
			}
			if stop == "package" {
				return based, errors.New("a <package> inside a <package>")
			}
			tag, err := s.tag()
			if err != nil {
				return based, err
			}
			if stop == "location" && !drop {
				t, stripped, err := groupLocation(dir, tag)
				if err != nil {
					return based, err
				}
				tag = t
				if stripped {
					based++
				}
			}
			if _, err := out.Write(tag); err != nil {
				return based, err
			}
		}
		if _, err := io.WriteString(out, "\n"); err != nil {
			return based, err
		}
	}
}

// groupLocation checks a <location> tag and returns it without xml:base.
func groupLocation(dir string, tag []byte) (out []byte, stripped bool, err error) {
	attrs, err := tagAttrs(tag)
	if err != nil {
		return nil, false, err
	}
	href := ""
	for _, a := range attrs {
		switch {
		case a.Name.Local == "href":
			href = a.Value
		case a.Name.Space == "xml" && a.Name.Local == "base":
			stripped = true
		}
	}
	if _, ok := groupPath(dir, href); !ok {
		return nil, false, fmt.Errorf("a package at %q, which the group cannot answer", href)
	}
	if !stripped {
		return tag, false, nil
	}
	var w xw
	w.WriteString("<location")
	w.attr("href", href)
	if bytes.HasSuffix(tag, []byte("/>")) {
		w.WriteString("/>")
	} else {
		w.WriteString(">")
	}
	return []byte(w.String()), true, nil
}

// tagAttrs parses the attributes of a start tag.
func tagAttrs(tag []byte) ([]xml.Attr, error) {
	t, err := xml.NewDecoder(bytes.NewReader(tag)).RawToken()
	if err != nil {
		return nil, err
	}
	se, ok := t.(xml.StartElement)
	if !ok {
		return nil, fmt.Errorf("not a start tag: %.100q", tag)
	}
	return se.Attr, nil
}

// docScanner walks an XML document by the "<" that start its tags, copying
// what is between them, without parsing or holding it. XML has no other "<"
// than in comments, CDATA and processing instructions, which it skips over
// whole. Tags end at the first ">" outside quotes.
type docScanner struct {
	br  *bufio.Reader
	buf []byte
	end string // the root's end tag, "" when its start tag closes it too
}

// newDocScanner reads a document of type typ up to and through its root's
// start tag. Its packages are copied as they are, so a namespace prefix the
// merged root does not declare (docs) cannot go along.
func newDocScanner(r io.Reader, typ string) (*docScanner, error) {
	s := &docScanner{br: bufio.NewReaderSize(r, mergeBuf)}
	if _, err := s.copyTo(io.Discard, docs[typ].root); err != nil {
		return nil, fmt.Errorf("no <%s> element: %w", docs[typ].root, err)
	}
	start, err := s.tag()
	if err != nil {
		return nil, err
	}
	attrs, err := tagAttrs(start)
	if err != nil {
		return nil, err
	}
	for _, a := range attrs {
		if a.Name.Space == "xmlns" && !strings.Contains(docs[typ].xmlns, "xmlns:"+a.Name.Local+"=") {
			return nil, fmt.Errorf("its root declares the namespace %q, which a merge cannot carry", a.Name.Local)
		}
	}
	if !bytes.HasSuffix(start, []byte("/>")) {
		s.end = "/" + docs[typ].root
	}
	return s, nil
}

// next reads up to and through the next package's start tag; ok is false at
// the end of the document, read to its end: a gzip stream checks its checksum
// there.
func (s *docScanner) next() (start []byte, ok bool, err error) {
	if s.end != "" {
		stop, err := s.copyTo(io.Discard, "package", s.end)
		if err != nil {
			return nil, false, err
		}
		if stop == "package" {
			start, err = s.tag()
			return start, err == nil, err
		}
	}
	_, err = io.Copy(io.Discard, s.br)
	return nil, false, err
}

// copyTo copies the document to w up to the next tag of one of the names stop
// ("/name" for an end tag) and says which. That tag's "<" is read and not
// copied; tag reads the rest. The end of the document is an error.
func (s *docScanner) copyTo(w io.Writer, stop ...string) (string, error) {
	for {
		chunk, err := s.br.ReadSlice('<')
		if errors.Is(err, bufio.ErrBufferFull) {
			if _, err := w.Write(chunk); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", unexpectedEnd(err)
		}
		if _, err := w.Write(chunk[:len(chunk)-1]); err != nil {
			return "", err
		}
		head, _ := s.br.Peek(16) // past any name in stop
		end := ""
		switch {
		case bytes.HasPrefix(head, []byte("!--")):
			end = "-->"
		case bytes.HasPrefix(head, []byte("![CDATA[")):
			end = "]]>"
		case bytes.HasPrefix(head, []byte("?")):
			end = "?>"
		default:
			for _, name := range stop {
				if isTagName(head, name) {
					return name, nil
				}
			}
		}
		if _, err := w.Write(lessThan); err != nil {
			return "", err
		}
		if end != "" {
			if err := s.through(w, end); err != nil {
				return "", err
			}
		}
	}
}

var lessThan = []byte{'<'}

// isTagName reports whether what follows a "<" is the tag name.
func isTagName(head []byte, name string) bool {
	if len(head) <= len(name) || string(head[:len(name)]) != name {
		return false
	}
	switch head[len(name)] {
	case ' ', '\t', '\r', '\n', '/', '>':
		return true
	}
	return false
}

// through copies the document to w through the next end.
func (s *docScanner) through(w io.Writer, end string) error {
	for {
		buf, err := s.br.Peek(s.br.Size())
		if i := bytes.Index(buf, []byte(end)); i >= 0 {
			if _, err := w.Write(buf[:i+len(end)]); err != nil {
				return err
			}
			_, err := s.br.Discard(i + len(end))
			return err
		}
		if err != nil {
			return unexpectedEnd(err)
		}
		n := len(buf) - len(end) + 1
		if _, err := w.Write(buf[:n]); err != nil {
			return err
		}
		if _, err := s.br.Discard(n); err != nil {
			return err
		}
	}
}

// tag reads the rest of a tag copyTo stopped at and returns it whole, valid
// until the next call.
func (s *docScanner) tag() ([]byte, error) {
	s.buf = append(s.buf[:0], '<')
	var quote byte
	for len(s.buf) < maxTag {
		b, err := s.br.ReadByte()
		if err != nil {
			return nil, unexpectedEnd(err)
		}
		s.buf = append(s.buf, b)
		switch {
		case quote != 0:
			if b == quote {
				quote = 0
			}
		case b == '"' || b == '\'':
			quote = b
		case b == '>':
			return s.buf, nil
		}
	}
	return nil, fmt.Errorf("a tag of more than %d bytes", maxTag)
}

func unexpectedEnd(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
