package repo

import (
	"io"
	"strings"
	"testing"
)

// CheckContent holds a path to its extension's signature and replays the
// body; an archive without entries and XML behind a byte order mark pass.
func TestCheckContent(t *testing.T) {
	for _, c := range []struct {
		path, declared, body string
		ok                   bool
	}{
		{"a/lib-1.0.jar", "", "PK\x03\x04rest", true},
		{"a/empty.zip", "", "PK\x05\x06" + strings.Repeat("\x00", 18), true},
		{"a/lib-1.0.jar", "", "<html>sign in</html>", false},
		{"a/lib-1.0.tgz", "", "PK\x05\x06", false},
		{"a/maven-metadata.xml", "", "\xef\xbb\xbf\n<metadata/>", true},
		{"a/maven-metadata.xml", "", "not found", false},
		{"a/notes.txt", "", "PK\x03\x04", true}, // no signature for the extension, nothing declared
		{"a/notes", "text/plain", "\x00\x01\x02binary", false},
	} {
		r, err := CheckContent(c.path, c.declared, strings.NewReader(c.body))
		if (err == nil) != c.ok {
			t.Errorf("%s %q: %v", c.path, c.body, err)
			continue
		}
		if err != nil {
			if StatusOf(err) != 400 {
				t.Errorf("%s: status %d", c.path, StatusOf(err))
			}
			continue
		}
		if b, _ := io.ReadAll(r); string(b) != c.body {
			t.Errorf("%s: replayed %q", c.path, b)
		}
	}
}
