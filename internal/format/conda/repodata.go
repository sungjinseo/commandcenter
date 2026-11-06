package conda

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
)

// maxDoc bounds a member document a group merge reads (conda-forge's
// linux-64 repodata.json is ~200 MB).
const maxDoc = 1 << 30

// repodata is <subdir>/repodata.json (and current_repodata.json, the same
// here): the packages of one subdir by file name.
type repodata struct {
	Info struct {
		Subdir string `json:"subdir"`
	} `json:"info"`
	Packages map[string]json.RawMessage `json:"packages"`
	Conda    map[string]json.RawMessage `json:"packages.conda"`
	Removed  []string                   `json:"removed"`
	Version  int                        `json:"repodata_version"`
}

func newRepodata(subdir string) *repodata {
	rd := &repodata{Packages: map[string]json.RawMessage{}, Conda: map[string]json.RawMessage{}, Removed: []string{}, Version: 1}
	rd.Info.Subdir = subdir
	return rd
}

func (rd *repodata) add(r row) error {
	b, err := json.Marshal(r.Index)
	if err != nil {
		return err
	}
	if strings.HasSuffix(r.File, ".conda") {
		rd.Conda[r.File] = b
	} else {
		rd.Packages[r.File] = b
	}
	return nil
}

// channeldata is channeldata.json: each package name's latest version,
// with its about.json, and the subdirs it is in.
type channeldata struct {
	Version  int                   `json:"channeldata_version"`
	Packages map[string]channelPkg `json:"packages"`
	Subdirs  []string              `json:"subdirs"`
}

type channelPkg struct {
	Description string   `json:"description,omitempty"`
	DevURL      string   `json:"dev_url,omitempty"`
	DocURL      string   `json:"doc_url,omitempty"`
	Home        string   `json:"home,omitempty"`
	License     string   `json:"license,omitempty"`
	Subdirs     []string `json:"subdirs"`
	Summary     string   `json:"summary,omitempty"`
	Timestamp   any      `json:"timestamp,omitempty"`
	Version     string   `json:"version"`
	latest      row
}

func newChanneldata() *channeldata {
	return &channeldata{Version: 1, Packages: map[string]channelPkg{}, Subdirs: []string{}}
}

// add records r: its subdir, and its about if it is the latest version
// (then build number; of one build, the .tar.bz2 before the .conda) of its
// name.
func (cd *channeldata) add(r row) {
	name := r.str("name")
	p, ok := cd.Packages[name]
	if !ok || newer(r, p.latest) {
		about := func(key string) string {
			s, _ := r.About[key].(string)
			return s
		}
		p = channelPkg{Description: about("description"), DevURL: about("dev_url"), DocURL: about("doc_url"), Home: about("home"),
			License: about("license"), Summary: about("summary"), Timestamp: r.Index["timestamp"], Version: r.str("version"), Subdirs: p.Subdirs, latest: r}
	}
	p.Subdirs = addString(p.Subdirs, r.Subdir)
	cd.Packages[name] = p
	cd.Subdirs = addString(cd.Subdirs, r.Subdir)
}

func addString(list []string, s string) []string {
	i := sort.SearchStrings(list, s)
	if i < len(list) && list[i] == s {
		return list
	}
	return append(list[:i], append([]string{s}, list[i:]...)...)
}

func newer(a, b row) bool {
	if va, vb := a.str("version"), b.str("version"); va != vb {
		return versionLess(vb, va)
	}
	na, _ := a.Index["build_number"].(json.Number)
	nb, _ := b.Index["build_number"].(json.Number)
	if na != nb {
		return versionLess(string(nb), string(na))
	}
	return strings.HasSuffix(a.File, ".tar.bz2") && strings.HasSuffix(b.File, ".conda")
}

var tokenRE = regexp.MustCompile(`[0-9]+|[A-Za-z]+`)

// versionLess orders conda versions enough to pick a channel's latest:
// runs of digits and of letters are compared one by one, numbers as
// numbers, words lexically ("a" < "b" < "rc"), a word before a number or
// a missing part ("1.0rc1" < "1.0" < "1.0.1") except "post", which comes
// after. ponytail: not conda's VersionOrder (no epochs, no "dev"
// special-casing); enough for channeldata's "version".
func versionLess(a, b string) bool {
	ta, tb := tokenRE.FindAllString(a, -1), tokenRE.FindAllString(b, -1)
	for i := 0; i < len(ta) || i < len(tb); i++ {
		x, y := tokenAt(ta, i), tokenAt(tb, i)
		if x == y {
			continue
		}
		if cx, cy := class(x), class(y); cx != cy {
			return cx < cy
		}
		if class(x) == 2 { // numbers: by length, then digits
			x, y = strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
			if len(x) != len(y) {
				return len(x) < len(y)
			}
		}
		return x < y
	}
	return false
}

