package npm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"commandcenter/internal/repo"
)

// UploadDocument is what `npm publish` sends for the package tarball f of
// size bytes, for the component upload API: the package name, which the
// publish URL names, and the document with the tarball's package.json as
// its one version, tagged latest, and f as its attachment, base64-encoded
// as it is read. The publish sets the version's dist from the stored
// tarball, so the document has none. A tarball without a valid package.json
// is a 400. Closing doc stops the encoding.
func UploadDocument(ctx context.Context, f io.ReaderAt, size int64) (name string, doc io.ReadCloser, err error) {
	bad := func(format string, a ...any) error {
		return repo.Errorf(http.StatusBadRequest, "invalid npm tarball: %s", fmt.Sprintf(format, a...))
	}
	left := maxTarballBytes
	pj, err := packageJSON(ctx, io.NewSectionReader(f, 0, size), &left, bad)
	if err != nil {
		return "", nil, err
	}
	name, version := pj.Name, cleanVersion(pj.Version)
	if name == "" || version == "" {
		return "", nil, bad("package.json has no name or version")
	}
	var man map[string]json.RawMessage
	if err := json.Unmarshal(pj.Raw, &man); err != nil {
		return "", nil, bad("package.json: %v", err)
	}
	delete(man, "dist")
	man["version"], _ = json.Marshal(version)
	man["_id"], _ = json.Marshal(name + "@" + version)
	head, err := json.Marshal(map[string]any{"_id": name, "name": name, "dist-tags": map[string]string{"latest": version},
		"versions": map[string]any{version: man}})
	if err != nil {
		return "", nil, bad("package.json: %v", err)
	}
	file, _ := json.Marshal(TarballFile(name, version))
	// The keys come sorted, "versions" last: the attachment goes before the closing brace.
	prefix := string(head[:len(head)-1]) + `,"_attachments":{` + string(file) +
		`:{"content_type":"application/octet-stream","length":` + strconv.FormatInt(size, 10) + `,"data":"`
	pr, pw := io.Pipe()
	go func() {
		enc := base64.NewEncoder(base64.StdEncoding, pw)
		_, err := io.Copy(enc, io.NewSectionReader(f, 0, size))
		if err == nil {
			err = enc.Close()
		}
		pw.CloseWithError(err)
	}()
	return name, struct {
		io.Reader
		io.Closer
	}{io.MultiReader(strings.NewReader(prefix), pr, strings.NewReader(`"}}}`)), pr}, nil
}
