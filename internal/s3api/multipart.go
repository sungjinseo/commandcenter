package s3api

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/repo"
)

// Multipart uploads live where Docker's upload sessions do: in the tmp
// directory of the repository's blob store (blob.Store.SessionPath), which
// every node shares, so the parts of one upload may reach any node. The
// upload is a JSON sidecar, ses-s3-<id>.json, and each part a file,
// ses-s3-<id>-<number>-<md5>, the part's ETag in its name; a part sent
// again replaces it. Nothing is in the database until Complete stores the
// object. The tmp sweep (at a store's open, compact, and the
// blobstore.delete-temp-files task) removes session files not written for
// blob.SessionMaxAge, so an abandoned upload goes 24 hours after its last
// part (each part touches the sidecar); an upload must be completed within
// 24 hours of its first part's write.

const (
	maxParts    = 10000
	minPartSize = 5 << 20 // every part but the last (AWS)
)

var uploadIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// upload is the sidecar of a multipart upload.
type upload struct {
	Repo        string    `json:"repo"`
	Key         string    `json:"key"`
	ContentType string    `json:"contentType"`
	Attrs       attrs     `json:"attrs"`
	Owner       string    `json:"owner"`
	Started     time.Time `json:"started"`

	id  string
	bs  *blob.Store
	dir string // the store's tmp directory
}

func (u *upload) name(suffix string) string { return "s3-" + u.id + suffix }
func (u *upload) sidecar() string           { return u.bs.SessionPath(u.name(".json")) }

// part is one stored part.
type part struct {
	n    int
	md5  string
	size int64
	mod  time.Time
	path string
}

// parts reads the upload's parts from its directory, by number.
func (u *upload) parts() (map[int]part, error) {
	entries, err := os.ReadDir(u.dir)
	if err != nil {
		return nil, err
	}
	prefix := filepath.Base(u.bs.SessionPath(u.name("-")))
	out := map[int]part{}
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), prefix)
		num, sum, ok2 := strings.Cut(rest, "-")
		n, err := strconv.Atoi(num)
		if !ok || !ok2 || err != nil || len(sum) != 32 {
			continue // a part being written, or another upload's
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out[n] = part{n: n, md5: sum, size: info.Size(), mod: info.ModTime(), path: filepath.Join(u.dir, e.Name())}
	}
	return out, nil
}

// remove deletes the upload's files.
func (u *upload) remove() {
	if ps, err := u.parts(); err == nil {
		for _, p := range ps {
			os.Remove(p.path)
		}
	}
	os.Remove(u.sidecar())
}

// openUpload loads the upload id of rp/key; NoSuchUpload when it is not
// there, is another key's, or expired.
func (h *Handler) openUpload(rp *repo.Repository, key, id string) (*upload, error) {
	if !uploadIDRE.MatchString(id) {
		return nil, errNoSuchUpload
	}
	bs, err := h.Store.BlobStore(rp.Name)
	if err != nil {
		return nil, err
	}
	u := &upload{id: id, bs: bs}
	u.dir = filepath.Dir(bs.SessionPath(u.name("")))
	b, err := os.ReadFile(u.sidecar())
	if err != nil {
		return nil, errNoSuchUpload
	}
	if json.Unmarshal(b, u) != nil || u.Repo != rp.Name || u.Key != key {
		return nil, errNoSuchUpload
	}
	if info, err := os.Stat(u.sidecar()); err != nil || h.clock().Sub(info.ModTime()) > blob.SessionMaxAge {
		u.remove()
		return nil, errNoSuchUpload
	}
	return u, nil
}