func tokenAt(t []string, i int) string {
	if i < len(t) {
		return t[i]
	}
	return ""
}

// class ranks a token: a word, a missing part, a number, "post".
func class(t string) int {
	switch {
	case t == "post":
		return 3
	case t != "" && t[0] >= '0' && t[0] <= '9':
		return 2
	case t == "":
		return 1
	}
	return 0
}

// rowsOf lists the rows of rp, in name then version (path) order, those
// of one subdir when subdir is not "".
func rowsOf(ctx context.Context, tx *meta.Tx, rp *repo.Repository, subdir string) ([]row, error) {
	names, err := tx.ListPackageNames(ctx, rp.Name)
	if err != nil {
		return nil, err
	}
	var out []row
	for _, name := range names {
		pkgs, err := tx.ListPackages(ctx, rp.Name, name)
		if err != nil {
			return nil, err
		}
		for _, p := range pkgs {
			var r row
			if err := decodeJSON([]byte(p.Attrs), &r); err != nil {
				return nil, fmt.Errorf("package %s %s of %q: %w", p.Name, p.Version, rp.Name, err)
			}
			if subdir == "" || r.Subdir == subdir {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// rebuild writes subdir's repodata.json and current_repodata.json and the
// channel's channeldata.json from rp's rows, in the transaction that
// changed them.
func (h *Handler) rebuild(ctx context.Context, tx *meta.Tx, rp *repo.Repository, subdir string) error {
	all, err := rowsOf(ctx, tx, rp, "")
	if err != nil {
		return err
	}
	rd, cd := newRepodata(subdir), newChanneldata()
	for _, r := range all {
		cd.add(r)
		if r.Subdir == subdir {
			if err := rd.add(r); err != nil {
				return err
			}
		}
	}
	docs := map[string]any{subdir + "/repodata.json": rd, subdir + "/current_repodata.json": rd, "channeldata.json": cd}
	for p, doc := range docs {
		b, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		sums, err := h.Store.WriteBlob(rp.Name, strings.NewReader(string(b)), nil)
		if err != nil {
			return err
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, p, ctJSON, sums)); err != nil {
			return err
		}
	}
	return nil
}

// Group merges. A group's <subdir>/repodata.json lists its members' packages
// of that subdir (firstRepodata, or mergeRepodata with mergeMembers);
// channeldata.json is the first member's entry per package name, its subdirs
// the union. Members are read a package at a time, never whole.

// mergeRepodata is the resolve.MergeFunc of <subdir>/repodata.json of a group
// with mergeMembers: every member's files, a file listed by several members
// being the first's.
// ponytail: the seen set holds every file name of the merged channel (a
// few hundred thousand for conda-forge); a bloom filter if that bites.
func mergeRepodata(subdir string) func(ctx context.Context, members []io.ReadSeeker, w io.Writer) error {
	return func(ctx context.Context, members []io.ReadSeeker, w io.Writer) error {
		seen := map[string]bool{}
		return writeMerged(w, subdir, members, func(_ int, file string, _ json.RawMessage) bool {
			if seen[file] {
				return false
			}
			seen[file] = true
			return true
		})
	}
}

// writeMerged writes the members' "packages" then their "packages.conda" as
// <subdir>/repodata.json, each member read once per section (the sections of
// one output object cannot interleave), the entries keep allows.
func writeMerged(w io.Writer, subdir string, members []io.ReadSeeker, keep func(member int, file string, raw json.RawMessage) bool) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	if _, err := fmt.Fprintf(bw, `{"info":{"subdir":%s},"packages":{`, jsonString(subdir)); err != nil {
		return err
	}
	for i, section := range []string{"packages", "packages.conda"} {
		if i > 0 {
			bw.WriteString(`},"packages.conda":{`)
		}
		first := true
		for n, m := range members {
			if _, err := m.Seek(0, io.SeekStart); err != nil {
				return err
			}
			err := topLevel(m, func(key string, dec *json.Decoder) error {
				if key != section {
					return skipValue(dec)
				}
				return entries(dec, func(file string, raw json.RawMessage) error {
					if !keep(n, file, raw) {
						return nil
					}
					return writeEntry(bw, &first, file, raw)
				})
			})
			if err != nil {
				return err
			}
		}
	}
	bw.WriteString(`},"removed":[],"repodata_version":1}`)
	return bw.Flush()
}

// firstPrefix marks a group's repodata of firstRepodata: flipping
// mergeMembers merges again.
const firstPrefix = "conda-first:"

// member is what a group reads of a leaf member: doc is its repodata for the
// path asked, names its channeldata.json; nil for what it does not have.
type member struct{ doc, names *meta.Asset }

// docTarget is the fetch of a channel's repodata.json, current_repodata.json or
// channeldata.json. They decide whose a package name is in a group
// (firstRepodata): what a remote refuses or answers with what is no such
// document fails closed. A repodata.json can be hundreds of MiB: the check goes
// no further than the first of these keys.
func docTarget(p, route string) proxy.Target {
	return proxy.Target{Key: p, Remote: p, Route: route, DeniedIsFailure: true,
		Check: proxy.JSONDocument("info", "packages", "packages.conda", "repodata_version", "channeldata_version", "subdirs")}
}

// firstRepodata is group rp's document p (subdir's repodata.json or
// current_repodata.json) without mergeMembers: a package name is listed from
// the first member that has it, with all the builds that member lists,
// whatever a later member has. So a newer public build never beats an
// internal package of the same name, which conda would otherwise install
// (dependency confusion). A name counts wherever the member has it, as a
// client reads its platform's subdir and noarch: so channeldata.json, which
// names a channel's packages of all subdirs, is read too, of every member but
// the last (nothing comes after it to hide). The result is stored in rp's
// namespace and reused until one of those documents changes. A member that
// fails is left out only while no later member answers: a later (public)
// member never stands in for an earlier (internal) one.
func (h *Handler) firstRepodata(ctx context.Context, rp *repo.Repository, p, subdir string) (meta.Asset, error) {
	leaves := resolve.Leaves(rp)
	members := make([]member, len(leaves))
	sum := sha256.New()
	var failed error
	answered := 0
	look := func(m *repo.Repository, file string) (*meta.Asset, error) {
		res, err := h.Resolver.LookupTarget(ctx, m, docTarget(file, p), proxy.Metadata)
		switch {
		case err == nil && failed != nil:
			return nil, failed
		case err == nil:
			answered++
			fmt.Fprintln(sum, file, res.Asset.BlobSHA256)
			return &res.Asset, nil
		case repo.StatusOf(err) != http.StatusNotFound:
			slog.Warn("group member document failed", "group", rp.Name, "member", m.Name, "path", file, "err", logsafe.Err(err))
			failed = cmp.Or(failed, err)
		}
		return nil, nil
	}
	var only *meta.Asset
	for i, m := range leaves {
		var err error
		if members[i].doc, err = look(m, p); err != nil {
			return meta.Asset{}, err
		}
		only = cmp.Or(only, members[i].doc)
		if i < len(leaves)-1 {
			if members[i].names, err = look(m, "channeldata.json"); err != nil {
				return meta.Asset{}, err
			}
		}
	}
	switch {
	case only == nil && failed != nil:
		return meta.Asset{}, failed
	case only == nil:
		return meta.Asset{}, repo.ErrNotFound
	}
	fp := firstPrefix + hex.EncodeToString(sum.Sum(nil))
	stored, err := h.Store.Get(ctx, rp.Name, p)
	if err == nil && stored.RemoteETag == fp {
		return stored, nil
	}
	if err != nil && answered == 1 {
		// One document and no name to hide, and no merged copy was ever
		// served: the member's document as it is, as resolve.Merged does.
		return *only, nil
	}
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(h.writeFirst(pw, subdir, members))
	}()
	a, err := h.Store.Put(ctx, rp.Name, p, ctJSON, pr, func(a *meta.Asset) { a.RemoteETag = fp })
	pr.Close() // unblocks the merger if Put stopped early
	<-done
	return a, err
}

// writeFirst writes firstRepodata's document. A name's owner is the index in
// docs of the first member that has it, -1 when that member has no document
// for this subdir and so lists nothing here. A name no earlier member claimed
// belongs to whoever lists it.
func (h *Handler) writeFirst(w io.Writer, subdir string, members []member) error {
	var docs []io.ReadSeeker
	owner := map[string]int{}
	for i, m := range members {
		mine := -1
		if m.doc != nil {
			f, err := h.Store.Open(*m.doc)
			if err != nil {
				return err
			}
			defer f.Close()
			mine = len(docs)
			docs = append(docs, f)
		}
		if i == len(members)-1 {
			break
		}
		claim := func(name string) {
			if _, ok := owner[name]; !ok {
				owner[name] = mine
			}
		}
		if m.names != nil {
			f, err := h.Store.Open(*m.names)
			if err != nil {
				return err
			}
			err = topLevel(f, func(key string, dec *json.Decoder) error {
				if key != "packages" {
					return skipValue(dec)
				}
				return entries(dec, func(name string, _ json.RawMessage) error {
					claim(strings.ToLower(name))
					return nil
				})
			})
			f.Close()
			if err != nil {
				return err
			}
		}
		if m.doc != nil {
			err := topLevel(docs[mine], func(key string, dec *json.Decoder) error {
				if key != "packages" && key != "packages.conda" {
					return skipValue(dec)
				}
				return entries(dec, func(_ string, raw json.RawMessage) error {
					claim(entryName(raw))
					return nil
				})
			})
			if err != nil {
				return err
			}
		}
	}
	return writeMerged(w, subdir, docs, func(n int, _ string, raw json.RawMessage) bool {
		if len(owner) == 0 {
			return true
		}
		o, ok := owner[entryName(raw)]
		return !ok || o == n
	})
}

// entryName is the package name of a repodata entry, in lower case as conda
// compares names.
func entryName(raw json.RawMessage) string {
	var e struct {
		Name string `json:"name"`
	}
	json.Unmarshal(raw, &e) // not an object: a name nobody claims
	return strings.ToLower(e.Name)
}

// mergeChanneldata merges the members' channeldata.json.
func mergeChanneldata(ctx context.Context, members []io.ReadSeeker, w io.Writer) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	seen := map[string]bool{}
	var subdirs []string
	bw.WriteString(`{"channeldata_version":1,"packages":{`)
	first := true
	for _, m := range members {
		err := topLevel(m, func(key string, dec *json.Decoder) error {
			switch key {
			case "packages":
				return entries(dec, func(name string, raw json.RawMessage) error {
					if seen[name] {
						return nil
					}
					seen[name] = true
					return writeEntry(bw, &first, name, raw)
				})
			case "subdirs":
				var list []string
				if err := dec.Decode(&list); err != nil {
					return err
				}
				for _, s := range list {
					subdirs = addString(subdirs, s)
				}
				return nil
			}
			return skipValue(dec)
		})
		if err != nil {
			return err
		}
	}
	if subdirs == nil {
		subdirs = []string{}
	}
	b, _ := json.Marshal(subdirs)
	fmt.Fprintf(bw, `},"subdirs":%s}`, b)
	return bw.Flush()
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func writeEntry(w *bufio.Writer, first *bool, key string, raw json.RawMessage) error {
	if !*first {
		w.WriteByte(',')
	}
	*first = false
	w.WriteString(jsonString(key))
	w.WriteByte(':')
	_, err := w.Write(raw)
	return err
}

// topLevel reads the JSON object r holds (at most maxDoc), calling fn with
// each key and the decoder at its value, which fn must consume (Decode,
// entries or skipValue).
func topLevel(r io.Reader, fn func(key string, dec *json.Decoder) error) error {
	dec := json.NewDecoder(io.LimitReader(r, maxDoc))
	if err := expect(dec, '{'); err != nil {
		return err
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("json: key %v is not a string", tok)
		}
		if err := fn(key, dec); err != nil {
			return err
		}
	}
	return expect(dec, '}')
}

// entries consumes the object at dec, calling fn with each key and value.
func entries(dec *json.Decoder, fn func(key string, raw json.RawMessage) error) error {
	if err := expect(dec, '{'); err != nil {
		return err
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("json: key %v is not a string", tok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		if err := fn(key, raw); err != nil {
			return err
		}
	}
	return expect(dec, '}')
}

// skipValue consumes one value at dec, token by token, so a section the
// merge does not need is never held in memory.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

func expect(dec *json.Decoder, delim json.Delim) error {
	tok, err := dec.Token()
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != delim {
		return fmt.Errorf("json: %v where %c was expected", tok, delim)
	}
	return nil
}
