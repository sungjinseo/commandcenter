package s3

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Multipart uploads. An upload is a row (meta.S3Upload) and a directory in
// its bucket's blob store tmp (blob.Store.UploadDir) holding each part as
// <n>.part with its MD5 in <n>.md5. Complete joins the parts into one blob,
// so the object is an ordinary asset. The directory is in the shared tmp
// of a multi-node setup, so any node may take any part.

const (
	maxPartNumber = 10000
	minPartSize   = 5 << 20 // every part but the last
	// keepAliveEvery is how often Complete writes a space while it joins the
	// parts, so clients and proxies do not time the request out.
	keepAliveEvery = 10 * time.Second
	// UploadMaxAge is how long an upload may go without a part before
	// SweepUploads removes it.
	UploadMaxAge = 7 * 24 * time.Hour
)

func validUploadID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

type initiateResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

func (h *Handler) createUpload(q *req, rp *repo.Repository) error {
	if q.p.Anonymous() {
		return errAccessDenied
	}
	hdrs, err := objectHeaders(q.r.Header)
	if err != nil {
		return err
	}
	bs, err := h.Store.BlobStore(rp.Name)
	if err != nil {
		return err
	}
	b := make([]byte, 16)
	rand.Read(b)
	id := hex.EncodeToString(b)
	if err := os.MkdirAll(bs.UploadDir(id), 0o755); err != nil {
		return err
	}
	now := h.now().UTC()
	u := meta.S3Upload{ID: id, Repo: rp.Name, Path: q.key, Initiator: q.p.Owner(), ContentType: contentType(q.r.Header),
		Headers: hdrs, Started: now, Updated: now}
	if err := h.Store.DB.PutS3Upload(q.ctx, u); err != nil {
		os.RemoveAll(bs.UploadDir(id))
		return err
	}
	writeXML(q.w, q.r, initiateResult{Xmlns: xmlns, Bucket: rp.Name, Key: q.key, UploadID: id})
	return nil
}

// upload returns q's upload (uploadId) of rp and q's key, and its
// directory. Only its initiator or an admin may use it.
func (h *Handler) upload(q *req, rp *repo.Repository) (meta.S3Upload, string, error) {
	id := q.get("uploadId")
	if !validUploadID(id) {
		return meta.S3Upload{}, "", errNoSuchUpload
	}
	u, err := h.Store.DB.GetS3Upload(q.ctx, id)
	if errors.Is(err, meta.ErrNotFound) || err == nil && (u.Repo != rp.Name || u.Path != q.key) {
		return meta.S3Upload{}, "", errNoSuchUpload
	}
	if err != nil {
		return meta.S3Upload{}, "", err
	}
	if u.Initiator != q.p.Owner() && !q.p.Admin {
		return meta.S3Upload{}, "", errAccessDenied
	}
	bs, err := h.Store.BlobStore(rp.Name)
	if err != nil {
		return meta.S3Upload{}, "", err
	}
	return u, bs.UploadDir(id), nil
}

func partNumber(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxPartNumber {
		return 0, invalidArgument("Part number must be an integer between 1 and 10000, inclusive")
	}
	return n, nil
}

func (h *Handler) uploadPart(q *req, rp *repo.Repository) error {
	n, err := partNumber(q.get("partNumber"))
	if err != nil {
		return err
	}
	u, dir, err := h.upload(q, rp)
	if err != nil {
		return err
	}
	sums, err := q.checksums()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, strconv.Itoa(n)+".tmp-*")
	if errors.Is(err, os.ErrNotExist) {
		return errNoSuchUpload // aborted or completed meanwhile
	}
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once renamed
	m, s := md5.New(), sha256.New()
	size, err := io.Copy(io.MultiWriter(f, m, s), sums.wrap(q.body))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	got := blob.Sums{MD5: hex.EncodeToString(m.Sum(nil)), SHA256: hex.EncodeToString(s.Sum(nil)), Size: size}
	if err := sums.verify(got); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, strconv.Itoa(n)+".part")); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, strconv.Itoa(n)+".md5"), []byte(got.MD5)); err != nil {
		return err
	}
	h.Store.DB.TouchS3Upload(q.ctx, u.ID, h.now())
	q.w.Header().Set("ETag", quoteETag(got.MD5))
	q.w.WriteHeader(http.StatusOK)
	return nil
}

func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// part is one stored part of an upload.
type part struct {
	n        int
	etag     string
	size     int64
	modified time.Time
}

// parts reads the parts stored in dir, by number.
func parts(dir string) ([]part, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoSuchUpload
	}
	if err != nil {
		return nil, err
	}
	var out []part
	for _, e := range entries {
		num, ok := strings.CutSuffix(e.Name(), ".part")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		etag, err := os.ReadFile(filepath.Join(dir, num+".md5"))
		if err != nil {
			continue // the part's checksum is still being written
		}
		out = append(out, part{n: n, etag: string(etag), size: info.Size(), modified: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].n < out[j].n })
	return out, nil
}

