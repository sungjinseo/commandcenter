package cargo

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
		{"GET", "config.json", auth.Read},
		{"GET", "3/s/serde", auth.Read},
		{"HEAD", "crates/serde/1.0.0/download", auth.Read},
		{"PUT", "api/v1/crates/new", auth.Add},
		{"DELETE", "api/v1/crates/serde/1.0.0/yank", auth.Edit},
		{"PUT", "api/v1/crates/serde/1.0.0/unyank", auth.Edit},
		{"POST", "nonsense/path/x/y/z/q", auth.Add},
		{"DELETE", "nonsense/path/x/y/z/q", auth.Delete},
	} {
		if got := h.Access(httptest.NewRequest(c.method, "/", nil), nil, c.path); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.method, c.path, got, c.want)
		}
	}
}
