package blob

// This file lets the azure backend compile on its own: the orchestrator drops
// it on merge, as the reconcile agent defines metaBackend in blob.go.

// metaBackend keeps small named objects next to the blobs (the reconcile journal). key is a slash path such as
// "journal/2026/10/30/09/<name>.json" (no leading slash, no "..", [A-Za-z0-9._/-] only); walk never reports them.
type metaBackend interface {
	putMeta(key string, data []byte) error
	getMeta(key string) ([]byte, error)                      // ErrNotFound when missing
	listMeta(prefix string, fn func(key string) error) error // keys starting with prefix, lexical order
	removeMeta(key string) error                             // a missing key is not an error
}

var _ metaBackend = (*azure)(nil)
