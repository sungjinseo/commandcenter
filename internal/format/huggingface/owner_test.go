package huggingface

import (
	"context"
	"testing"
)

// A document of the hub that is no JSON does not replace the good one, which
// would stand until metadataMaxAge ends.
func TestDocumentKeepsCopy(t *testing.T) {
	e := newEnv(t)
	const doc = "api/models/org/model/revision/main"
	if resp, _ := e.do("GET", doc); resp.StatusCode != 200 {
		t.Fatalf("the document: %d", resp.StatusCode)
	}
	before, err := e.store.Get(context.Background(), "hf", doc)
	if err != nil {
		t.Fatal(err)
	}
	e.garbage.Store(true)
	e.now.Add(2 * 3600)
	if resp, body := e.do("GET", doc); resp.StatusCode != 200 || resp.Header.Get("Warning") == "" {
		t.Fatalf("garbage: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	if now, err := e.store.Get(context.Background(), "hf", doc); err != nil || now.BlobSHA256 != before.BlobSHA256 {
		t.Fatalf("garbage replaced the copy: %v", err)
	}
}
