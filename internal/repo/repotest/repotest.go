// Package repotest has test helpers for code that emits activity events.
package repotest

import (
	"fmt"
	"strings"
	"sync"
)

// Event is one recorded Emit call.
type Event struct{ Kind, Actor, Repo, Subject, Detail string }

// Sink is a repo.EventSink that keeps what it receives.
type Sink struct {
	mu  sync.Mutex
	Got []Event
}

func (s *Sink) Emit(kind, actor, repo, subject, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Got = append(s.Got, Event{kind, actor, repo, subject, detail})
}

// Events returns a copy of the recorded events.
func (s *Sink) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.Got...)
}

// Reset forgets the recorded events.
func (s *Sink) Reset() {
	s.mu.Lock()
	s.Got = nil
	s.mu.Unlock()
}

// String lists events as "kind actor repo subject" lines, for failure messages.
func (s *Sink) String() string {
	var b strings.Builder
	for _, e := range s.Events() {
		fmt.Fprintf(&b, "%s|%s|%s|%s\n", e.Kind, e.Actor, e.Repo, e.Subject)
	}
	return b.String()
}
