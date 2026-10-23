package repo

import (
	"net/http"

	"commandcenter/internal/auth"
)

// EventSink receives activity events (events.Recorder). Emit must not block.
type EventSink interface {
	Emit(kind, actor, repo, subject, detail string)
}

// Emit reports an activity event for request r, with the request's principal
// as the actor ("" for anonymous). It does nothing when no sink is set.
func (s *Store) Emit(r *http.Request, kind, repoName, subject, detail string) {
	if s.Events == nil {
		return
	}
	s.Events.Emit(kind, auth.FromContext(r.Context()).Owner(), repoName, subject, detail)
}

// EmitAs reports an activity event with an explicit actor, for work that does
// not come from a request (events.System for scheduled tasks such as cleanup).
func (s *Store) EmitAs(kind, actor, repoName, subject, detail string) {
	if s.Events == nil {
		return
	}
	s.Events.Emit(kind, actor, repoName, subject, detail)
}