func (h *Handler) createUpload(w http.ResponseWriter, r *http.Request, p auth.Principal, bucket, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	rp, err := h.bucket(r, p, bucket, auth.Add, key)
	if err != nil {
		return err
	}
	if err := h.checkWrite(r, p, rp, key); err != nil {
		return err
	}
	ct, at, err := objectHeaders(r, key)
	if err != nil {
		return err
	}
	bs, err := h.Store.BlobStore(rp.Name)
	if err != nil {
		return err
	}
	b := make([]byte, 16)
	rand.Read(b)
	u := &upload{Repo: rp.Name, Key: key, ContentType: ct, Attrs: at, Owner: p.Owner(), Started: h.clock().UTC(), id: hex.EncodeToString(b), bs: bs}
	js, _ := json.Marshal(u)
	if err := writeNew(u.sidecar(), js); err != nil {
		return err
	}
	if v := r.Header.Get("X-Amz-Checksum-Algorithm"); v != "" {
		w.Header().Set("X-Amz-Checksum-Algorithm", v)
	}
	writeXML(w, struct {
		XMLName  struct{} `xml:"http://s3.amazonaws.com/doc/2006-03-01/ InitiateMultipartUploadResult"`
		Bucket   string
		Key      string
		UploadID string `xml:"UploadId"`
	}{Bucket: rp.Name, Key: key, UploadID: u.id})
	return nil
}

// writeNew creates path with data, synced.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

// uploadPart serves UploadPart and UploadPartCopy: the part streams to a
// file of its own, then replaces the part of its number.
func (h *Handler) uploadPart(w http.ResponseWriter, r *http.Request, p auth.Principal, sig *signer, bucket, key, id string) error {
	n, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || n < 1 || n > maxParts {
		return errf(http.StatusBadRequest, "InvalidArgument", "partNumber must be between 1 and %d", maxParts)
	}
	rp, err := h.bucket(r, p, bucket, auth.Add, key)
	if err != nil {
		return err
	}
	u, err := h.openUpload(rp, key, id)
	if err != nil {
		return err
	}
	var content io.Reader
	var b *body
	copied := r.Header.Get("X-Amz-Copy-Source") != ""
	if copied {
		src, err := h.copySource(r, p)
		if err != nil {
			return err
		}
		f, err := h.Store.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		start, end, err := copyRange(r.Header.Get("X-Amz-Copy-Source-Range"), src.Size)
		if err != nil {
			return err
		}
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return err
		}
		content = io.LimitReader(f, end-start)
	} else {
		if b, err = newBody(r, sig, h.maxUpload()); err != nil {
			return err
		}
		content = b
	}
	rnd := make([]byte, 8)
	rand.Read(rnd)
	tmp := u.bs.SessionPath(u.name(fmt.Sprintf("-%05d.%x", n, rnd)))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	sum := md5.New()
	_, err = io.Copy(io.MultiWriter(f, sum), content)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	etag := hex.EncodeToString(sum.Sum(nil))
	unlock, err := h.Store.Lock(r.Context(), "s3\x00part\x00"+id+"\x00"+strconv.Itoa(n))
	if err != nil {
		os.Remove(tmp)
		return err
	}
	defer unlock()
	if old, err := u.parts(); err == nil {
		if o, ok := old[n]; ok {
			os.Remove(o.path)
		}
	}
	if err := os.Rename(tmp, u.bs.SessionPath(u.name(fmt.Sprintf("-%05d-%s", n, etag)))); err != nil {
		os.Remove(tmp)
		return err
	}
	now := h.clock()
	os.Chtimes(u.sidecar(), now, now) // the upload is alive
	if copied {
		writeXML(w, struct {
			XMLName      struct{} `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CopyPartResult"`
			LastModified string
			ETag         string
		}{LastModified: isoTime(now), ETag: `"` + etag + `"`})
		return nil
	}
	b.echo(w)
	w.Header().Set("ETag", `"`+etag+`"`)
	return nil
}

// copyRange parses x-amz-copy-source-range ("bytes=first-last"); none is
// the whole source.
func copyRange(v string, size int64) (start, end int64, err error) {
	if v == "" {
		return 0, size, nil
	}
	a, b, ok := strings.Cut(strings.TrimPrefix(v, "bytes="), "-")
	first, err1 := strconv.ParseInt(a, 10, 64)
	last, err2 := strconv.ParseInt(b, 10, 64)
	if !ok || err1 != nil || err2 != nil || first < 0 || last < first || last >= size {
		return 0, 0, errf(http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "x-amz-copy-source-range is not a range of the source")
	}
	return first, last + 1, nil
}

