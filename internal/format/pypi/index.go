package pypi

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	jsonType = "application/vnd.pypi.simple.v1+json"
	htmlType = "text/html; charset=utf-8"
)

var errIndex = errors.New("pypi: malformed simple index page")

// File is one distribution file of a project.
type File struct {
	Filename       string
	URL            string // upstream location (proxies only)
	SHA256         string
	RequiresPython string
	Yanked         bool
	YankedReason   string
	HasMetadata    bool   // PEP 658: <URL>.metadata holds the core metadata
	MetadataSHA256 string // its sha256 when advertised
}

// parseIndex streams the files of an upstream project page: PEP 691 JSON when
// contentType says so, PEP 503 HTML otherwise. Relative URLs resolve against base.
func parseIndex(r io.Reader, contentType string, base *url.URL, fn func(File) error) error {
	if strings.Contains(contentType, "json") {
		return parseJSONIndex(r, base, fn)
	}
	return parseHTMLIndex(r, base, fn)
}

func parseJSONIndex(r io.Reader, base *url.URL, fn func(File) error) error {
	dec := json.NewDecoder(r)
	if tok, err := dec.Token(); err != nil {
		return err
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errIndex
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		if key, _ := kt.(string); key != "files" {
			if err := skipValue(dec); err != nil {
				return err
			}
			continue
		}
		if tok, err := dec.Token(); err != nil {
			return err
		} else if d, ok := tok.(json.Delim); !ok || d != '[' {
			return errIndex
		}
		for dec.More() {
			var jf struct {
				Filename       string            `json:"filename"`
				URL            string            `json:"url"`
				Hashes         map[string]string `json:"hashes"`
				RequiresPython *string           `json:"requires-python"`
				Yanked         json.RawMessage   `json:"yanked"`
				CoreMetadata   json.RawMessage   `json:"core-metadata"`
				DistInfoMeta   json.RawMessage   `json:"dist-info-metadata"`
			}
			if err := dec.Decode(&jf); err != nil {
				return err
			}
			if !validFile(jf.Filename) {
				continue
			}
			u := resolveURL(base, jf.URL)
			if u == "" {
				continue
			}
			f := File{Filename: jf.Filename, URL: u, SHA256: cleanSHA256(jf.Hashes["sha256"])}
			if jf.RequiresPython != nil {
				f.RequiresPython = *jf.RequiresPython
			}
			f.Yanked, f.YankedReason = parseYanked(jf.Yanked)
			if jf.CoreMetadata == nil {
				jf.CoreMetadata = jf.DistInfoMeta // the key before PEP 714
			}
			f.HasMetadata, f.MetadataSHA256 = parseJSONMetadata(jf.CoreMetadata)
			if err := fn(f); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // ']'
			return err
		}
	}
	return nil
}

// skipValue consumes one JSON value without materializing it. Nested
// containers are walked token by token, so memory stays flat.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	depth := 1
	if d != '{' && d != '[' {
		return errIndex
	}
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

// parseJSONMetadata reads a PEP 691 "core-metadata" value: true, false, or
// the hashes of the metadata file.
func parseJSONMetadata(raw json.RawMessage) (bool, string) {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b, ""
	}
	var hashes map[string]string
	if json.Unmarshal(raw, &hashes) == nil && len(hashes) > 0 {
		return true, cleanSHA256(hashes["sha256"])
	}
	return false, ""
}

// parseHTMLMetadata reads a PEP 658 data-core-metadata attribute: "true" or
// "<hash name>=<hash>".
func parseHTMLMetadata(v string, ok bool) (bool, string) {
	switch {
	case !ok || strings.EqualFold(v, "false"):
		return false, ""
	case strings.HasPrefix(strings.ToLower(v), "sha256="):
		return true, cleanSHA256(v[len("sha256="):])
	}
	return true, ""
}

func parseYanked(raw json.RawMessage) (bool, string) {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b, ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return true, s
	}
	return false, ""
}

// parseHTMLIndex streams the anchors of a PEP 503 page. It understands the
// generated markup package indexes produce, not arbitrary HTML.
func parseHTMLIndex(r io.Reader, base *url.URL, fn func(File) error) error {
	br := bufio.NewReaderSize(r, 32*1024)
	for {
		if err := skipUntil(br, '<'); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		tag, err := readTag(br)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		name, attrs := tag, ""
		if i := strings.IndexAny(tag, " \t\r\n"); i >= 0 {
			name, attrs = tag[:i], tag[i:]
		}
		if !strings.EqualFold(name, "a") {
			continue
		}
		text, err := readUntil(br, '<', 64<<10)
		if err != nil {
			return errIndex
		}
		a := parseAttrs(attrs)
		href, ok := a["href"]
		if !ok {
			continue
		}
		f := File{Filename: strings.TrimSpace(html.UnescapeString(text))}
		if !validFile(f.Filename) {
			continue
		}
		if u, err := url.Parse(href); err == nil {
			if v, ok := strings.CutPrefix(u.Fragment, "sha256="); ok {
				f.SHA256 = cleanSHA256(v)
			}
		}
		f.URL = resolveURL(base, href)
		if f.URL == "" {
			continue
		}
		f.RequiresPython = a["data-requires-python"]
		if reason, ok := a["data-yanked"]; ok {
			f.Yanked, f.YankedReason = true, reason
		}
		md, ok := a["data-core-metadata"]
		if !ok {
			md, ok = a["data-dist-info-metadata"] // the attribute before PEP 714
		}
		f.HasMetadata, f.MetadataSHA256 = parseHTMLMetadata(md, ok)
		if err := fn(f); err != nil {
			return err
		}
	}
}

