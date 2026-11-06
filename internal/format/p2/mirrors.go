package p2

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/xz"
)

const (
	// maxArtifacts bounds the XML of an artifacts document (tens of MiB for
	// the largest release sites); a larger one is served as the remote sent it.
	maxArtifacts = 512 << 20
	// maxTag bounds one tag, and the properties element held while its
	// properties are read (an element holds PGP keys of some KiB).
	maxTag = 8 << 20
	// maxXZWindow is the dictionary an artifacts.xml.xz may declare: what
	// xz -9 uses. Allocated as the output grows.
	maxXZWindow = 64 << 20
)

// isArtifacts reports whether file is an artifacts document.
func isArtifacts(file string) bool {
	return file == "artifacts.xml" || file == "artifacts.jar" || file == "artifacts.xml.xz"
}

var (
	propertiesStartRE = regexp.MustCompile(`^<properties(\s[^>]*[^/>])?\s*>$`)
	propertiesEndRE   = regexp.MustCompile(`^</properties\s*>$`)
	propertyEndRE     = regexp.MustCompile(`^</property\s*>$`)
	propertyRE        = regexp.MustCompile(`^<property\s`)
	mirrorsRE         = regexp.MustCompile(`^<property\s[^>]*\bname\s*=\s*["']p2\.mirrorsURL["']`)
	sizeRE            = regexp.MustCompile(`(\bsize\s*=\s*["'])[0-9]*(["'])`)
)

var errTagTooLarge = errors.New("a tag or properties element is too large")

// withoutMirrors returns the artifacts document a of the proxy rp without
// p2.mirrorsURL: with it, a client fetches a mirror list from that URL and
// the bundles from the mirrors, not through the proxy. A document that has
// the property is replaced in the cache, once, and keeps the remote's
// validators: a 304 keeps the rewritten one, a 200 brings a new one to rewrite
// (as Nexus stores it rewritten). It gets a Last-Modified after the remote's,
// so a client that cached the remote's bytes reads it again. A document
// that cannot be read is served as it came, logged: a client may still make
// more of it than this does.
func (h *Handler) withoutMirrors(ctx context.Context, rp *repo.Repository, a meta.Asset) meta.Asset {
	if _, ok := h.noMirrors.Load(a.BlobSHA256); ok {
		return a
	}
	out, err := h.rewriteMirrors(ctx, rp, a)
	if err != nil {
		slog.Warn("p2 artifacts document served as the remote sent it", "repo", rp.Name, "path", a.Path, "err", logsafe.Err(err))
		return a
	}
	// ponytail: one 64-byte key per document version a node has seen; an LRU
	// if a proxy of nightly sites runs for years.
	h.noMirrors.Store(out.BlobSHA256, struct{}{})
	return out
}

func (h *Handler) rewriteMirrors(ctx context.Context, rp *repo.Repository, a meta.Asset) (meta.Asset, error) {
	unlock, err := h.Store.Lock(ctx, "p2\x00"+rp.Name+"\x00"+a.Path)
	if err != nil {
		return a, err
	}
	defer unlock()
	for range 3 { // the fetcher does not take this lock: a new document may come while one is rewritten
		// Another request or node may have rewritten it meanwhile.
		cur, err := h.Store.Get(ctx, rp.Name, a.Path)
		if err != nil {
			return a, err
		}
		var found bool
		err = readArtifacts(h.Store, cur, func(r io.Reader) (err error) {
			found, err = stripMirrors(io.Discard, r)
			return err
		})
		if err != nil || !found {
			return cur, err
		}
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(writeWithoutMirrors(pw, h.Store, cur)) }()
		sums, err := h.Store.WriteBlob(rp.Name, pr, nil)
		pr.CloseWithError(err)
		if err != nil {
			return cur, err
		}
		out := h.Store.NewAsset(rp.Name, cur.Path, cur.ContentType, sums)
		out.RemoteETag, out.RemoteLastModified, out.CheckedAt = cur.RemoteETag, cur.RemoteLastModified, cur.CheckedAt
		out.LastModified = cur.LastModified.Add(time.Second)
		replaced := false
		err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
			// Every writer waits for the database's one write lock, so
			// nothing comes between this read and this write.
			now, err := tx.GetAsset(ctx, rp.Name, cur.Path)
			if err != nil || now.BlobSHA256 != cur.BlobSHA256 {
				return err
			}
			replaced = true
			return tx.PutAsset(ctx, out)
		})
		if err != nil || replaced {
			return out, err
		}
	}
	return a, errors.New("the document keeps changing")
}

