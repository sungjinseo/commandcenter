package repo

import (
	"context"
	"net/http"

	"commandcenter/internal/auth"
)

// EventSink receives activity events (events.Recorder). Emit must not block.
type EventSink interface {
	Emit(kind, actor, repo, subject, detail string)
}

// CtxSink is an EventSink that also reads the request's context of an
// event a request caused (the webhook dispatcher: the client's address).
type CtxSink interface {
	EmitCtx(ctx context.Context, kind, actor, repo, subject, detail string)
}

// EmitContext sends an event to sink, with ctx (the request's that caused
// it) when sink is a CtxSink.
func EmitContext(ctx context.Context, sink EventSink, kind, actor, repoName, subject, detail string) {
	if c, ok := sink.(CtxSink); ok {
		c.EmitCtx(ctx, kind, actor, repoName, subject, detail)
		return
	}
	sink.Emit(kind, actor, repoName, subject, detail)
}

// Emit reports an activity event for request r, with the request's principal
// as the actor ("" for anonymous). It does nothing when no sink is set.
func (s *Store) Emit(r *http.Request, kind, repoName, subject, detail string) {
	if s.Events == nil {
		return
	}
	EmitContext(r.Context(), s.Events, kind, auth.FromContext(r.Context()).Owner(), repoName, subject, detail)
}

// EmitAs reports an activity event with an explicit actor, for work that does
// not come from a request (events.System for scheduled tasks such as cleanup).
func (s *Store) EmitAs(kind, actor, repoName, subject, detail string) {
	if s.Events == nil {
		return
	}
	s.Events.Emit(kind, actor, repoName, subject, detail)
}
