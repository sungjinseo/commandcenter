package ui

import (
	"fmt"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"testing"
)

// staticNames are the files the shell cannot work without; every other
// embedded file is checked by walking the directory.
var staticNames = []string{"index.html", "app.css", "app.js", "browse.js", "dash.js", "search.js", "shell.js", "admin.js", "ops.js", "access.js", "settings.js", "repoform.js", "upload.js", "util.js", "i18n.js"}

func readStatic(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(staticFiles, "static/"+name)
	if err != nil {
		t.Fatalf("embedded %s: %v", name, err)
	}
	return string(b)
}

// staticFilesWithExt lists the embedded files ending in ext, relative to static/.
func staticFilesWithExt(t *testing.T, ext string) []string {
	t.Helper()
	var out []string
	err := fs.WalkDir(staticFiles, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(p, ext) {
			out = append(out, strings.TrimPrefix(p, "static/"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStaticFilesPresent(t *testing.T) {
	for _, n := range staticNames {
		if s := readStatic(t, n); strings.TrimSpace(s) == "" {
			t.Errorf("%s is empty", n)
		}
	}
}

// TestStaticJSListed keeps staticNames in step with the shipped modules.
func TestStaticJSListed(t *testing.T) {
	listed := map[string]bool{}
	for _, n := range staticNames {
		listed[n] = true
	}
	for _, n := range staticFilesWithExt(t, ".js") {
		if !listed[n] {
			t.Errorf("%s is embedded but not in staticNames", n)
		}
	}
}

func TestStaticSizeBudget(t *testing.T) {
	const budget = 512 << 10 // 524288 bytes (190 KiB, raised to 320 KiB in plan 10, to 400 KiB in plan 11 and to 512 KiB in plan 12, by ruling)
	total := 0
	err := fs.WalkDir(staticFiles, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += int(info.Size())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if total >= budget {
		t.Errorf("static files total %d bytes, budget is < %d", total, budget)
	}
}

// i18nKeys parses one dictionary of i18n.js ("const <name> = {" up to
// "};"), which holds exactly one `"key": "value",` pair per line.
func i18nKeys(t *testing.T, js, name string) map[string]bool {
	t.Helper()
	m := regexp.MustCompile(`(?s)\nconst ` + name + ` = \{\n(.*?)\n\};\n`).FindStringSubmatch(js)
	if m == nil {
		t.Fatalf("i18n.js: no `const %s = {` ... `};` block", name)
	}
	pair := regexp.MustCompile(`^  "([a-zA-Z0-9_.]+)": "(?:[^"\\]|\\.)*",$`)
	keys := map[string]bool{}
	for _, line := range strings.Split(m[1], "\n") {
		p := pair.FindStringSubmatch(line)
		if p == nil {
			t.Errorf("i18n.js %s: line is not one \"key\": \"value\", pair: %q", name, line)
			continue
		}
		if keys[p[1]] {
			t.Errorf("i18n.js %s: duplicate key %q", name, p[1])
		}
		keys[p[1]] = true
	}
	if len(keys) == 0 {
		t.Fatalf("i18n.js %s: no keys", name)
	}
	return keys
}

// TestI18nKeySetsEqual: the ko and en dictionaries hold the same keys.
func TestI18nKeySetsEqual(t *testing.T) {
	js := readStatic(t, "i18n.js")
	ko, en := i18nKeys(t, js, "ko"), i18nKeys(t, js, "en")
	for k := range ko {
		if !en[k] {
			t.Errorf("key %q is in ko but not in en", k)
		}
	}
	for k := range en {
		if !ko[k] {
			t.Errorf("key %q is in en but not in ko", k)
		}
	}
}

// TestI18nKeysUsedExist: every literal key passed to t("…") or t('…') in
// any module exists in the dictionaries.
func TestI18nKeysUsedExist(t *testing.T) {
	keys := i18nKeys(t, readStatic(t, "i18n.js"), "ko")
	use := regexp.MustCompile(`\bt\(\s*(?:"([^"]*)"|'([^']*)')`)
	n := 0
	for _, name := range staticFilesWithExt(t, ".js") {
		for _, m := range use.FindAllStringSubmatch(readStatic(t, name), -1) {
			k := m[1] + m[2]
			n++
			if !keys[k] {
				t.Errorf("%s uses t(%q), which is not in the dictionaries", name, k)
			}
		}
	}
	if n < 30 {
		t.Errorf("only %d t() keys found; the shell and router are expected to use t()", n)
	}
}

// TestI18nDefaultLanguage: without a stored choice, an English browser gets
// English and every other browser Korean (plan 10 Decision 5).
func TestI18nDefaultLanguage(t *testing.T) {
	js := readStatic(t, "i18n.js")
	if !strings.Contains(js, `nav.startsWith("en") ? "en" : "ko"`) {
		t.Error(`i18n.js must fall back with nav.startsWith("en") ? "en" : "ko"`)
	}
	if strings.Contains(js, `startsWith("ko") ? "ko" : "en"`) {
		t.Error("i18n.js still falls back to English")
	}
}

func TestIndexHTMLNoInline(t *testing.T) {
	html := readStatic(t, "index.html")
	// A <script> element must be a same-origin module with an empty body.
	scripts := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`).FindAllStringSubmatch(html, -1)
	if len(scripts) == 0 {
		t.Error("index.html has no <script>")
	}
	for _, m := range scripts {
		src := regexp.MustCompile(`\bsrc="([^"]*)"`).FindStringSubmatch(m[1])
		if src == nil {
			t.Errorf("inline <script> without src: %q", m[0])
		} else if !strings.HasPrefix(src[1], "/ui/") || strings.Contains(src[1], "//") || strings.Contains(src[1], "..") {
			t.Errorf("<script> src %q is not a same-origin /ui/ file", src[1])
		}
		if !strings.Contains(m[1], `type="module"`) {
			t.Errorf("<script> is not a module: %q", m[0])
		}
		if strings.TrimSpace(m[2]) != "" {
			t.Errorf("<script> has an inline body: %q", m[0])
		}
	}
	if regexp.MustCompile(`(?i)<style\b`).MatchString(html) {
		t.Error("index.html has a <style> element")
	}
	if regexp.MustCompile(`(?i)\sstyle\s*=`).MatchString(html) {
		t.Error("index.html has a style= attribute")
	}
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(html) {
		t.Error("index.html has an inline event handler")
	}
	for _, ref := range []string{`src="/ui/app.js"`, `href="/ui/app.css"`} {
		if !strings.Contains(html, ref) {
			t.Errorf("index.html does not reference %s", ref)
		}
	}
	if regexp.MustCompile(`(?i)(src|href)\s*=\s*"(https?:)?//`).MatchString(html) {
		t.Error("index.html references an external URL")
	}
}

// TestJSImportsSameOrigin: modules import only sibling embedded files.
func TestJSImportsSameOrigin(t *testing.T) {
	// "import … from 'x'", "export … from 'x'" and a side-effect "import 'x'",
	// wherever they appear on a line; \x60 is a backtick.
	staticRe := regexp.MustCompile(`\b(?:from|import)\s*["'\x60]([^"'\x60]*)["'\x60]`)
	// A dynamic import() must name a single string literal.
	dynAny := regexp.MustCompile(`\bimport\s*\(`)
	dynLit := regexp.MustCompile(`\bimport\s*\(\s*["']([^"']*)["']\s*\)`)
	for _, name := range staticFilesWithExt(t, ".js") {
		js := readStatic(t, name)
		if a, l := len(dynAny.FindAllString(js, -1)), len(dynLit.FindAllString(js, -1)); a != l {
			t.Errorf("%s: %d dynamic import() calls, only %d with a string literal", name, a, l)
		}
		var specs []string
		for _, m := range staticRe.FindAllStringSubmatch(js, -1) {
			specs = append(specs, m[1])
		}
		for _, m := range dynLit.FindAllStringSubmatch(js, -1) {
			specs = append(specs, m[1])
		}
		for _, spec := range specs {
			if !regexp.MustCompile(`^\./[a-z0-9-]+\.js$`).MatchString(spec) {
				t.Errorf("%s imports %q: only ./name.js is allowed", name, spec)
				continue
			}
			if _, err := fs.Stat(staticFiles, path.Join("static", path.Dir(name), spec)); err != nil {
				t.Errorf("%s imports %q, which is not embedded", name, spec)
			}
		}
	}
}

func TestJSNoInnerHTMLFromData(t *testing.T) {
	files := staticFilesWithExt(t, ".js")
	if len(files) < 2 {
		t.Fatalf("expected several .js modules, got %v", files)
	}
	for _, name := range files {
		js := readStatic(t, name)
		// Allow clearing with a literal; any other assignment may carry data.
		if m := regexp.MustCompile("innerHTML\\s*=\\s*[^\"'`]").FindString(js); m != "" {
			t.Errorf("%s assigns innerHTML from a non-literal: %q", name, m)
		}
		// An on* key or attribute with a string value is an inline handler,
		// whether passed to h() ({onclick: "…"}) or to setAttribute.
		if m := regexp.MustCompile(`(?i)["']?\bon[a-z]+["']?\s*:\s*["'\x60]|setAttribute\(\s*["'\x60]on`).FindString(js); m != "" {
			t.Errorf("%s sets an on* attribute from a string: %q", name, m)
		}
		for _, bad := range []string{"outerHTML", "insertAdjacentHTML", "document.write", "eval(", "innerHTML +=", "new Function", "DOMParser", "createContextualFragment"} {
			// upload.js may parse a .pom as XML for reading (plan 9 Decision 8); see TestUploadXMLReadOnly.
			if bad == "DOMParser" && name == "upload.js" {
				continue
			}
			if strings.Contains(js, bad) {
				t.Errorf("%s uses %s", name, bad)
			}
		}
		// A string as the first argument makes a timer evaluate code.
		if m := regexp.MustCompile(`\b(setTimeout|setInterval)\s*\(\s*["'` + "`" + `]`).FindString(js); m != "" {
			t.Errorf("%s passes a string to a timer: %q", name, m)
		}
	}
}

func TestAppCSSNoExternalURL(t *testing.T) {
	for _, name := range staticFilesWithExt(t, ".css") {
		css := readStatic(t, name)
		if regexp.MustCompile(`(?i)url\(\s*['"]?(https?:)?//|@import`).MatchString(css) {
			t.Errorf("%s references an external resource", name)
		}
	}
}

// TestUploadXMLReadOnly: upload.js parses a .pom only as XML, with one
// DOMParser use, and never moves parsed nodes into the page.
func TestUploadXMLReadOnly(t *testing.T) {
	js := readStatic(t, "upload.js")
	if n := strings.Count(js, "DOMParser"); n != 1 {
		t.Errorf("upload.js uses DOMParser %d times, want exactly 1", n)
	}
	if !regexp.MustCompile(`new DOMParser\(\)\.parseFromString\([a-zA-Z_$][\w$]*, "application/xml"\)`).MatchString(js) {
		t.Error(`upload.js must call new DOMParser().parseFromString(text, "application/xml")`)
	}
	for _, bad := range []string{"text/html", "importNode", "adoptNode"} {
		if strings.Contains(js, bad) {
			t.Errorf("upload.js uses %s", bad)
		}
	}
}

// TestUploadWired: the sidebar item, the route and the Browse toolbar button.
func TestUploadWired(t *testing.T) {
	for file, want := range map[string]string{
		"shell.js":  `"#browse/upload"`,
		"app.js":    `"browse/upload"`,
		"browse.js": `t("tree.upload")`,
	} {
		if !strings.Contains(readStatic(t, file), want) {
			t.Errorf("%s does not contain %s", file, want)
		}
	}
}

// TestOpsWired: the plan 11 operations screens are reachable and use the
// operations APIs (Decision 8).
func TestOpsWired(t *testing.T) {
	for file, wants := range map[string][]string{
		"shell.js":    {`"#admin/repository/cleanuppolicies"`, `t("nav.cleanup")`},
		"app.js":      {`"admin/repository/cleanuppolicies": true`},
		"admin.js":    {`viewPolicies(view, p0)`, `logsCard(isLive)`, `trendCard(isLive, "24h"`},
		"dash.js":     {`trendCard(isLive, "7d"`, `api("events?limit=6")`},
		"ops.js":      {`"cleanup/policies"`, `"/preview"`, `"/run"`, `"/-/api/logs/download"`, `api("stats/requests?range="`},
		"repoform.js": {`cleanupPolicy: policy ? policy.value : storedPolicy`},
		"browse.js":   {`t("comp.downloads")`, `t("comp.lastDownloaded")`},
	} {
		js := readStatic(t, file)
		for _, want := range wants {
			if !strings.Contains(js, want) {
				t.Errorf("%s does not contain %s", file, want)
			}
		}
	}
}

func TestStaticETagAndNoCache(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	for _, p := range []string{"/ui/", "/ui/app.js", "/ui/app.css", "/ui/some/typo"} {
		r := e.get(t, p, "")
		wantStatus(t, r, http.StatusOK)
		if cc := r.header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control = %q", p, cc)
		}
		etag := r.header.Get("ETag")
		if len(etag) < 10 || etag[0] != '"' {
			t.Fatalf("%s: ETag = %q", p, etag)
		}
		req, _ := http.NewRequest("GET", e.ts.URL+p, nil)
		req.Header.Set("If-None-Match", etag)
		res, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusNotModified || len(body) != 0 {
			t.Errorf("%s: If-None-Match gave %d with %d body bytes", p, res.StatusCode, len(body))
		}
		if res.Header.Get("ETag") != etag || res.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: 304 headers %v", p, res.Header)
		}
	}
	js := readStatic(t, "app.js")
	sum := sha256.Sum256([]byte(js))
	if got := e.get(t, "/ui/app.js", "").header.Get("ETag"); got != `"`+hex.EncodeToString(sum[:])+`"` {
		t.Errorf("ETag %s is not the sha256 of the embedded file", got)
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/ui/app.js", nil)
	req.Header.Set("If-None-Match", `"stale"`)
	if res, err := noRedirect.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatalf("a stale ETag must get the file: %v %v", res, err)
	}
}

// TestSettingsMessagesPinned ties the SMTP server text settings.js matches to
// its regex literal.
func TestSettingsMessagesPinned(t *testing.T) {
	js := readStatic(t, "settings.js")
	const re = "re-enter the password"
	if !strings.Contains(js, "const REENTER = /"+re+"/;") {
		t.Errorf("settings.js does not define REENTER as /%s/", re)
	}
	if !regexp.MustCompile(re).MatchString(smtpReenterMessage) {
		t.Errorf("server text %q no longer matches /%s/ used by settings.js", smtpReenterMessage, re)
	}
}

// TestAccessMessagesPinned ties the server texts that access.js matches to
// the regex literals in the script: rewording either side fails the test.
func TestAccessMessagesPinned(t *testing.T) {
	js := readStatic(t, "access.js")
	for _, c := range []struct{ name, msg, re string }{
		{"LOCKOUT", lockoutMessage, "no admin who can log in"},
		{"IN_USE", roleInUseMessage, "in use"},
		{"WRONG_PW", wrongPasswordMessage, "the current password is incorrect"},
		{"REENTER", bindReenterMessage, "re-enter the bind password"},
	} {
		if !strings.Contains(js, "const "+c.name+" = /"+c.re+"/;") {
			t.Errorf("access.js does not define %s as /%s/", c.name, c.re)
		}
		if !regexp.MustCompile(c.re).MatchString(c.msg) {
			t.Errorf("server text %q no longer matches /%s/ used by access.js", c.msg, c.re)
		}
	}
}

// TestI18nServerDefault (final review I2): the server's defaultLanguage is
// the fallback when the browser has no stored choice. applyServerDefault
// returns early on a stored ko/en choice (so it still wins), accepts only
// ko/en, never stores the server value, and the shell calls it with the
// /-/api/me answer.
func TestI18nServerDefault(t *testing.T) {
	js := readStatic(t, "i18n.js")
	ms := regexp.MustCompile(`(?s)export function applyServerDefault\(l\) \{\n(.*?)\n\}\n`).FindAllStringSubmatch(js, -1)
	if len(ms) != 1 {
		t.Fatalf("i18n.js must export function applyServerDefault(l) exactly once, found %d", len(ms))
	}
	body := ms[0][1]
	stored := strings.Index(body, `if (storedLang()) return false;`)
	sw := strings.Index(body, "lang = l;")
	if stored < 0 || sw < 0 || stored > sw {
		t.Errorf("applyServerDefault must return on a stored choice before switching:\n%s", body)
	}
	if !strings.Contains(body, `if ((l !== "ko" && l !== "en") || l === lang) return false;`) {
		t.Errorf("applyServerDefault must accept only ko/en:\n%s", body)
	}
	if strings.Contains(body, "setItem") {
		t.Errorf("applyServerDefault must not store the server default:\n%s", body)
	}
	if !strings.Contains(body, "for (const fn of listeners) fn(l);") {
		t.Errorf("applyServerDefault must tell the listeners (the shell re-renders):\n%s", body)
	}
	// initial() and applyServerDefault share the stored-choice test.
	if !regexp.MustCompile(`(?s)function initial\(\) \{\n  const v = storedLang\(\);\n  if \(v\) return v;`).MatchString(js) {
		t.Error("initial() must prefer the stored choice through storedLang()")
	}
	shell := readStatic(t, "shell.js")
	if !strings.Contains(shell, "applyServerDefault(state.me.defaultLanguage)") {
		t.Error("shell.js must apply the server default from /-/api/me")
	}
}

// TestI18nHeaderAndSingleDecls: commit 160994d once prepended a duplicate
// storedLang() and an unclosed initial() before the header comment, which
// broke the whole SPA. Pin the start of the file and single declarations.
func TestI18nHeaderAndSingleDecls(t *testing.T) {
	js := readStatic(t, "i18n.js")
	if !strings.HasPrefix(js, "// Korean/English UI strings") {
		t.Errorf("i18n.js must start with its header comment, starts with %q", js[:min(60, len(js))])
	}
	for _, f := range []string{"function storedLang(", "function initial(", "function applyServerDefault("} {
		if n := strings.Count(js, f); n != 1 {
			t.Errorf("%s declared %d times in i18n.js, want 1", f, n)
		}
	}
}

func isJSIdent(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// jsBalance reports the first bracket problem in JavaScript source, ignoring
// brackets inside strings, template literals, regex literals and comments.
// It is a cheap structural check, not a parser.
func jsBalance(src string) error {
	var stack []byte
	var tmpl []int // bracket depth at each open template substitution
	line := 1
	regexOK := true // a "/" starts a regex when the last token is not a value
	n := len(src)
	// scanTemplate reads template text from i (just after ` or }) up to the
	// closing backtick or the next ${.
	scanTemplate := func(i int) (int, error) {
		for ; i < n; i++ {
			switch {
			case src[i] == 92:
				i++
			case src[i] == '\n':
				line++
			case src[i] == '`':
				return i, nil
			case src[i] == '$' && i+1 < n && src[i+1] == '{':
				tmpl = append(tmpl, len(stack))
				return i + 1, nil
			}
		}
		return i, fmt.Errorf("line %d: unterminated template literal", line)
	}
	for i := 0; i < n; i++ {
		c := src[i]
		switch {
		case c == '\n':
			line++
		case c == ' ' || c == '\t' || c == '\r':
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
			i--
		case c == '/' && i+1 < n && src[i+1] == '*':
			for i += 2; i+1 < n && !(src[i] == '*' && src[i+1] == '/'); i++ {
				if src[i] == '\n' {
					line++
				}
			}
			i++
		case c == '"' || c == '\'':
			for i++; i < n && src[i] != c; i++ {
				if src[i] == 92 { // backslash
					i++
				} else if src[i] == '\n' {
					return fmt.Errorf("line %d: unterminated string", line)
				}
			}
			regexOK = false
		case c == '`' || (c == '}' && len(tmpl) > 0 && tmpl[len(tmpl)-1] == len(stack)):
			if c == '}' {
				tmpl = tmpl[:len(tmpl)-1]
			}
			var err error
			if i, err = scanTemplate(i + 1); err != nil {
				return err
			}
			regexOK = false
		case c == '/' && regexOK:
			inClass := false
			for i++; i < n && (src[i] != '/' || inClass); i++ {
				switch src[i] {
				case 92: // backslash
					i++
				case '[':
					inClass = true
				case ']':
					inClass = false
				case '\n':
					return fmt.Errorf("line %d: unterminated regex", line)
				}
			}
			regexOK = false
		case c == '{' || c == '(' || c == '[':
			stack = append(stack, c)
			regexOK = true
		case c == '}' || c == ')' || c == ']':
			want := map[byte]byte{'}': '{', ')': '(', ']': '['}[c]
			if len(stack) == 0 || stack[len(stack)-1] != want {
				return fmt.Errorf("line %d: unexpected %q", line, c)
			}
			stack = stack[:len(stack)-1]
			regexOK = false
		case isJSIdent(c):
			j := i
			for j < n && isJSIdent(src[j]) {
				j++
			}
			switch src[i:j] {
			case "return", "typeof", "case", "of", "in", "delete", "void", "throw", "new", "else", "do", "yield", "await":
				regexOK = true
			default:
				regexOK = false
			}
			i = j - 1
		default: // operators and punctuation: a value cannot just have ended
			regexOK = true
		}
	}
	if len(stack) != 0 {
		return fmt.Errorf("%d unclosed brackets at end of file (innermost %q)", len(stack), stack[len(stack)-1])
	}
	return nil
}

// TestStaticJSBalanced is the Node-free parse gate: every embedded .js file
// must have balanced {}, () and [] outside strings, templates, regexes and
// comments. test/smoke/ui-smoke.sh runs the real node --check.
func TestStaticJSBalanced(t *testing.T) {
	for _, n := range staticFilesWithExt(t, ".js") {
		if err := jsBalance(readStatic(t, n)); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	if err := jsBalance("function f() {\n  const v = 1;\n"); err == nil {
		t.Error("an unclosed brace must be reported")
	}
	if err := jsBalance("const a = \"}\"; const b = /[}]/; const c = `x${ {a:1}.a }}`; // {\n"); err != nil {
		t.Errorf("brackets in strings, regexes, templates and comments must be ignored: %v", err)
	}
}
