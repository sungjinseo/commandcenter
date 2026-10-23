package npm

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/auth"
)

func TestPublishSemaphoreBoundsConcurrency(t *testing.T) {
	ctx := context.Background()
	rel1, err := acquirePublish(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rel2, err := acquirePublish(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() {
		rel, err := acquirePublish(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		got <- rel
	}()
	select {
	case <-got:
		t.Fatal("third publish did not wait")
	case <-time.After(100 * time.Millisecond):
	}
	rel1()
	select {
	case rel3 := <-got:
		rel3()
	case <-time.After(2 * time.Second):
		t.Fatal("third publish did not proceed after a release")
	}
	rel2()
}

func TestPublishSemaphoreCancel(t *testing.T) {
	bg := context.Background()
	rel1, _ := acquirePublish(bg)
	rel2, _ := acquirePublish(bg)
	defer rel1()
	defer rel2()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { _, err := acquirePublish(ctx); done <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error for a cancelled context")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled acquire did not return")
	}
	// the slot count is intact: releasing one lets a new acquire succeed
	rel1()
	rel, err := acquirePublish(bg)
	if err != nil {
		t.Fatal(err)
	}
	rel()
}

func TestPublishReleasesSlotAfterFailure(t *testing.T) {
	old := publishSem
	publishSem = make(chan struct{}, 1)
	defer func() { publishSem = old }()
	e := newEnv(t)
	if w := e.do("PUT", "npm-hosted", "@s/slot", "not json", ci); w.Code != 400 {
		t.Fatalf("bad publish: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("PUT", "npm-hosted", "@s/slot", publishBody("@s/slot", "1.0.0", []byte("tgz"), true), ci); w.Code != 400 {
		t.Fatalf("tampered publish: %d", w.Code)
	}
	done := make(chan int, 1)
	go func() {
		done <- e.do("PUT", "npm-hosted", "@s/slot", publishBody("@s/slot", "1.0.0", []byte("tgz"), false), ci).Code
	}()
	select {
	case code := <-done:
		if code != 201 {
			t.Fatalf("publish after failures: %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publish blocked: slot leaked by a failed publish")
	}
}

func fillPublishSlots(t *testing.T) {
	t.Helper()
	old := publishSem
	publishSem = make(chan struct{}, 1)
	publishSem <- struct{}{}
	t.Cleanup(func() { publishSem = old })
}

func TestPublishWaitTimeoutIs503(t *testing.T) {
	fillPublishSlots(t)
	oldWait := publishWaitTimeout
	publishWaitTimeout = 50 * time.Millisecond
	defer func() { publishWaitTimeout = oldWait }()
	e := newEnv(t)
	w := e.do("PUT", "npm-hosted", "@s/wait", publishBody("@s/wait", "1.0.0", []byte("tgz"), false), ci)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "too many concurrent publishes") {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}

func TestPublishCancelledWaitIsNotAnError(t *testing.T) {
	fillPublishSlots(t)
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/repository/npm-hosted/@s/gone", strings.NewReader(publishBody("@s/gone", "1.0.0", []byte("tgz"), false)))
	r = r.WithContext(auth.WithPrincipal(ctx, ci))
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	e.h.Serve(w, r, e.repos["npm-hosted"], "@s/gone")
	if w.Code == 500 || strings.Contains(w.Body.String(), "internal error") {
		t.Fatalf("a cancelled wait must not be a 500: %d %q", w.Code, w.Body.String())
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

func TestPublishSetsReadDeadline(t *testing.T) {
	e := newEnv(t)
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest("PUT", "/repository/npm-hosted/@s/dl", strings.NewReader(publishBody("@s/dl", "1.0.0", []byte("tgz"), false)))
	r = r.WithContext(auth.WithPrincipal(r.Context(), ci))
	e.h.Serve(w, r, e.repos["npm-hosted"], "@s/dl")
	if w.Code != 201 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	// Each body read runs under the publish idle deadline (armed before the
	// read, cleared after it; final review M1).
	armed := 0
	for _, d := range w.deadlines {
		if d.IsZero() {
			continue
		}
		armed++
		if time.Until(d) < publishReadTimeout-time.Minute {
			t.Fatalf("deadlines = %v: want publishReadTimeout", w.deadlines)
		}
	}
	if armed == 0 {
		t.Fatalf("deadlines = %v: want a non-zero deadline while publishing", w.deadlines)
	}
}
