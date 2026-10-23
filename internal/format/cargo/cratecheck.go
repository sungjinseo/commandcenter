package cargo

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"strings"

	"commandcenter/internal/repo"
)

// Bounds on the gzip+tar stream checkCrate reads. The total keeps a gzip bomb
// from pinning the CPU; none of them depends on a size an archive claims.
// Tests may lower them.
var (
	maxCrateEntries       = 10000
	maxCrateEntry   int64 = 50 << 20
	maxCrateTotal   int64 = 512 << 20
)

// checkCrate verifies that r is a gzip+tar .crate for name@vers: every entry
// sits under `<name>-<vers>/`, no path is absolute or climbs with `..`, and
// `<name>-<vers>/Cargo.toml` is present. Errors are 400s.
func checkCrate(r io.Reader, name, vers string) error {
	bad := func(format string, a ...any) error {
		return repo.Errorf(http.StatusBadRequest, "invalid .crate: "+format, a...)
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return bad("not a gzip archive")
	}
	defer gz.Close()
	lr := &io.LimitedReader{R: gz, N: maxCrateTotal + 1}
	tr := tar.NewReader(lr)
	root := name + "-" + vers
	manifest := false
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if lr.N <= 0 {
				return bad("more than %d bytes uncompressed", maxCrateTotal)
			}
			return bad("not a tar archive: %v", err)
		}
		if n >= maxCrateEntries {
			return bad("more than %d entries", maxCrateEntries)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeRegA, tar.TypeDir:
		case tar.TypeSymlink, tar.TypeLink:
			return bad("entry %q is a link", h.Name)
		default:
			// Sparse files make tar.Reader synthesize zeros that cost no
			// stream bytes, so only plain files and directories are allowed.
			return bad("entry %q has unsupported type %q", h.Name, h.Typeflag)
		}
		for k := range h.PAXRecords {
			if strings.HasPrefix(k, "GNU.sparse.") {
				return bad("entry %q is a sparse file", h.Name)
			}
		}
		p := h.Name
		if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
			return bad("entry %q has an absolute or unsafe path", h.Name)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." {
				return bad("entry %q contains ..", p)
			}
		}
		if p != root && p != root+"/" && !strings.HasPrefix(p, root+"/") {
			return bad("entry %q is outside %s/", p, root)
		}
		if h.Size > maxCrateEntry {
			return bad("entry %q is larger than %d bytes", p, maxCrateEntry)
		}
		if p == root+"/Cargo.toml" && h.Typeflag == tar.TypeReg {
			manifest = true
		}
		written, err := io.Copy(io.Discard, tr)
		total += written
		if err != nil {
			if lr.N <= 0 || total > maxCrateTotal {
				return bad("more than %d bytes uncompressed", maxCrateTotal)
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return bad("truncated archive")
			}
			return bad("corrupt archive: %v", err)
		}
		if lr.N <= 0 || total > maxCrateTotal {
			return bad("more than %d bytes uncompressed", maxCrateTotal)
		}
	}
	if !manifest {
		return bad("%s/Cargo.toml is missing", root)
	}
	return nil
}
