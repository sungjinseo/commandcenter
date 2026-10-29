package repo

import "context"

// MetadataRebuilder regenerates the derived index files of a hosted
// repository (apt dists/, yum repodata/, helm index.yaml, conda repodata
// and channeldata) from its rows, the way an upload or delete does, for the
// repository.rebuild-metadata task. logf (never nil) receives progress.
type MetadataRebuilder interface {
	RebuildMetadata(ctx context.Context, rp *Repository, logf func(format string, args ...any)) error
}