func skipUntil(br *bufio.Reader, delim byte) error {
	for {
		c, err := br.ReadByte()
		if err != nil {
			return err
		}
		if c == delim {
			return nil
		}
	}
}

// readTag returns the bytes of a start tag up to its closing '>' (consumed).
// A '>' inside a single- or double-quoted attribute value does not end the
// tag. Comments ("!--") are skipped up to "-->"; other declarations and
// processing instructions ("!", "?") end at the first '>' with no quote
// tracking. More than 64 KB before the end is an error.
func readTag(br *bufio.Reader) (string, error) {
	const limit = 64 << 10
	if p, _ := br.Peek(3); string(p) == "!--" {
		br.Discard(3)
		var last3 [3]byte
		for {
			c, err := br.ReadByte()
			if err != nil {
				return "", err
			}
			last3 = [3]byte{last3[1], last3[2], c}
			if string(last3[:]) == "-->" {
				return "!--", nil
			}
		}
	}
	plain := false
	if p, _ := br.Peek(1); len(p) == 1 && (p[0] == '!' || p[0] == '?') {
		plain = true
	}
	var buf []byte
	var quote byte
	for {
		c, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		if quote == 0 && c == '>' {
			return string(buf), nil
		}
		if plain {
			// no quote tracking
		} else if quote != 0 {
			if c == quote {
				quote = 0
			}
		} else if c == '"' || c == '\'' {
			quote = c
		}
		if len(buf) >= limit {
			return "", errIndex
		}
		buf = append(buf, c)
	}
}

// readUntil returns the bytes before delim (consuming delim); more than limit
// bytes before it is an error.
func readUntil(br *bufio.Reader, delim byte, limit int) (string, error) {
	var buf []byte
	for {
		c, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		if c == delim {
			return string(buf), nil
		}
		if len(buf) >= limit {
			return "", errIndex
		}
		buf = append(buf, c)
	}
}

// parseAttrs parses the attribute part of a start tag; names are lower-cased
// and values unescaped.
func parseAttrs(s string) map[string]string {
	out := map[string]string{}
	for {
		s = strings.TrimLeft(s, " \t\r\n/")
		if s == "" {
			return out
		}
		i := strings.IndexAny(s, "= \t\r\n")
		if i < 0 {
			out[strings.ToLower(s)] = ""
			return out
		}
		name := strings.ToLower(s[:i])
		s = strings.TrimLeft(s[i:], " \t\r\n")
		if !strings.HasPrefix(s, "=") {
			out[name] = ""
			continue
		}
		s = strings.TrimLeft(s[1:], " \t\r\n")
		var val string
		if s != "" && (s[0] == '"' || s[0] == '\'') {
			q := s[0]
			if end := strings.IndexByte(s[1:], q); end >= 0 {
				val, s = s[1:1+end], s[2+end:]
			} else {
				val, s = s[1:], ""
			}
		} else {
			end := strings.IndexAny(s, " \t\r\n")
			if end < 0 {
				end = len(s)
			}
			val, s = s[:end], s[end:]
		}
		out[name] = html.UnescapeString(val)
	}
}

// resolveURL resolves ref against base and returns it without fragment. It
// returns "" for an empty reference, an unparsable one, one that resolves to
// the page itself, or one that is not http or https.
func resolveURL(base *url.URL, ref string) string {
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	u = base.ResolveReference(u)
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	u.Fragment = ""
	page := *base
	page.Fragment = ""
	if u.String() == page.String() {
		return ""
	}
	return u.String()
}

// wantsJSON reports whether the client prefers PEP 691 JSON over HTML.
func wantsJSON(accept string) bool {
	jq, hq := -1.0, -1.0
	for _, part := range strings.Split(accept, ",") {
		mt, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			if v, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(p)), "q="); ok {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					q = f
				}
			}
		}
		switch strings.ToLower(strings.TrimSpace(mt)) {
		case "application/vnd.pypi.simple.v1+json", "application/vnd.pypi.simple.latest+json":
			jq = max(jq, q)
		case "application/vnd.pypi.simple.v1+html", "application/vnd.pypi.simple.latest+html", "text/html", "*/*":
			hq = max(hq, q)
		}
	}
	return jq > 0 && jq >= hq
}

