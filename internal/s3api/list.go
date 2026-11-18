package s3api

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/repo"
)

const s3NS = "http://s3.amazonaws.com/doc/2006-03-01/"

type owner struct {
	ID          string
	DisplayName string
}

// listBuckets lists the hosted raw repositories p may browse. Like AWS, it
// needs a signed request.
func (h *Handler) listBuckets(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if p.Anonymous() {
		return denied(p)
	}
	created, err := h.Store.DB.RepositoriesCreated(r.Context(), "raw", "hosted")
	if err != nil {
		return err
	}
	type bucket struct {
		Name         string
		CreationDate string
		BucketRegion string
	}
	var out struct {
		XMLName struct{} `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListAllMyBucketsResult"`
		Owner   owner
		Buckets struct {
			Bucket []bucket
		}
	}
	out.Owner = owner{p.Owner(), p.Owner()}
	for _, rp := range h.Registry.Snapshot().Sorted {
		if rp.Format == "raw" && rp.Type == "hosted" && h.Auth.Can(p, rp.Name, auth.Browse) {
			out.Buckets.Bucket = append(out.Buckets.Bucket, bucket{rp.Name, isoTime(created[rp.Name]), h.Region})
		}
	}
	writeXML(w, out)
	return nil
}

type object struct {
	Key          string
	LastModified string
	ETag         string
	Size         int64
	StorageClass string
}

type commonPrefix struct{ Prefix string }

// listObjects serves ListObjectsV2 (list-type=2) and ListObjects (V1).
// Keys come in byte order (the path column's "C" collation, S3's UTF-8
// binary order); a delimiter rolls the keys that have it after the prefix
// into common prefixes. Where p's privilege comes from a content selector,
// only the keys it reaches are listed.
func (h *Handler) listObjects(w http.ResponseWriter, r *http.Request, p auth.Principal, rp *repo.Repository) error {
	q := r.URL.Query()
	v2 := q.Get("list-type") == "2"
	prefix, delim := q.Get("prefix"), q.Get("delimiter")
	maxKeys := 1000
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return errf(http.StatusBadRequest, "InvalidArgument", "max-keys must be a number from 0")
		}
		maxKeys = min(n, 1000)
	}
	encode := func(s string) string { return s }
	switch q.Get("encoding-type") {
	case "":
	case "url":
		encode = func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "%2F", "/") }
	default:
		return errf(http.StatusBadRequest, "InvalidArgument", "encoding-type must be url")
	}
	// after is where the listing resumes: past the key or common prefix
	// that ended the previous page (a continuation token, a marker,
	// start-after), whichever is furthest.
	after := q.Get("start-after")
	token := q.Get("continuation-token")
	if v2 && token != "" {
		b, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			return errf(http.StatusBadRequest, "InvalidArgument", "the continuation token is invalid")
		}
		after = max(after, string(b))
	}
	if !v2 {
		after = q.Get("marker")
	}
	everyKey := h.Auth.CanAll(p, rp.Name, auth.Browse)
	var contents []object
	var prefixes []commonPrefix
	truncated, last := false, ""
	bound, inclusive := prefix, true
	if after >= prefix {
		bound, inclusive = skip(after, prefix, delim)
	}
scan:
	for maxKeys > 0 {
		page, err := h.Store.DB.AssetsFrom(r.Context(), rp.Name, bound, inclusive, 1000)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		bound, inclusive = page[len(page)-1].Path, false
		for _, a := range page {
			if !strings.HasPrefix(a.Path, prefix) {
				break scan
			}
			name := a.Path
			cp := false
			if i := strings.Index(a.Path[len(prefix):], delim); delim != "" && i >= 0 {
				name, cp = a.Path[:len(prefix)+i+len(delim)], true
			}
			if len(contents)+len(prefixes) == maxKeys {
				truncated = true
				break scan
			}
			if cp {
				prefixes = append(prefixes, commonPrefix{encode(name)})
				last = name
				bound, inclusive = skip(name, prefix, delim)
				continue scan // the rest under the common prefix is skipped
			}
			if everyKey || h.Auth.CanPath(p, rp.Name, auth.Browse, a.Path) {
				contents = append(contents, object{encode(a.Path), isoTime(a.LastModified), etagOf(a), a.Size, "STANDARD"})
				last = a.Path
			}
		}
	}
	if v2 {
		next := ""
		if truncated {
			next = base64.RawURLEncoding.EncodeToString([]byte(last))
		}
		writeXML(w, struct {
			XMLName               struct{} `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
			Name                  string
			Prefix                string
			Delimiter             string `xml:",omitempty"`
			MaxKeys               int
			KeyCount              int
			IsTruncated           bool
			EncodingType          string `xml:",omitempty"`
			ContinuationToken     string `xml:",omitempty"`
			NextContinuationToken string `xml:",omitempty"`
			StartAfter            string `xml:",omitempty"`
			Contents              []object
			CommonPrefixes        []commonPrefix
		}{Name: rp.Name, Prefix: encode(prefix), Delimiter: encode(delim), MaxKeys: maxKeys, KeyCount: len(contents) + len(prefixes),
			IsTruncated: truncated, EncodingType: q.Get("encoding-type"), ContinuationToken: token, NextContinuationToken: next,
			StartAfter: encode(q.Get("start-after")), Contents: contents, CommonPrefixes: prefixes})
		return nil
	}
	next := ""
	if truncated {
		next = encode(last)
	}
	writeXML(w, struct {
		XMLName        struct{} `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
		Name           string
		Prefix         string
		Marker         string
		NextMarker     string `xml:",omitempty"`
		Delimiter      string `xml:",omitempty"`
		MaxKeys        int
		IsTruncated    bool
		EncodingType   string `xml:",omitempty"`
		Contents       []object
		CommonPrefixes []commonPrefix
	}{Name: rp.Name, Prefix: encode(prefix), Marker: encode(q.Get("marker")), NextMarker: next, Delimiter: encode(delim), MaxKeys: maxKeys,
		IsTruncated: truncated, EncodingType: q.Get("encoding-type"), Contents: contents, CommonPrefixes: prefixes})
	return nil
}

// skip is the bound a listing resumes from after the key or common prefix
// name: past name, and past every key under it when it is a common prefix.
// ponytail: a key under the prefix that continues with U+10FFFF and more
// would be listed again; such keys do not occur.
func skip(name, prefix, delim string) (bound string, inclusive bool) {
	if delim != "" && strings.HasPrefix(name, prefix) && strings.HasSuffix(name, delim) && strings.Contains(name[len(prefix):], delim) {
		return name + "\U0010FFFF", false
	}
	return name, false
}
