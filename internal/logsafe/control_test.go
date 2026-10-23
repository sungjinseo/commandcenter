package logsafe

import "testing"

// QA L-7: control characters in a request path must not reach the log raw.
func TestPathEscapesControlCharacters(t *testing.T) {
	got := Path("/repository/x/a\x00b\x1b[0m\n/c")
	for _, r := range got {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("control character left in %q", got)
		}
	}
	if got != `/repository/x/a\x00b\x1b[0m\x0a/c` {
		t.Fatalf("got %q", got)
	}
	if Path("/plain/ü") != "/plain/ü" {
		t.Fatal("printable text changed")
	}
}