// readArtifacts calls fn with the XML of the artifacts document a, whichever
// its form.
func readArtifacts(store *repo.Store, a meta.Asset, fn func(io.Reader) error) error {
	f, err := store.Open(a)
	if err != nil {
		return err
	}
	defer f.Close()
	xml, closeXML, err := openXML(f, a)
	if err != nil {
		return err
	}
	defer closeXML()
	return fn(xml)
}

func openXML(f blob.File, a meta.Asset) (io.Reader, func() error, error) {
	none := func() error { return nil }
	switch {
	case strings.HasSuffix(a.Path, ".jar"):
		zr, err := zip.NewReader(f, a.Size)
		if err != nil {
			return nil, none, err
		}
		e, err := zr.Open("artifacts.xml")
		if err != nil {
			return nil, none, err
		}
		return e, e.Close, nil
	case strings.HasSuffix(a.Path, ".xz"):
		r, err := xz.NewReader(f, xz.Options{MaxWindow: maxXZWindow})
		return r, none, err
	}
	return f, none, nil
}

// writeWithoutMirrors writes the artifacts document a to w without
// p2.mirrorsURL, in its form: an .xml.xz stored, not compressed (xz.StoredWriter);
// a jar with only its artifacts.xml, as what else it held would no longer match.
func writeWithoutMirrors(w io.Writer, store *repo.Store, a meta.Asset) error {
	return readArtifacts(store, a, func(xml io.Reader) error {
		switch {
		case strings.HasSuffix(a.Path, ".jar"):
			zw := zip.NewWriter(w)
			e, err := zw.CreateHeader(&zip.FileHeader{Name: "artifacts.xml", Method: zip.Deflate})
			if err != nil {
				return err
			}
			if _, err := stripMirrors(e, xml); err != nil {
				return err
			}
			return zw.Close()
		case strings.HasSuffix(a.Path, ".xz"):
			xw := xz.NewStoredWriter(w)
			if _, err := stripMirrors(xw, xml); err != nil {
				return err
			}
			return xw.Close()
		}
		_, err := stripMirrors(w, xml)
		return err
	})
}

// stripMirrors copies the XML of r to w, a tag at a time so that a document
// of any size takes little memory, without the p2.mirrorsURL properties,
// correcting the size of the properties element that held them. It reports
// whether it removed one. A properties element is held back until its end tag.
// A property written with an end tag of its own loses that too.
func stripMirrors(out io.Writer, r io.Reader) (removed bool, err error) {
	br, w := bufio.NewReaderSize(r, 64<<10), bufio.NewWriterSize(out, 64<<10)
	var (
		chunk, start, body []byte
		inside, dropped    bool
		dropEnd            bool
		count              int
		total              int64
	)
	for {
		chunk, err = readChunk(br, chunk[:0])
		if total += int64(len(chunk)); total > maxArtifacts {
			return false, errors.New("the document is too large")
		}
		if err != nil && err != io.EOF {
			return false, err
		}
		tag := chunk
		if i := bytes.LastIndexByte(chunk, '<'); i >= 0 {
			tag = chunk[i:]
		}
		switch {
		case err == io.EOF:
			if inside {
				return false, errors.New("the document ends inside a properties element")
			}
			w.Write(chunk)
			return removed, w.Flush()
		case dropEnd:
			if !propertyEndRE.Match(tag) {
				return false, errors.New("a p2.mirrorsURL property that is not closed")
			}
			dropEnd = false
		case !inside && propertiesStartRE.Match(tag):
			inside, dropped, count = true, false, 0
			start, body = append(start[:0], chunk...), body[:0]
		case !inside:
			w.Write(chunk) // a failure comes back from every later call, and Flush
		case propertiesEndRE.Match(tag):
			inside = false
			if dropped {
				removed = true
				start = sizeRE.ReplaceAll(start, []byte("${1}"+strconv.Itoa(count)+"${2}"))
			}
			w.Write(start)
			w.Write(body)
			w.Write(chunk)
		case mirrorsRE.Match(tag):
			dropped, dropEnd = true, !bytes.HasSuffix(tag, []byte("/>"))
		default:
			if propertyRE.Match(tag) {
				count++
			}
			if body = append(body, chunk...); len(body) > maxTag {
				return false, errTagTooLarge
			}
		}
	}
}

// readChunk appends to buf what r holds up to and including the next '>':
// text and a tag. At the end of the data it is what is left, and io.EOF.
func readChunk(r *bufio.Reader, buf []byte) ([]byte, error) {
	for {
		frag, err := r.ReadSlice('>')
		buf = append(buf, frag...)
		if err != bufio.ErrBufferFull {
			return buf, err
		}
		if len(buf) > maxTag {
			return buf, errTagTooLarge
		}
	}
}
