package repo

import (
	"context"
	"net/http"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
)

// PublishTimeHeader lets an upload state when its content was first
// published, as an RFC 3339 time: a move from another server keeps what a
// client reads (npm's time.<version>, Helm's created, PyPI's upload_time,
// NuGet's published, ...) and what cleanup's age rules count from. It is an
// administrator's alone (MayStatePublishTime): the age gates of clients such
// as yarn 4 trust these times. Everything else about the upload stays: the
// privilege check, the write policy, the validation. The upload's event
// names the stated time, and the row's checked_at keeps when it really came.
//
// Generated documents (index.yaml, a package document, repodata) keep the
// time they were written: a Last-Modified older than the copy a client holds
// would answer it 304.
const PublishTimeHeader = "X-Command-Center-Publish-Time"

// publishTimeFormats are the formats whose uploads record a stated time. The
// others refuse it rather than drop it: docker has no publish time a client
// reads, and its garbage collection counts from the rows' times.
var publishTimeFormats = map[string]bool{"maven": true, "npm": true, "pypi": true, "raw": true, "helm": true, "apt": true, "yum": true,
	"nuget": true, "rubygems": true, "r": true, "go": true, "gitlfs": true, "conda": true}

// MayStatePublishTime reports whether p may state a publish time: an
// administrator signed in with a password or a session. A token may no more
// than it may change a setting: a stolen one could publish a version that
// looks a year old to the clients that wait for a version to age.
func MayStatePublishTime(p auth.Principal) bool { return p.Admin && p.Via != auth.ViaToken }

type publishTimeKey struct{}

// WithPublishTime returns ctx with the publish time value states for an
// upload by p to a repository of format, or the refusal: a caller who may
// not state one (never ignored: the caller would believe its time was
// taken), a format that records none, or a value that is not a time from
// after the epoch up to now.
func WithPublishTime(ctx context.Context, p auth.Principal, format, value string, now time.Time) (context.Context, error) {
	if !MayStatePublishTime(p) {
		return nil, Errorf(http.StatusForbidden, "only an administrator signed in with a password may send %s", PublishTimeHeader)
	}
	if !publishTimeFormats[format] {
		return nil, Errorf(http.StatusBadRequest, "%s repositories do not record %s", format, PublishTimeHeader)
	}
	t, err := time.Parse(time.RFC3339, value)
	switch {
	case err != nil:
		return nil, Errorf(http.StatusBadRequest, "%s: %.64q is not an RFC 3339 time", PublishTimeHeader, value)
	case t.After(now):
		return nil, Errorf(http.StatusBadRequest, "%s: %s is in the future", PublishTimeHeader, value)
	case t.UnixMilli() <= 0: // the database reads 0 as no time at all
		return nil, Errorf(http.StatusBadRequest, "%s: %s is not after 1970", PublishTimeHeader, value)
	}
	return context.WithValue(ctx, publishTimeKey{}, t.UTC().Truncate(time.Millisecond)), nil
}

// PublishTime returns the publish time the request of ctx stated.
func PublishTime(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(publishTimeKey{}).(time.Time)
	return t, ok
}

// PublishedAt is the time an upload is on record as published: the one the
// request stated, else now.
func PublishedAt(ctx context.Context, now time.Time) time.Time {
	if t, ok := PublishTime(ctx); ok {
		return t
	}
	return now
}

// Published returns the asset option that gives a file its request's stated
// publish time. CheckedAt keeps the write's own time.
func Published(ctx context.Context) func(*meta.Asset) {
	return func(a *meta.Asset) {
		if t, ok := PublishTime(ctx); ok {
			a.CreatedAt, a.LastModified = t, t
		}
	}
}

// PublishedAsset is NewAsset for a file a client published (Published).
func (s *Store) PublishedAsset(ctx context.Context, repo, path, contentType string, sums blob.Sums) meta.Asset {
	a := s.NewAsset(repo, path, contentType, sums)
	Published(ctx)(&a)
	return a
}