// renderer streams a project page. Nothing is sent until the first file, so a
// project without files can still be answered with 404.
type renderer struct {
	w       http.ResponseWriter
	bw      *bufio.Writer
	asJSON  bool
	project string
	n       int
}

func newRenderer(w http.ResponseWriter, asJSON bool, project string) *renderer {
	return &renderer{w: w, asJSON: asJSON, project: project}
}

func (r *renderer) Started() bool { return r.n > 0 }

func (r *renderer) begin() {
	ct := htmlType
	if r.asJSON {
		ct = jsonType
	}
	r.w.Header().Set("Content-Type", ct)
	r.w.Header().Set("Vary", "Accept")
	r.w.WriteHeader(http.StatusOK)
	r.bw = bufio.NewWriterSize(r.w, 32*1024)
	if r.asJSON {
		name, _ := json.Marshal(r.project)
		fmt.Fprintf(r.bw, `{"meta":{"api-version":"1.0"},"name":%s,"files":[`, name)
		return
	}
	p := html.EscapeString(r.project)
	fmt.Fprintf(r.bw, "<!DOCTYPE html>\n<html><head><meta name=\"pypi:repository-version\" content=\"1.0\"><title>Links for %s</title></head><body><h1>Links for %s</h1>\n", p, p)
}

// File writes one entry; href is the download URL without a fragment.
func (r *renderer) File(f File, href string) error {
	if r.n == 0 {
		r.begin()
	}
	if r.asJSON {
		if r.n > 0 {
			r.bw.WriteString(",")
		}
		obj := map[string]any{"filename": f.Filename, "url": href, "hashes": map[string]string{}}
		if f.SHA256 != "" {
			obj["hashes"] = map[string]string{"sha256": f.SHA256}
		}
		if f.RequiresPython != "" {
			obj["requires-python"] = f.RequiresPython
		}
		if f.Yanked {
			if f.YankedReason != "" {
				obj["yanked"] = f.YankedReason
			} else {
				obj["yanked"] = true
			}
		}
		if f.HasMetadata { // both keys: PEP 714
			var md any = true
			if f.MetadataSHA256 != "" {
				md = map[string]string{"sha256": f.MetadataSHA256}
			}
			obj["core-metadata"], obj["dist-info-metadata"] = md, md
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		r.bw.Write(b)
	} else {
		link := href
		if f.SHA256 != "" {
			link += "#sha256=" + f.SHA256
		}
		fmt.Fprintf(r.bw, `<a href="%s"`, html.EscapeString(link))
		if f.HasMetadata { // both attributes: PEP 714
			md := "true"
			if f.MetadataSHA256 != "" {
				md = "sha256=" + f.MetadataSHA256
			}
			fmt.Fprintf(r.bw, ` data-core-metadata="%s" data-dist-info-metadata="%s"`, md, md)
		}
		if f.RequiresPython != "" {
			fmt.Fprintf(r.bw, ` data-requires-python="%s"`, html.EscapeString(f.RequiresPython))
		}
		if f.Yanked {
			fmt.Fprintf(r.bw, ` data-yanked="%s"`, html.EscapeString(f.YankedReason))
		}
		fmt.Fprintf(r.bw, ">%s</a><br/>\n", html.EscapeString(f.Filename))
	}
	r.n++
	return nil
}

// Close finishes the page. sent is false when no file was written; then
// nothing has been sent and the caller can answer 404.
func (r *renderer) Close() (sent bool, err error) {
	if r.n == 0 {
		return false, nil
	}
	if r.asJSON {
		r.bw.WriteString("]}")
	} else {
		r.bw.WriteString("</body></html>\n")
	}
	return true, r.bw.Flush()
}

// writeProjects writes the /simple/ project list.
func writeProjects(w http.ResponseWriter, asJSON bool, names []string) error {
	bw := bufio.NewWriter(w)
	w.Header().Set("Vary", "Accept")
	if asJSON {
		w.Header().Set("Content-Type", jsonType)
		bw.WriteString(`{"meta":{"api-version":"1.0"},"projects":[`)
		for i, n := range names {
			if i > 0 {
				bw.WriteString(",")
			}
			b, _ := json.Marshal(n)
			fmt.Fprintf(bw, `{"name":%s}`, b)
		}
		bw.WriteString("]}")
		return bw.Flush()
	}
	w.Header().Set("Content-Type", htmlType)
	bw.WriteString("<!DOCTYPE html>\n<html><head><meta name=\"pypi:repository-version\" content=\"1.0\"><title>Simple index</title></head><body>\n")
	for _, n := range names {
		e := html.EscapeString(n)
		fmt.Fprintf(bw, "<a href=\"%s/\">%s</a><br/>\n", e, e)
	}
	bw.WriteString("</body></html>\n")
	return bw.Flush()
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// cleanSHA256 lowercases s and returns it only if it is a well-formed sha256 digest.
func cleanSHA256(s string) string {
	s = strings.ToLower(s)
	if !sha256Hex.MatchString(s) {
		return ""
	}
	return s
}
