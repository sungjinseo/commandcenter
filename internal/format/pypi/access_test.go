package pypi

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
		{"GET", "simple/demo/", auth.Read},
		{"HEAD", "packages/demo/demo-1.0.tar.gz", auth.Read},
		{"POST", "", auth.Add},
	} {
		if got := h.Access(httptest.NewRequest(c.method, "/", nil), nil, c.path); got != c.want {
			t.Errorf("%s %q: %v, want %v", c.method, c.path, got, c.want)
		}
	}
}
