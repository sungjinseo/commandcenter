package auth

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"commandcenter/internal/meta"
)

var errSig = errors.New("bad signature")

func TestS3Keys(t *testing.T) {
	f := newUserTokenFixture(t)
	ctx := context.Background()
	k, secret, err := f.a.CreateS3Key(ctx, "adder", "ci", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^CCK[A-Z2-7]{17}$`).MatchString(k.AccessKeyID) || len(secret) != 40 || k.Owner != "adder" {
		t.Fatalf("key %+v secret %q", k, secret)
	}
	ok := func(s string) error {
		if s != secret {
			return errSig
		}
		return nil
	}
	p, err := f.a.S3Check(ctx, k.AccessKeyID, ok)
	if err != nil || p.User != "adder" || p.Via != ViaToken {
		t.Fatalf("check: %+v %v", p, err)
	}
	if l, _ := f.db.ListS3Keys(ctx, "adder"); len(l) != 1 || l[0].LastUsed.IsZero() {
		t.Fatalf("last used: %+v", l)
	}
	if _, err := f.a.S3Check(ctx, k.AccessKeyID, func(string) error { return errSig }); !errors.Is(err, errSig) {
		t.Fatalf("bad signature: %v", err)
	}
	if _, err := f.a.S3Check(ctx, "CCKNOPE", ok); !errors.Is(err, ErrS3KeyUnknown) {
		t.Fatalf("unknown: %v", err)
	}
	// A disabled owner fails at once, though the key is cached.
	f.users[1].Status = meta.StatusDisabled
	f.apply()
	if _, err := f.a.S3Check(ctx, k.AccessKeyID, ok); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("disabled owner: %v", err)
	}
	f.users[1].Status = meta.StatusActive
	f.apply()
	// A deleted key fails at once on this node.
	if _, err := f.a.DeleteS3Key(ctx, k.AccessKeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.S3Check(ctx, k.AccessKeyID, ok); !errors.Is(err, ErrS3KeyUnknown) {
		t.Fatalf("deleted: %v", err)
	}
	// Expiry.
	k2, s2, err := f.a.CreateS3Key(ctx, "adder", "", f.clock.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(2 * time.Hour)
	if _, err := f.a.S3Check(ctx, k2.AccessKeyID, func(s string) error { return nil }); !errors.Is(err, ErrS3KeyExpired) {
		t.Fatalf("expired: %v %q", err, s2)
	}
	// At most MaxS3KeysPerUser.
	for i := 1; i < MaxS3KeysPerUser; i++ {
		if _, _, err := f.a.CreateS3Key(ctx, "adder", "", time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.a.CreateS3Key(ctx, "adder", "", time.Time{}); !errors.Is(err, ErrTooManyS3Keys) {
		t.Fatalf("too many: %v", err)
	}
	if _, _, err := f.a.CreateS3Key(ctx, "nobody", "", time.Time{}); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("unknown owner: %v", err)
	}
}
