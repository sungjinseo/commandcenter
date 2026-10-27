package docker

import (
	"strings"
	"testing"
)

// TestParseManifestDetails: the attrs keep what the web UI shows, the bytes
// an image's layers add up to and the platforms an index lists.
func TestParseManifestDetails(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	m, err := parseManifest([]byte(`{"schemaVersion":2,"mediaType":"`+mtSchema2+`","config":{"mediaType":"x","digest":"`+d+`","size":7},"layers":[{"mediaType":"x","digest":"`+d+`","size":100},{"mediaType":"x","digest":"`+d+`","size":250}]}`), "")
	if err != nil || m.LayerBytes != 350 || len(m.Platforms) != 0 {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	m, err = parseManifest([]byte(`{"schemaVersion":2,"mediaType":"`+mtOCIIndex+`","manifests":[`+
		`{"mediaType":"`+mtOCIManifest+`","digest":"`+d+`","size":1,"platform":{"architecture":"amd64","os":"linux"}},`+
		`{"mediaType":"`+mtOCIManifest+`","digest":"`+d+`","size":1,"platform":{"architecture":"arm64","os":"linux","variant":"v8"}},`+
		`{"mediaType":"`+mtOCIManifest+`","digest":"`+d+`","size":1}]}`), "")
	if err != nil || m.LayerBytes != 0 || strings.Join(m.Platforms, " ") != "linux/amd64 linux/arm64/v8" {
		t.Fatalf("index: %+v %v", m, err)
	}
}
