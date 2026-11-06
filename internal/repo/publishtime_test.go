package repo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
)

func TestWithPublishTime(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	admin := auth.Principal{User: "root", Admin: true, Via: auth.ViaPassword}
	session := auth.Principal{User: "root", Admin: true, Via: auth.ViaSession}
	for _, c := range []struct {
		what   string
		p      auth.Principal
		format string
		value  string
		status int
	}{
		{"an administrator", admin, "npm", "2020-01-02T03:04:05Z", 0},
		{"an offset", admin, "helm", "2020-01-02T12:04:05+09:00", 0},
		{"now and a session", session, "raw", "2026-11-06T12:00:00Z", 0},
		{"anonymous", auth.Principal{}, "npm", "2020-01-02T03:04:05Z", http.StatusForbidden},
		{"a user", auth.Principal{User: "ci", Via: auth.ViaPassword}, "npm", "2020-01-02T03:04:05Z", http.StatusForbidden},
		{"a token", auth.Principal{User: "root", Admin: true, Via: auth.ViaToken}, "npm", "2020-01-02T03:04:05Z", http.StatusForbidden},
		{"docker", admin, "docker", "2020-01-02T03:04:05Z", http.StatusBadRequest},
		{"not a time", admin, "npm", "yesterday", http.StatusBadRequest},
		{"the future", admin, "npm", "2026-11-06T12:00:01Z", http.StatusBadRequest},
		{"the epoch", admin, "npm", "1970-01-01T00:00:00Z", http.StatusBadRequest},
		{"before the epoch", admin, "npm", "1969-01-01T00:00:00Z", http.StatusBadRequest},
	} {
		ctx, err := WithPublishTime(context.Background(), c.p, c.format, c.value, now)
		if c.status == 0 {
			if err != nil {
				t.Errorf("%s: %v", c.what, err)
			} else if got, ok := PublishTime(ctx); !ok || got.Location() != time.UTC {
				t.Errorf("%s: %v %v", c.what, got, ok)
			}
			continue
		}
		var re *Error
		if !errors.As(err, &re) || re.Status != c.status {
			t.Errorf("%s: %v, want %d", c.what, err, c.status)
		}
	}
	// Without a stated time everything is the write's own.
	if got := PublishedAt(context.Background(), now); !got.Equal(now) {
		t.Errorf("no time stated: %v", got)
	}
	ctx, _ := WithPublishTime(context.Background(), admin, "raw", "2020-01-02T03:04:05.678912Z", now)
	want := time.Date(2020, 1, 2, 3, 4, 5, 678000000, time.UTC)
	if got := PublishedAt(ctx, now); !got.Equal(want) {
		t.Errorf("stated: %v", got)
	}
	a := meta.Asset{CreatedAt: now, LastModified: now, CheckedAt: now}
	Published(ctx)(&a)
	if !a.CreatedAt.Equal(want) || !a.LastModified.Equal(want) || !a.CheckedAt.Equal(now) {
		t.Errorf("asset: %+v", a)
	}
}

type detailSink struct{ details []string }

func (d *detailSink) Emit(_, _, _, _, detail string) { d.details = append(d.details, detail) }

// The upload's event says a time was stated, for whoever reads the log.
func TestEmitNamesAStatedTime(t *testing.T) {
	sink := &detailSink{}
	s := &Store{Events: sink}
	admin := auth.Principal{User: "root", Admin: true, Via: auth.ViaPassword}
	ctx, err := WithPublishTime(context.Background(), admin, "raw", "2020-01-02T03:04:05Z", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s.Emit(httptest.NewRequest("PUT", "/x", nil).WithContext(ctx), "upload", "raw", "x", "size=1")
	s.Emit(httptest.NewRequest("PUT", "/y", nil), "upload", "raw", "y", "")
	if got := sink.details[0]; !strings.Contains(got, "size=1") || !strings.Contains(got, "publishTime=2020-01-02T03:04:05Z") {
		t.Errorf("event: %q", got)
	}
	if got := sink.details[1]; got != "" {
		t.Errorf("event without a stated time: %q", got)
	}
}