type listPartsResult struct {
	XMLName              xml.Name   `xml:"ListPartsResult"`
	Xmlns                string     `xml:"xmlns,attr"`
	Bucket               string     `xml:"Bucket"`
	Key                  string     `xml:"Key"`
	UploadID             string     `xml:"UploadId"`
	Initiator            owner      `xml:"Initiator"`
	Owner                owner      `xml:"Owner"`
	StorageClass         string     `xml:"StorageClass"`
	PartNumberMarker     int        `xml:"PartNumberMarker"`
	NextPartNumberMarker int        `xml:"NextPartNumberMarker"`
	MaxParts             int        `xml:"MaxParts"`
	IsTruncated          bool       `xml:"IsTruncated"`
	Parts                []partInfo `xml:"Part"`
}

type partInfo struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

func (h *Handler) listParts(q *req, rp *repo.Repository) error {
	u, dir, err := h.upload(q, rp)
	if err != nil {
		return err
	}
	all, err := parts(dir)
	if err != nil {
		return err
	}
	max := 1000
	if v := q.get("max-parts"); v != "" {
		if max, err = strconv.Atoi(v); err != nil || max < 0 {
			return invalidArgument("max-parts must be a non-negative integer")
		}
		max = min(max, 1000)
	}
	marker, _ := strconv.Atoi(q.get("part-number-marker"))
	o := owner{ID: u.Initiator, DisplayName: u.Initiator}
	out := listPartsResult{Xmlns: xmlns, Bucket: rp.Name, Key: u.Path, UploadID: u.ID, Initiator: o, Owner: o,
		StorageClass: "STANDARD", PartNumberMarker: marker, MaxParts: max, Parts: []partInfo{}}
	for _, p := range all {
		if p.n <= marker {
			continue
		}
		if len(out.Parts) == max {
			out.IsTruncated = true
			break
		}
		out.Parts = append(out.Parts, partInfo{PartNumber: p.n, LastModified: s3Time(p.modified), ETag: quoteETag(p.etag), Size: p.size})
		out.NextPartNumberMarker = p.n
	}
	writeXML(q.w, q.r, out)
	return nil
}

type completeRequest struct {
	Parts []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	} `xml:"Part"`
}

type completeResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

func (h *Handler) completeUpload(q *req, rp *repo.Repository) error {
	u, dir, err := h.upload(q, rp)
	if err != nil {
		return err
	}
	var body completeRequest
	if err := q.readXML(&body); err != nil {
		return err
	}
	if len(body.Parts) == 0 {
		return s3err(http.StatusBadRequest, "MalformedXML", "You must specify at least one part")
	}
	// One completion at a time: a retry waits and then finds the upload gone.
	unlock, err := h.Store.Lock(q.ctx, "s3\x00upload\x00"+u.ID)
	if err != nil {
		return err
	}
	defer unlock()
	have, err := parts(dir)
	if err != nil {
		return err
	}
	byNum := map[int]part{}
	for _, p := range have {
		byNum[p.n] = p
	}
	var files []string
	etags := md5.New()
	for i, want := range body.Parts {
		if i > 0 && want.PartNumber <= body.Parts[i-1].PartNumber {
			return s3err(http.StatusBadRequest, "InvalidPartOrder", "The list of parts was not in ascending order. The parts list must be specified in order by part number.")
		}
	}
	for i, want := range body.Parts {
		p, ok := byNum[want.PartNumber]
		if !ok || strings.Trim(want.ETag, `" `) != p.etag {
			return s3err(http.StatusBadRequest, "InvalidPart", "One or more of the specified parts could not be found. The part might not have been uploaded, or the specified entity tag might not have matched the part's entity tag.")
		}
		if i < len(body.Parts)-1 && p.size < minPartSize {
			return s3err(http.StatusBadRequest, "EntityTooSmall", "Your proposed upload is smaller than the minimum allowed object size.")
		}
		raw, _ := hex.DecodeString(p.etag)
		etags.Write(raw)
		files = append(files, filepath.Join(dir, strconv.Itoa(p.n)+".part"))
	}
	etag := hex.EncodeToString(etags.Sum(nil)) + "-" + strconv.Itoa(len(body.Parts))

	// Joining the parts takes as long as copying them: answer 200 now and
	// keep the connection busy; the result or the error is the body.
	w := q.w
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("X-Amz-Request-Id", q.r.Header.Get("X-Request-Id"))
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(xml.Header))
	rc := http.NewResponseController(w)
	rc.Flush()
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(keepAliveEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				mu.Lock()
				w.Write([]byte(" "))
				rc.Flush()
				mu.Unlock()
			}
		}
	}()
	a, err := h.join(q, rp, u, files, etag)
	close(done)
	mu.Lock()
	defer mu.Unlock()
	enc := xml.NewEncoder(w)
	if err != nil {
		e := toS3(q.r, err)
		if e.Status >= 500 {
			logFailure(q.r, err)
		}
		enc.Encode(errBody(q.r, e))
		return nil
	}
	h.Store.DB.DeleteS3Upload(context.WithoutCancel(q.ctx), u.ID)
	os.RemoveAll(dir)
	enc.Encode(completeResult{Xmlns: xmlns, Location: "/" + rp.Name + "/" + a.Path, Bucket: rp.Name, Key: a.Path, ETag: quoteETag(etag)})
	return nil
}

