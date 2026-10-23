package npm

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"commandcenter/internal/repo"
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

// rewritePackument copies a packument, pointing every versions.*.dist.tarball
// at tarballURL(file).
func rewritePackument(src io.Reader, dst io.Writer, tarballURL func(file string) string) error {
	dec := json.NewDecoder(src)
	dec.UseNumber()
	tw := newTokenWriter(dst)
	err := copyValue(dec, tw, nil, func(path []string, s string) string {
		if isTarballPath(path) {
			return tarballURL(fileFromURL(s))
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

// mergePackuments merges documents for one package. Top-level fields, versions
// and time entries come from the first member that has them; dist-tags too,
// including "latest": member order decides, so a hosted member's latest is not
// overridden by a newer release from a proxy (QA M-4, plan 13 Decision 3).
// Members that are not JSON objects are skipped.
func mergePackuments(members []io.ReadSeeker, dst io.Writer) error {
	var valid []io.ReadSeeker
	latest := ""
	tags := map[string]string{}
	var tagOrder []string
	for _, m := range members {
		pairs, err := readDistTags(m)
		if err != nil {
			slog.Warn("skipping unreadable npm package document in group merge", "err", err)
			continue
		}
		valid = append(valid, m)
		for _, p := range pairs {
			if p.tag == "latest" {
				if latest == "" {
					latest = p.version
				}
				continue
			}
			if _, ok := tags[p.tag]; !ok {
				tags[p.tag] = p.version
				tagOrder = append(tagOrder, p.tag)
			}
		}
	}
	if len(valid) == 0 {
		return errors.New("npm: no readable package document to merge")
	}

	tw := newTokenWriter(dst)
	tw.Begin(true)
	seenTop := map[string]bool{}
	for _, m := range valid {
		err := eachTopLevel(m, func(key string, dec *json.Decoder) error {
			switch key {
			case "versions", "time", "dist-tags", "_rev", "_attachments":
				return skipValue(dec)
			}
			if seenTop[key] {
				return skipValue(dec)
			}
			seenTop[key] = true
			tw.Key(key)
			return copyValue(dec, tw, nil, nil)
		})
		if err != nil {
			return err
		}
	}

	tw.Key("dist-tags")
	tw.Begin(true)
	if latest != "" {
		tw.Key("latest")
		tw.Value(latest)
	}
	for _, t := range tagOrder {
		tw.Key(t)
		tw.Value(tags[t])
	}
	tw.End()

	for _, section := range []string{"versions", "time"} {
		tw.Key(section)
		tw.Begin(true)
		seen := map[string]bool{}
		for _, m := range valid {
			err := eachTopLevel(m, func(key string, dec *json.Decoder) error {
				if key != section {
					return skipValue(dec)
				}
				return eachEntryIfObject(dec, func(k string) error {
					if seen[k] {
						return skipValue(dec)
					}
					seen[k] = true
					tw.Key(k)
					return copyValue(dec, tw, nil, nil)
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

// writeVersionDoc writes the manifest of one version, or of the version a
// dist-tag points to. Nothing is written when it does not exist.
func writeVersionDoc(r io.ReadSeeker, versionOrTag string, tarballURL func(file string) string, w io.Writer) error {
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
					return tarballURL(fileFromURL(s))
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
