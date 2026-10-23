package golang

import (
	"archive/zip"
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

const maxDoc = 1 << 20

// modulePath returns the path declared by the module directive of a go.mod.
func modulePath(r io.Reader) (string, error) {
	sc := bufio.NewScanner(io.LimitReader(r, maxDoc))
	sc.Buffer(make([]byte, 64<<10), maxDoc)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "module" {
			continue
		}
		p := fields[1]
		if strings.HasPrefix(p, `"`) || strings.HasPrefix(p, "`") {
			u, err := strconv.Unquote(p)
			if err != nil {
				return "", err
			}
			p = u
		}
		return p, nil
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New("go.mod has no module directive")
}

const maxZipDirBytes = 4 << 20

// maxZipEntries is a variable so tests can lower it.
var maxZipEntries = 50000

// checkDirectory bounds the central directory using the end-of-central-directory
// record before archive/zip allocates for it. Zip64 archives are not supported.
func checkDirectory(r io.ReaderAt, size int64) error {
	n := size
	if n > 65557 {
		n = 65557
	}
	notZip := repo.Errorf(http.StatusBadRequest, "not a valid zip file")
	if n < 22 {
		return notZip
	}
	tail := make([]byte, n)
	if _, err := r.ReadAt(tail, size-n); err != nil && err != io.EOF {
		return err
	}
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) != 0x06054b50 {
			continue
		}
		// A signature whose comment runs past the end is not a record archive/zip
		// accepts (the zip read fails with ErrFormat), so skip it here and let
		// that read reject the file.
		if i+22+int(binary.LittleEndian.Uint16(tail[i+20:])) > len(tail) {
			continue
		}
		eocdPos := size - int64(len(tail)) + int64(i)
		count := binary.LittleEndian.Uint16(tail[i+10:])
		dirSize := binary.LittleEndian.Uint32(tail[i+12:])
		dirOffset := binary.LittleEndian.Uint32(tail[i+16:])
		// The locator may lie before the tail window, so read it from the source.
		locator := false
		if eocdPos >= 20 {
			var b [4]byte
			if _, err := r.ReadAt(b[:], eocdPos-20); err != nil && err != io.EOF {
				return err
			}
			locator = binary.LittleEndian.Uint32(b[:]) == 0x07064b50
		}
		zip64 := count == 0xFFFF || dirSize == 0xFFFF || dirSize == 0xFFFFFFFF || dirOffset == 0xFFFFFFFF || locator
		switch {
		case zip64:
			return repo.Errorf(http.StatusBadRequest, "zip64 module zips are not supported")
		case int64(dirOffset)+int64(dirSize) != eocdPos:
			return repo.Errorf(http.StatusBadRequest, "zip central directory does not end at the end-of-directory record")
		case int(count) > maxZipEntries:
			return repo.Errorf(http.StatusBadRequest, "zip has %d entries, limit is %d", count, maxZipEntries)
		case dirSize > maxZipDirBytes:
			return repo.Errorf(http.StatusBadRequest, "zip central directory is %d bytes, limit is %d", dirSize, maxZipDirBytes)
		}
		return nil
	}
	return notZip
}

