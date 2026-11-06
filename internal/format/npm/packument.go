package npm

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

type tagPair struct {
	tag, version string
}

func isTarballPath(path []string) bool {
	return len(path) == 4 && path[0] == "versions" && path[2] == "dist" && path[3] == "tarball"
}

// fileFromURL returns the last path segment of a tarball URL.
func fileFromURL(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	if i := strings.LastIndexByte(u, '/'); i >= 0 {
		u = u[i+1:]
	}
	return u
}

// tarballName is the file name the tarball of name@version goes by here,
// given dist.tarball: its last segment, unless that names no tarball (GitHub
// Packages ends the URL in a hash), which gets the usual name, so that the
// file is a component like any other.
func tarballName(name, version, upstream string) string {
	if f := fileFromURL(upstream); strings.HasSuffix(f, ".tgz") {
		return f
	}
	return TarballFile(name, version)
}

// rewritePackument copies a packument, pointing every versions.*.dist.tarball
// at tarballURL(version, the URL it had).
func rewritePackument(src io.Reader, dst io.Writer, tarballURL func(version, upstream string) string) error {
	dec := json.NewDecoder(src)
	dec.UseNumber()
	tw := newTokenWriter(dst)
	err := copyValue(dec, tw, nil, func(path []string, s string) string {
		if isTarballPath(path) {
			return tarballURL(path[1], s)
		}
		return s
	})
	if err != nil {
		return err
	}
	return tw.Flush()
}

// readDistTags returns the dist-tags of a packument in document order.
func readDistTags(r io.ReadSeeker) ([]tagPair, error) {
	var out []tagPair
	err := eachTopLevel(r, func(key string, dec *json.Decoder) error {
		if key != "dist-tags" {
			return skipValue(dec)
		}
		return eachEntryIfObject(dec, func(tag string) error {
			var v any
			if err := dec.Decode(&v); err != nil {
				return err
			}
			if s, ok := v.(string); ok {
				out = append(out, tagPair{tag, s})
			}
			return nil
		})
	})
	return out, err
}

// versionDist is what a package document says of one version's tarball.
type versionDist struct {
	Tarball   string `json:"tarball"`
	Shasum    string `json:"shasum"`
	Integrity string `json:"integrity"`
}

// scanDoc reads the package document r and calls fn with each version it
// lists and that version's dist. Anything but a JSON object with a name is
// an error: a sign-in or error page a remote answered with 200 is told from
// a package document by it.
func scanDoc(r io.ReadSeeker, fn func(version string, d versionDist)) error {
	name := ""
	err := eachTopLevel(r, func(key string, dec *json.Decoder) error {
		switch key {
		case "name":
			var v any
			err := dec.Decode(&v)
			name, _ = v.(string)
			return err
		case "versions":
			return eachEntryIfObject(dec, func(version string) error {
				var d versionDist
				err := eachEntryIfObject(dec, func(k string) error {
					if k != "dist" {
						return skipValue(dec)
					}
					var raw json.RawMessage
					err := dec.Decode(&raw)
					json.Unmarshal(raw, &d) // fields of another type stay empty
					return err
				})
				fn(version, d)
				return err
			})
		}
		return skipValue(dec)
	})
	if err == nil && name == "" {
		err = errors.New("npm: not a package document: no name")
	}
	return err
}

