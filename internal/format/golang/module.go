package golang

import (
	"archive/zip"
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

const maxDoc = 1 << 20

// goModDirectives are the go.mod directives of Go 1.26 (x/mod/modfile), each
// mapped to whether it may open a ( ) block. Raise goModDirectivesMinor with it.
var goModDirectives = map[string]bool{
	"module": true, "go": false, "toolchain": false, "godebug": true, "require": true,
	"exclude": true, "replace": true, "retract": true, "tool": true, "ignore": true,
}

const goModDirectivesMinor = 26

// goVersionRE and laxGoVersionRE are x/mod/modfile's: the go command reads a
// go directive that matches either.
var (
	goVersionRE    = regexp.MustCompile(`^([1-9][0-9]*)\.(0|[1-9][0-9]*)(\.(0|[1-9][0-9]*))?([a-z]+[0-9]+)?$`)
	laxGoVersionRE = regexp.MustCompile(`^v?(([1-9][0-9]*)\.(0|[1-9][0-9]*))([^0-9].*)$`)
)

// modulePath checks a go.mod and returns the path its module directive
// declares. Like x/mod/modfile it rejects broken tokens (only // comments,
// quoted strings that end on their line), unbalanced ( ) blocks, a missing,
// repeated or malformed module directive, a repeated or malformed go
// directive, and directives or blocks Go 1.26 does not know, unless the go
// directive names a newer Go (the go command then asks for that Go instead).
// The arguments of other directives are not checked.
func modulePath(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxDoc)
	var (
		path, blockVerb string // blockVerb is "" in a block of unknown type
		modules, n, at  int
		inBlock, goSeen bool
		newerGo         bool
		unknown         error
	)
	for sc.Scan() {
		n++
		toks, err := goModTokens(sc.Text())
		if err != nil {
			return "", fmt.Errorf("line %d: %v", n, err)
		}
		if len(toks) == 0 {
			continue
		}
		verb, args, last := toks[0], toks[1:], len(toks)-1
		if inBlock {
			if verb == ")" {
				if last > 0 {
					return "", fmt.Errorf("line %d: expected newline after closing paren", n)
				}
				inBlock = false
				continue
			}
			if blockVerb == "" {
				continue
			}
			verb, args = blockVerb, toks
		} else if verb == "(" || verb == ")" {
			return "", fmt.Errorf("line %d: unexpected %s", n, verb)
		} else if opens := toks[last] == "("; opens || last >= 2 && toks[last-1] == "(" && toks[last] == ")" {
			// "verb (" opens a block, "verb ( )" is an empty one
			head := toks[:last]
			if !opens {
				head = toks[:last-1]
			}
			blockVerb = ""
			if len(head) == 1 && goModDirectives[head[0]] {
				blockVerb = head[0]
			} else if unknown == nil {
				unknown = fmt.Errorf("line %d: unknown block type %q", n, strings.Join(head, " "))
			}
			inBlock, at = opens, n
			continue
		}
		switch verb {
		case "module":
			if modules++; modules > 1 {
				return "", fmt.Errorf("line %d: repeated module statement", n)
			}
			if len(args) != 1 {
				return "", fmt.Errorf("line %d: usage: module module/path", n)
			}
			path = args[0]
			if strings.HasPrefix(path, `"`) {
				if path, err = strconv.Unquote(path); err != nil {
					return "", fmt.Errorf("line %d: invalid quoted string: %v", n, err)
				}
			} else if strings.ContainsAny(path, "\"'`") {
				return "", fmt.Errorf("line %d: unquoted string cannot contain quote", n)
			}
		case "go":
			if goSeen {
				return "", fmt.Errorf("line %d: repeated go statement", n)
			}
			goSeen = true
			var m []string
			if len(args) == 1 {
				if m = goVersionRE.FindStringSubmatch(args[0]); m == nil {
					if m = laxGoVersionRE.FindStringSubmatch(args[0]); m != nil {
						m = m[1:]
					}
				}
			}
			if m == nil {
				return "", fmt.Errorf("line %d: invalid go version %q", n, strings.Join(args, " "))
			}
			major, _ := strconv.Atoi(m[1])
			minor, _ := strconv.Atoi(m[2])
			newerGo = major > 1 || minor > goModDirectivesMinor
		default:
			if _, ok := goModDirectives[verb]; !ok && unknown == nil {
				unknown = fmt.Errorf("line %d: unknown directive %q", n, verb)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	switch {
	case inBlock:
		return "", fmt.Errorf("line %d: unterminated block", at)
	case unknown != nil && !newerGo:
		return "", unknown
	case modules == 0:
		return "", errors.New("go.mod has no module directive")
	}
	return path, nil
}

// goModTokens splits a go.mod line into tokens like x/mod/modfile's lexer:
// spaces, tabs and carriage returns separate them, "//" starts a comment,
// each of ( ) [ ] { } , is a token, a token that starts with " or ` is a
// string that must end on the line, and any other is a run of printable,
// non-space runes.
func goModTokens(line string) ([]string, error) {
	var toks []string
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
			continue
		case strings.HasPrefix(line[i:], "//"):
			return toks, nil
		case strings.IndexByte("()[]{},", c) >= 0:
			toks = append(toks, line[i:i+1])
			i++
			continue
		case c == '"' || c == '`':
			j := i + 1
			for ; j < len(line) && line[j] != c; j++ {
				if line[j] == '\\' && c == '"' {
					j++
				}
			}
			if j >= len(line) {
				return nil, errors.New("unterminated string")
			}
			toks = append(toks, line[i:j+1])
			i = j + 1
			continue
		}
		j := i
		for j < len(line) && !strings.HasPrefix(line[j:], "//") {
			if strings.HasPrefix(line[j:], "/*") {
				return nil, errors.New("mod files must use // comments (not /* */ comments)")
			}
			r, size := utf8.DecodeRuneInString(line[j:])
			if unicode.IsSpace(r) || !unicode.IsPrint(r) || strings.ContainsRune("()[]{},", r) {
				break
			}
			j += size
		}
		if j == i {
			r, _ := utf8.DecodeRuneInString(line[i:])
			return nil, fmt.Errorf("unexpected input character %#q", r)
		}
		toks = append(toks, line[i:j])
		i = j
	}
	return toks, nil
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

// checkZip validates a module zip as the go command checks one it downloads:
// every entry lies under "{module}@{version}/" with a valid name, reads back
// with its CRC-32 and declared size, and a root go.mod declares module.
// module and version are unescaped.
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
	isDir := make(map[string]bool, len(zr.File)) // by name without a trailing slash
	folded := make(map[string]string, len(zr.File))
	var unzipped uint64 // bytes read so far
	for _, f := range zr.File {
		rel, ok := strings.CutPrefix(f.Name, prefix)
		if !ok {
			return repo.Errorf(http.StatusBadRequest, "zip entry %q is outside %s", f.Name, prefix)
		}
		// As in x/mod/zip.CheckZip, the module root's own entry has no name to
		// check, and a directory entry (zip -r writes them) is checked by its
		// name without the slash and may repeat.
		if rel != "" {
			name := strings.TrimSuffix(rel, "/")
			dir := name != rel
			if was, dup := isDir[name]; dup && !(was && dir) {
				return repo.Errorf(http.StatusBadRequest, "zip has duplicate entry %q", f.Name)
			}
			isDir[name] = dir
			lower := strings.ToLower(name)
			if other, dup := folded[lower]; dup && other != name {
				return repo.Errorf(http.StatusBadRequest, "zip entries %q and %q differ only in case", prefix+other, f.Name)
			}
			folded[lower] = name
			if err := checkFilePath(name); err != nil {
				return repo.Errorf(http.StatusBadRequest, "zip entry %q: %v", f.Name, err)
			}
			for _, seg := range strings.Split(name, "/") {
				if seg == ".." || seg == "." {
					return repo.Errorf(http.StatusBadRequest, "zip entry %q has a relative path element", f.Name)
				}
			}
		}
		// The declared sizes are what the go client sums and limits; capping
		// each term keeps the running total from wrapping.
		switch n := f.UncompressedSize64; {
		case n > maxUnzipped || unzipped+n > maxUnzipped:
			return repo.Errorf(http.StatusBadRequest, "zip uncompressed size exceeds %d bytes", int64(maxUnzipped))
		case rel == "LICENSE" && n > maxLICENSE:
			return repo.Errorf(http.StatusBadRequest, "zip LICENSE exceeds %d bytes", maxLICENSE)
		case rel == "go.mod" && n > uint64(maxModUpload):
			return repo.Errorf(http.StatusBadRequest, "zip go.mod exceeds %d bytes", maxModUpload)
		case rel == "go.mod" && strings.HasSuffix(version, "+incompatible"):
			return repo.Errorf(http.StatusBadRequest, "a +incompatible module zip must not contain a root go.mod")
		}
		// Read the entry to its end, which makes archive/zip check its CRC-32
		// and that it holds exactly the declared size. The limit and count
		// hold the total to the bytes really read, whatever the headers say.
		rc, err := f.Open()
		if err != nil {
			return repo.Errorf(http.StatusBadRequest, "zip entry %q: %v", f.Name, err)
		}
		cr := &countReader{r: io.LimitReader(rc, int64(maxUnzipped-unzipped)+1)}
		p := ""
		if rel == "go.mod" {
			p, err = modulePath(cr)
		}
		if err == nil {
			_, err = io.Copy(io.Discard, cr)
		}
		rc.Close()
		if unzipped += uint64(cr.n); unzipped > maxUnzipped {
			return repo.Errorf(http.StatusBadRequest, "zip uncompressed size exceeds %d bytes", int64(maxUnzipped))
		}
		if err != nil {
			return repo.Errorf(http.StatusBadRequest, "zip entry %q: %v", f.Name, err)
		}
		if rel == "go.mod" && p != module {
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

// maxUnzipped is go's limit on the total uncompressed size of a module zip,
// a variable so tests can lower it, and maxLICENSE its limit on a root LICENSE.
var maxUnzipped uint64 = 500 << 20

const maxLICENSE = 16 << 20

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