// join writes the parts as one blob and records it as the upload's object.
func (h *Handler) join(q *req, rp *repo.Repository, u meta.S3Upload, files []string, etag string) (meta.Asset, error) {
	var readers []io.Reader
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			return meta.Asset{}, err
		}
		readers, closers = append(readers, f), append(closers, f)
	}
	sums, err := h.Store.WriteBlob(rp.Name, io.MultiReader(readers...), nil)
	if err != nil {
		return meta.Asset{}, err
	}
	return h.commit(q, rp, u.Path, u.ContentType, sums, meta.S3ObjectMeta{ETag: etag, Headers: u.Headers})
}

func (h *Handler) abortUpload(q *req, rp *repo.Repository) error {
	u, dir, err := h.upload(q, rp)
	if err != nil {
		return err
	}
	if err := h.Store.DB.DeleteS3Upload(q.ctx, u.ID); err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return errNoSuchUpload
		}
		return err
	}
	os.RemoveAll(dir)
	q.w.WriteHeader(http.StatusNoContent)
	return nil
}

type listUploadsResult struct {
	XMLName            xml.Name       `xml:"ListMultipartUploadsResult"`
	Xmlns              string         `xml:"xmlns,attr"`
	Bucket             string         `xml:"Bucket"`
	KeyMarker          string         `xml:"KeyMarker"`
	UploadIDMarker     string         `xml:"UploadIdMarker"`
	NextKeyMarker      string         `xml:"NextKeyMarker"`
	NextUploadIDMarker string         `xml:"NextUploadIdMarker"`
	Prefix             string         `xml:"Prefix"`
	MaxUploads         int            `xml:"MaxUploads"`
	IsTruncated        bool           `xml:"IsTruncated"`
	Uploads            []uploadInfo   `xml:"Upload"`
	CommonPrefixes     []commonPrefix `xml:"CommonPrefixes"`
}

type uploadInfo struct {
	Key          string `xml:"Key"`
	UploadID     string `xml:"UploadId"`
	Initiator    owner  `xml:"Initiator"`
	Owner        owner  `xml:"Owner"`
	StorageClass string `xml:"StorageClass"`
	Initiated    string `xml:"Initiated"`
}

func (h *Handler) listUploads(q *req, rp *repo.Repository) error {
	max := 1000
	if v := q.get("max-uploads"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return invalidArgument("max-uploads must be a non-negative integer")
		}
		max = min(n, 1000)
	}
	prefix, keyMarker, idMarker := q.get("prefix"), q.get("key-marker"), q.get("upload-id-marker")
	if idMarker == "" && keyMarker != "" {
		idMarker = "\U0010FFFF" // after every upload of the key marker
	}
	list, err := h.Store.DB.ListS3Uploads(q.ctx, rp.Name, prefix, keyMarker, idMarker, max+1)
	if err != nil {
		return err
	}
	out := listUploadsResult{Xmlns: xmlns, Bucket: rp.Name, KeyMarker: keyMarker, UploadIDMarker: q.get("upload-id-marker"),
		Prefix: prefix, MaxUploads: max, Uploads: []uploadInfo{}, CommonPrefixes: []commonPrefix{}}
	for i, u := range list {
		if i == max {
			out.IsTruncated = true
			break
		}
		o := owner{ID: u.Initiator, DisplayName: u.Initiator}
		out.Uploads = append(out.Uploads, uploadInfo{Key: u.Path, UploadID: u.ID, Initiator: o, Owner: o, StorageClass: "STANDARD", Initiated: s3Time(u.Started)})
		out.NextKeyMarker, out.NextUploadIDMarker = u.Path, u.ID
	}
	writeXML(q.w, q.r, out)
	return nil
}

// SweepUploads removes the uploads no part arrived for in olderThan and
// returns how many. Every node may run it: the row's delete decides who
// removes the directory.
func (h *Handler) SweepUploads(ctx context.Context, olderThan time.Duration) (int, error) {
	stale, err := h.Store.DB.StaleS3Uploads(ctx, h.now().Add(-olderThan))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, u := range stale {
		if err := h.Store.DB.DeleteS3Upload(ctx, u.ID); err != nil {
			continue
		}
		if bs, err := h.Store.BlobStore(u.Repo); err == nil {
			os.RemoveAll(bs.UploadDir(u.ID))
		}
		n++
	}
	return n, nil
}
