package conda

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
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

// Group merges. A group's <subdir>/repodata.json lists every member's
// packages of that subdir, a file name listed by several members being
// the first member's (the member that answers for the file, as Lookup
// does); channeldata.json likewise by package name, its subdirs the
// union. Members are read a package at a time, never whole.

// mergeRepodata is the resolve.MergeFunc of <subdir>/repodata.json: the
// members' "packages" then their "packages.conda", each member read once
// per section (the sections of one output object cannot interleave).
// ponytail: the seen set holds every file name of the merged channel (a
// few hundred thousand for conda-forge); a bloom filter if that bites.
func mergeRepodata(subdir string) func(ctx context.Context, members []io.ReadSeeker, w io.Writer) error {
	return func(ctx context.Context, members []io.ReadSeeker, w io.Writer) error {
		bw := bufio.NewWriterSize(w, 64<<10)
		seen := map[string]bool{}
		if _, err := fmt.Fprintf(bw, `{"info":{"subdir":%s},"packages":{`, jsonString(subdir)); err != nil {
			return err
		}
		for i, section := range []string{"packages", "packages.conda"} {
			if i > 0 {
				bw.WriteString(`},"packages.conda":{`)
			}
			first := true
			for _, m := range members {
				if _, err := m.Seek(0, io.SeekStart); err != nil {
					return err
				}
				err := topLevel(m, func(key string, dec *json.Decoder) error {
					if key != section {
						return skipValue(dec)
					}
					return entries(dec, func(file string, raw json.RawMessage) error {
						if seen[file] {
							return nil
						}
						seen[file] = true
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
