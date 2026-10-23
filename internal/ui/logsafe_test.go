package ui

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A failed API call logs its error with the remote URL's credentials and
// query values redacted.
func TestFailRedactsErrorURLs(t *testing.T) {
	var buf bytes.Buffer
	u := &UI{Log: slog.New(slog.NewTextHandler(&buf, nil))}
	err := &url.Error{Op: "Get", URL: "https://user:secret@remote.example/x?token=abc", Err: errors.New("boom")}
	u.fail(httptest.NewRecorder(), httptest.NewRequest("GET", "/-/api/x", nil), err)
	if out := buf.String(); strings.Contains(out, "secret") || strings.Contains(out, "abc") || !strings.Contains(out, "boom") {
		t.Fatalf("log: %s", out)
	}
}