// completeUpload serves CompleteMultipartUpload: the listed parts, checked
// against the stored ones, are concatenated into the object, whose ETag is
// the MD5 of the parts' MD5s followed by "-" and the number of parts.
func (h *Handler) completeUpload(w http.ResponseWriter, r *http.Request, p auth.Principal, sig *signer, bucket, key, id string) error {
	rp, err := h.bucket(r, p, bucket, auth.Add, key)
	if err != nil {
		return err
	}
	unlock, err := h.Store.Lock(r.Context(), "s3\x00upload\x00"+id)
	if err != nil {
		return err
	}
	defer unlock()
	u, err := h.openUpload(rp, key, id)
	if err != nil {
		return err
	}
	var req struct {
		Part []struct {
			PartNumber int
			ETag       string
		}
	}
	if err := readXML(r, sig, &req); err != nil {
		return err
	}
	stored, err := u.parts()
	if err != nil {
		return err
	}
	if len(req.Part) == 0 {
		return errf(http.StatusBadRequest, "MalformedXML", "the request lists no parts")
	}
	all := md5.New()
	var paths []string
	var size int64
	for i, rq := range req.Part {
		if i > 0 && rq.PartNumber <= req.Part[i-1].PartNumber {
			return errf(http.StatusBadRequest, "InvalidPartOrder", "the parts must be listed in ascending order")
		}
		sp, ok := stored[rq.PartNumber]
		if !ok || strings.Trim(rq.ETag, `"`) != sp.md5 {
			return errf(http.StatusBadRequest, "InvalidPart", "part %d was not uploaded, or its ETag is not %s", rq.PartNumber, rq.ETag)
		}
		if i < len(req.Part)-1 && sp.size < minPartSize {
			return errf(http.StatusBadRequest, "EntityTooSmall", "part %d is smaller than 5 MiB and not the last", rq.PartNumber)
		}
		d, _ := hex.DecodeString(sp.md5)
		all.Write(d)
		paths = append(paths, sp.path)
		size += sp.size
	}
	if limit := h.maxUpload(); limit > 0 && size > limit {
		return errf(http.StatusRequestEntityTooLarge, "EntityTooLarge", "the object is larger than the maxUploadMiB setting allows")
	}
	if err := h.checkWrite(r, p, rp, key); err != nil {
		return err
	}
	pr := &partsReader{paths: paths}
	defer pr.Close()
	content, err := repo.StrictBody(rp, key, u.ContentType, pr)
	if err != nil {
		return err
	}
	at := u.Attrs
	at.ETag = hex.EncodeToString(all.Sum(nil)) + "-" + strconv.Itoa(len(req.Part))
	slow(w, r, func() (any, error) {
		sums, err := h.Store.WriteBlob(rp.Name, content, nil)
		if err != nil {
			return nil, err
		}
		if sums.Size != size {
			return nil, errf(http.StatusInternalServerError, "InternalError", "a part changed while the upload completed")
		}
		a, err := h.store(r, p, rp, key, u.ContentType, at, sums)
		if err != nil {
			return nil, err
		}
		u.remove()
		return struct {
			XMLName  struct{} `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CompleteMultipartUploadResult"`
			Location string
			Bucket   string
			Key      string
			ETag     string
		}{Location: "/" + rp.Name + "/" + key, Bucket: rp.Name, Key: key, ETag: etagOf(a)}, nil
	})
	return nil
}

// partsReader reads the files one after the other, opening each in turn.
type partsReader struct {
	paths []string
	f     *os.File
}

func (pr *partsReader) Read(b []byte) (int, error) {
	for {
		if pr.f == nil {
			if len(pr.paths) == 0 {
				return 0, io.EOF
			}
			f, err := os.Open(pr.paths[0])
			if err != nil {
				return 0, err
			}
			pr.f, pr.paths = f, pr.paths[1:]
		}
		n, err := pr.f.Read(b)
		if err == io.EOF {
			pr.f.Close()
			pr.f = nil
			if n == 0 {
				continue
			}
			err = nil
		}
		return n, err
	}
}

func (pr *partsReader) Close() {
	if pr.f != nil {
		pr.f.Close()
	}
}