// mergeDocs writes one package document from docs, the first dominant: a
// top-level field, a version and a time entry come from the first document
// that has them, and dist-tags is tags. take, if set, limits the versions
// and time entries the documents after the first add. tarballURL, if set,
// points every version's dist.tarball at tarballURL(version, the URL it had).
// Left out are _rev (the result is no document's revision) and
// time.unpublished, which npm view reads as "this package is gone".
func mergeDocs(dst io.Writer, docs []io.ReadSeeker, tags []tagPair, take func(key string) bool, tarballURL func(version, upstream string) string) error {
	tw := newTokenWriter(dst)
	tw.Begin(true)
	seen := map[string]bool{"versions": true, "time": true, "dist-tags": true, "_rev": true, "_attachments": true}
	for _, d := range docs {
		err := eachTopLevel(d, func(key string, dec *json.Decoder) error {
			if seen[key] {
				return skipValue(dec)
			}
			seen[key] = true
			tw.Key(key)
			return copyValue(dec, tw, nil, nil)
		})
		if err != nil {
			return err
		}
	}
	tw.Key("dist-tags")
	tw.Begin(true)
	for _, t := range tags {
		tw.Key(t.tag)
		tw.Value(t.version)
	}
	tw.End()
	for _, section := range []string{"versions", "time"} {
		tw.Key(section)
		tw.Begin(true)
		seen := map[string]bool{"unpublished": section == "time"}
		for i, d := range docs {
			err := eachTopLevel(d, func(key string, dec *json.Decoder) error {
				if key != section {
					return skipValue(dec)
				}
				return eachEntryIfObject(dec, func(k string) error {
					if seen[k] || i > 0 && take != nil && !take(k) {
						return skipValue(dec)
					}
					seen[k] = true
					tw.Key(k)
					var rewrite func(path []string, s string) string
					if section == "versions" && tarballURL != nil {
						rewrite = func(path []string, s string) string {
							if len(path) == 2 && path[0] == "dist" && path[1] == "tarball" {
								return tarballURL(k, s)
							}
							return s
						}
					}
					return copyValue(dec, tw, nil, rewrite)
				})
			})
			if err != nil {
				return err
			}
		}
		tw.End()
	}
	tw.End()
	return tw.Flush()
}

// mergedTags returns the dist-tags of docs taken together: a tag comes from
// the first document that has it, except latest, which is the highest of
// theirs (Nexus's group merge). One document gives its own.
func mergedTags(docs []io.ReadSeeker) ([]tagPair, error) {
	var out []tagPair
	at := map[string]int{}
	for _, d := range docs {
		tags, err := readDistTags(d)
		if err != nil {
			return nil, err
		}
		for _, t := range tags {
			switch i, ok := at[t.tag]; {
			case !ok:
				at[t.tag] = len(out)
				out = append(out, t)
			case t.tag == "latest":
				a, okA := semver.Parse(out[i].version)
				b, okB := semver.Parse(t.version)
				if okB && (!okA || semver.Compare(b, a) > 0) {
					out[i].version = t.version
				}
			}
		}
	}
	return out, nil
}

func writeDistTags(w io.Writer, tags []tagPair) error {
	tw := newTokenWriter(w)
	tw.Begin(true)
	for _, t := range tags {
		tw.Key(t.tag)
		tw.Value(t.version)
	}
	tw.End()
	return tw.Flush()
}

// writeVersion is writeVersionDoc for the documents of a read (handler.read):
// several are those a group merges, where the tag is the merged one
// (mergedTags) and the manifest that of the first document with the version.
func writeVersion(docs []io.ReadSeeker, versionOrTag string, tarballURL func(version, upstream string) string, w io.Writer) error {
	if len(docs) == 1 {
		return writeVersionDoc(docs[0], versionOrTag, tarballURL, w)
	}
	tags, err := mergedTags(docs)
	if err != nil {
		return err
	}
	want := versionOrTag
	for _, t := range tags {
		if t.tag == versionOrTag {
			want = t.version
			break
		}
	}
	for _, d := range docs {
		if err := writeVersionDoc(d, want, tarballURL, w); err == nil || repo.StatusOf(err) != http.StatusNotFound {
			return err
		}
	}
	return repo.Errorf(http.StatusNotFound, "version %q not found", versionOrTag)
}

// writeVersionDoc writes the manifest of one version, or of the version a
// dist-tag points to. Nothing is written when it does not exist.
func writeVersionDoc(r io.ReadSeeker, versionOrTag string, tarballURL func(version, upstream string) string, w io.Writer) error {
	tags, err := readDistTags(r)
	if err != nil {
		return err
	}
	want := versionOrTag
	for _, t := range tags {
		if t.tag == versionOrTag {
			want = t.version
			break
		}
	}
	found := false
	err = eachTopLevel(r, func(key string, dec *json.Decoder) error {
		if key != "versions" {
			return skipValue(dec)
		}
		return eachEntryIfObject(dec, func(v string) error {
			if v != want || found {
				return skipValue(dec)
			}
			found = true
			tw := newTokenWriter(w)
			err := copyValue(dec, tw, nil, func(path []string, s string) string {
				if len(path) == 2 && path[0] == "dist" && path[1] == "tarball" {
					return tarballURL(v, s)
				}
				return s
			})
			if err != nil {
				return err
			}
			return tw.Flush()
		})
	})
	if err != nil {
		return err
	}
	if !found {
		return repo.Errorf(http.StatusNotFound, "version %q not found", versionOrTag)
	}
	return nil
}
