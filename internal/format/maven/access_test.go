package maven

import (
	"net/http/httptest"
	"testing"

	"commandcenter/internal/auth"
)

func TestAccessKinds(t *testing.T) {
	h := &Handler{}
	const jar = "org/example/app/1.0/app-1.0.jar"
	for _, c := range []struct {
		method string
		want   auth.Access
	}{
		{"GET", auth.Read},
		{"HEAD", auth.Read},
		// PUT is Add even over an existing path: the writePolicy decides
		// whether an overwrite is allowed at all (maven-metadata.xml is
		// rewritten on every deploy).
		{"PUT", auth.Add},
		{"DELETE", auth.Delete},
	} {
		if got := h.Access(httptest.NewRequest(c.method, "/", nil), nil, jar); got != c.want {
			t.Errorf("%s: %v, want %v", c.method, got, c.want)
		}
	}
}