// checkZip validates a module zip: every entry lies under "{module}@{version}/"
// and a root go.mod declares module. module and version are unescaped.
func checkZip(r io.ReaderAt, size int64, module, version string) error {
	if err := checkDirectory(r, size); err != nil {
		return err
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return repo.Errorf(http.StatusBadRequest, "not a valid zip file: %v", err)
	}
	// archive/zip counts entries modulo 65536, so bound the real number too.
	if len(zr.File) > maxZipEntries {
		return repo.Errorf(http.StatusBadRequest, "zip has %d entries, limit is %d", len(zr.File), maxZipEntries)
	}
	prefix := module + "@" + version + "/"
	names := make(map[string]bool, len(zr.File))
	folded := make(map[string]string, len(zr.File))
	var unzipped uint64
	for _, f := range zr.File {
		rel, ok := strings.CutPrefix(f.Name, prefix)
		if !ok || rel == "" {
			return repo.Errorf(http.StatusBadRequest, "zip entry %q is outside %s", f.Name, prefix)
		}
		if names[f.Name] {
			return repo.Errorf(http.StatusBadRequest, "zip has duplicate entry %q", f.Name)
		}
		names[f.Name] = true
		lower := strings.ToLower(f.Name)
		if other, dup := folded[lower]; dup {
			return repo.Errorf(http.StatusBadRequest, "zip entries %q and %q differ only in case", other, f.Name)
		}
		folded[lower] = f.Name
		if err := checkFilePath(rel); err != nil {
			return repo.Errorf(http.StatusBadRequest, "zip entry %q: %v", f.Name, err)
		}
		// the declared sizes are what the go client sums; capping each term
		// keeps the running total from wrapping.
		if f.UncompressedSize64 > maxUnzipped || unzipped+f.UncompressedSize64 > maxUnzipped {
			return repo.Errorf(http.StatusBadRequest, "zip uncompressed size exceeds %d bytes", int64(maxUnzipped))
		}
		unzipped += f.UncompressedSize64
		for _, seg := range strings.Split(rel, "/") {
			if seg == ".." || seg == "." {
				return repo.Errorf(http.StatusBadRequest, "zip entry %q has a relative path element", f.Name)
			}
		}
		if rel != "go.mod" {
			continue
		}
		if strings.HasSuffix(version, "+incompatible") {
			return repo.Errorf(http.StatusBadRequest, "a +incompatible module zip must not contain a root go.mod")
		}
		rc, err := f.Open()
		if err != nil {
			return repo.Errorf(http.StatusBadRequest, "zip go.mod: %v", err)
		}
		p, err := modulePath(rc)
		rc.Close()
		if err != nil || p != module {
			return repo.Errorf(http.StatusBadRequest, "zip go.mod declares %q, want %q", p, module)
		}
	}
	return nil
}

// sortVersions removes duplicates and orders versions by semver precedence;
// unparsable versions go last in lexical order.
func sortVersions(versions []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range versions {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, okA := semver.Parse(out[i])
		b, okB := semver.Parse(out[j])
		switch {
		case okA && okB:
			if c := semver.Compare(a, b); c != 0 {
				return c < 0
			}
			return out[i] < out[j]
		case okA != okB:
			return okA
		}
		return out[i] < out[j]
	})
	return out
}

// pickLatest returns the highest release, else the highest prerelease, else "".
func pickLatest(versions []string) string {
	latest := ""
	for _, v := range versions {
		if _, ok := semver.Parse(v); ok {
			latest = semver.NewerRelease(latest, v)
		}
	}
	return latest
}

// maxUnzipped is go's limit on the total uncompressed size of a module zip.
const maxUnzipped = 500 << 20

// checkFilePath applies the file-name rules of golang.org/x/mod/module
// CheckFilePath that the zip check needs: non-empty slash-separated elements
// without a backslash or colon, and no Windows reserved device names.
func checkFilePath(p string) error {
	if p == "" {
		return errors.New("empty file path")
	}
	if strings.ContainsAny(p, `\:`) {
		return errors.New(`file path contains a backslash or colon`)
	}
	for _, elem := range strings.Split(p, "/") {
		if elem == "" {
			return errors.New("file path has an empty element (leading, trailing or double slash)")
		}
		short := elem
		if i := strings.Index(short, "."); i >= 0 {
			short = short[:i]
		}
		if isReservedName(short) {
			return fmt.Errorf("%q is a reserved Windows file name", elem)
		}
	}
	return nil
}

func isReservedName(s string) bool {
	switch strings.ToLower(s) {
	case "con", "prn", "aux", "nul":
		return true
	}
	if len(s) == 4 && s[3] >= '1' && s[3] <= '9' {
		switch strings.ToLower(s[:3]) {
		case "com", "lpt":
			return true
		}
	}
	return false
}
