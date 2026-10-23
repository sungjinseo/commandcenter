package logsafe

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestURLs(t *testing.T) {
	got := URLs(`fetch https://user:pw@host/p?token=x&a=b#frag failed; see http://h/ok.`)
	for _, bad := range []string{"user", "pw", "token=x", "a=b", "frag"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q leaked in %q", bad, got)
		}
	}
	if !strings.Contains(got, "https://host/p?token=REDACTED&a=REDACTED") || !strings.HasSuffix(got, "http://h/ok.") {
		t.Fatalf("got %q", got)
	}
}

func TestErrUnwrapsURLError(t *testing.T) {
	_, err := http.Get("http://user:pw@127.0.0.1:1/p?token=x")
	wrapped := fmt.Errorf("member: %w", err)
	got := Err(wrapped)
	for _, bad := range []string{"user", "pw", "token=x"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q leaked in %q", bad, got)
		}
	}
	if !strings.Contains(got, "http://127.0.0.1:1/p?token=REDACTED") || !strings.Contains(got, "connect") {
		t.Fatalf("got %q", got)
	}
	ue := &url.Error{Op: "Get", URL: "https://a:b@h/x?k=v", Err: errors.New("boom")}
	if got := Err(ue); got != `Get "https://h/x?k=REDACTED": boom` {
		t.Fatalf("got %q", got)
	}
}

func TestPath(t *testing.T) {
	if got := Path("/repository/n/-/user/token/SECRET"); strings.Contains(got, "SECRET") {
		t.Fatal(got)
	}
	if Path("/a/b") != "/a/b" {
		t.Fatal("changed")
	}
}
