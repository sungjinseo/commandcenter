package golang

import (
	"net/http/httptest"
	"testing"

	"commandcenter/internal/auth"
)

func TestAccessKinds(t *testing.T) {
	h := &Handler{}
	for _, c := range []struct {
		method, path string
		want         auth.Access
	}{
		{"GET", "example.com/m/@v/list", auth.Read},
		{"HEAD", "example.com/m/@v/v1.0.0.zip", auth.Read},
		{"PUT", "example.com/m/@v/v1.0.0.zip", auth.Add},
		{"DELETE", "example.com/m/@v/v1.0.0.zip", auth.Delete}, // was Add (QA M-11)
	} {
		if got := h.Access(httptest.NewRequest(c.method, "/", nil), nil, c.path); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.method, c.path, got, c.want)
		}
	}
}