func (h *Handler) abortUpload(w http.ResponseWriter, r *http.Request, p auth.Principal, bucket, key, id string) error {
	rp, err := h.bucket(r, p, bucket, auth.Add, key)
	if err != nil {
		return err
	}
	unlock, err := h.Store.Lock(r.Context(), "s3\x00upload\x00"+id)
	if err != nil {
		return err
	}
	defer unlock()
	u, err := h.openUpload(rp, key, id)
	if err != nil {
		return err
	}
	u.remove()
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) listParts(w http.ResponseWriter, r *http.Request, p auth.Principal, bucket, key, id string) error {
	rp, err := h.bucket(r, p, bucket, auth.Add, key)
	if err != nil {
		return err
	}
	u, err := h.openUpload(rp, key, id)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	marker, _ := strconv.Atoi(q.Get("part-number-marker"))
	maxN := 1000
	if v, err := strconv.Atoi(q.Get("max-parts")); err == nil && v >= 0 {
		maxN = min(v, 1000)
	}
	stored, err := u.parts()
	if err != nil {
		return err
	}
	type partXML struct {
		PartNumber   int
		LastModified string
		ETag         string
		Size         int64
	}
	var list []partXML
	for _, sp := range stored {
		if sp.n > marker {
			list = append(list, partXML{sp.n, isoTime(sp.mod), `"` + sp.md5 + `"`, sp.size})
		}
	}
	slices.SortFunc(list, func(a, b partXML) int { return a.PartNumber - b.PartNumber })
	truncated, next := len(list) > maxN, 0
	if truncated {
		list = list[:maxN]
	}
	if len(list) > 0 {
		next = list[len(list)-1].PartNumber
	}
	writeXML(w, struct {
		XMLName              struct{} `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListPartsResult"`
		Bucket               string
		Key                  string
		UploadID             string `xml:"UploadId"`
		Initiator            owner
		Owner                owner
		StorageClass         string
		PartNumberMarker     int
		NextPartNumberMarker int
		MaxParts             int
		IsTruncated          bool
		Part                 []partXML
	}{Bucket: rp.Name, Key: key, UploadID: id, Initiator: owner{u.Owner, u.Owner}, Owner: owner{u.Owner, u.Owner}, StorageClass: "STANDARD",
		PartNumberMarker: marker, NextPartNumberMarker: next, MaxParts: maxN, IsTruncated: truncated, Part: list})
	return nil
}

// listUploads serves ListMultipartUploads: the bucket's uploads in
// progress under the prefix, by key, at most 1000 (no paging).
func (h *Handler) listUploads(w http.ResponseWriter, r *http.Request, rp *repo.Repository) error {
	bs, err := h.Store.BlobStore(rp.Name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(bs.SessionPath("x"))
	prefix := r.URL.Query().Get("prefix")
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	type uploadXML struct {
		Key          string
		UploadID     string `xml:"UploadId"`
		Initiator    owner
		Owner        owner
		StorageClass string
		Initiated    string
	}
	var list []uploadXML
	pre := filepath.Base(bs.SessionPath("s3-"))
	for _, e := range entries {
		id, ok := strings.CutSuffix(strings.TrimPrefix(e.Name(), pre), ".json")
		if !ok || !strings.HasPrefix(e.Name(), pre) || !uploadIDRE.MatchString(id) {
			continue
		}
		var u upload
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err != nil || json.Unmarshal(b, &u) != nil {
			continue
		}
		if u.Repo == rp.Name && strings.HasPrefix(u.Key, prefix) {
			list = append(list, uploadXML{u.Key, id, owner{u.Owner, u.Owner}, owner{u.Owner, u.Owner}, "STANDARD", isoTime(u.Started)})
		}
	}
	slices.SortFunc(list, func(a, b uploadXML) int { return strings.Compare(a.Key+"\x00"+a.Initiated, b.Key+"\x00"+b.Initiated) })
	truncated := len(list) > 1000
	if truncated {
		list = list[:1000]
	}
	writeXML(w, struct {
		XMLName     struct{} `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListMultipartUploadsResult"`
		Bucket      string
		Prefix      string
		MaxUploads  int
		IsTruncated bool
		Upload      []uploadXML
	}{Bucket: rp.Name, Prefix: prefix, MaxUploads: 1000, IsTruncated: truncated, Upload: list})
	return nil
}
