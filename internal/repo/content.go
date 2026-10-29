package repo

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
)

// Strict content type validation (Nexus): a hosted repository with
// strictContentTypeValidation refuses an upload whose first bytes contradict
// what its path promises, or, when the path says nothing, the declared
// Content-Type's major type (text/* that sniffs as binary, image/* that is
// a zip). A format package passes its upload body through StrictBody and
// stores the reader it returns (raw, maven). npm, PyPI, NuGet and Helm parse
// every upload as a package whatever the setting, so they need no hook.

// magic is what an extension's content must start with.
var magic = map[string][]byte{
	".jar": []byte("PK\x03\x04"), ".war": []byte("PK\x03\x04"), ".ear": []byte("PK\x03\x04"), ".zip": []byte("PK\x03\x04"),
	".whl": []byte("PK\x03\x04"), ".egg": []byte("PK\x03\x04"), ".nupkg": []byte("PK\x03\x04"),
	".tgz": {0x1f, 0x8b}, ".gz": {0x1f, 0x8b}, ".crate": {0x1f, 0x8b},
	".rpm": {0xed, 0xab, 0xee, 0xdb}, ".deb": []byte("!<arch>\n"),
	".pom": []byte("<"), ".xml": []byte("<"),
}

func errContentType(p, why string) error {
	return Errorf(http.StatusBadRequest, "content of %s does not match its type (strict content type validation): %s", p, why)
}

// StrictBody is body when rp does not validate content types, else
// CheckContent's reader.
func StrictBody(rp *Repository, p, declared string, body io.Reader) (io.Reader, error) {
	if !rp.StrictContentTypeValidation {
		return body, nil
	}
	return CheckContent(p, declared, body)
}

// CheckContent peeks at body and returns a reader that replays it in full,
// or a 400 error when the content contradicts path p's extension or the
// declared Content-Type. Nothing is read past the first 512 bytes.
func CheckContent(p, declared string, body io.Reader) (io.Reader, error) {
	br := bufio.NewReaderSize(body, 512)
	head, err := br.Peek(512)
	if err != nil && err != io.EOF {
		return nil, err
	}
	ext := strings.ToLower(path.Ext(p))
	if want, ok := magic[ext]; ok {
		if string(want) == "<" { // XML may start with a byte order mark and blank lines
			head = bytes.TrimLeft(head, " \t\r\n\xef\xbb\xbf")
		}
		if !bytes.HasPrefix(head, want) {
			return nil, errContentType(p, "not a valid "+strings.TrimPrefix(ext, ".")+" file")
		}
		return br, nil
	}
	mt, _, perr := mime.ParseMediaType(declared)
	if perr != nil || mt == "application/octet-stream" || mt == "image/svg+xml" || len(head) == 0 {
		return br, nil // nothing declared, or a type the sniffer reads as text
	}
	sniffed := http.DetectContentType(head)
	major := func(s string) string { m, _, _ := strings.Cut(s, "/"); return m }
	switch {
	case major(mt) == "text" && !strings.HasPrefix(sniffed, "text/"):
		return nil, errContentType(p, fmt.Sprintf("declared %s but the content is binary (%s)", mt, sniffed))
	case (major(mt) == "image" || major(mt) == "video" || major(mt) == "audio") && major(sniffed) != major(mt):
		return nil, errContentType(p, fmt.Sprintf("declared %s but the content looks like %s", mt, sniffed))
	}
	return br, nil
}

// SetContentDisposition sets the Content-Disposition of a download of p:
// disposition is a repository's contentDisposition ("" sets nothing).
func SetContentDisposition(w http.ResponseWriter, disposition, p string) {
	switch disposition {
	case "inline":
		w.Header().Set("Content-Disposition", "inline")
	case "attachment":
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(p)}))
	}
}
