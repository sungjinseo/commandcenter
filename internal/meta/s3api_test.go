package meta

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestS3Keys(t *testing.T) {
	db := openTest(t)
	key := bytes.Repeat([]byte("a"), 32)
	if err := db.SetSecretKey(key); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i, owner := range []string{"alice", "ldap:bob", "alice"} {
		if err := db.CreateS3Key(ctx, S3Key{ID: fmt.Sprintf("K%d", i), Owner: owner, Secret: fmt.Sprintf("secret-%d", i), Created: t0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateS3Key(ctx, S3Key{ID: "K0", Owner: "carol", Secret: "s"}); !errors.Is(err, ErrExists) {
		t.Fatalf("a taken id: %v", err)
	}
	// The secret is sealed at rest and opened on read.
	raw, _ := db.GetSetting(ctx, s3KeyPrefix+"K1")
	if strings.Contains(raw, "secret-1") || !strings.Contains(raw, sealedPrefix) {
		t.Fatalf("stored: %s", raw)
	}
	k, err := db.GetS3Key(ctx, "K1")
	if err != nil || k.Owner != "ldap:bob" || k.Secret != "secret-1" || !k.Created.Equal(t0) || !k.LastUsed.IsZero() {
		t.Fatalf("get: %+v %v", k, err)
	}
	db.TouchS3Key(ctx, "K1")
	if k, _ := db.GetS3Key(ctx, "K1"); k.LastUsed.IsZero() || k.Secret != "secret-1" {
		t.Fatalf("touched: %+v", k)
	}
	list, err := db.ListS3Keys(ctx, "alice")
	if err != nil || len(list) != 2 || list[0].ID != "K0" || list[1].ID != "K2" || list[0].Secret != "" {
		t.Fatalf("alice's: %+v %v", list, err)
	}
	if list, _ := db.ListS3Keys(ctx, ""); len(list) != 3 || list[0].Owner != "alice" || list[2].Owner != "ldap:bob" {
		t.Fatalf("all: %+v", list)
	}
	for i := 3; i < 3+MaxS3KeysPerOwner-2; i++ {
		if err := db.CreateS3Key(ctx, S3Key{ID: fmt.Sprintf("K%d", i), Owner: "alice", Secret: "s", Created: t0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateS3Key(ctx, S3Key{ID: "KX", Owner: "alice", Secret: "s"}); !errors.Is(err, ErrTooManyS3Keys) {
		t.Fatalf("past the bound: %v", err)
	}
	// Rotating the secret key reseals the keys' secrets.
	newKey := bytes.Repeat([]byte("b"), 32)
	if _, _, err := db.RotateSecretKey(ctx, newKey); err != nil {
		t.Fatal(err)
	}
	after, _ := db.GetSetting(ctx, s3KeyPrefix+"K1")
	var row s3KeyRow
	json.Unmarshal([]byte(after), &row)
	box, _ := newSealer(newKey)
	if v, err := box.open(row.Secret); after == raw || err != nil || v != "secret-1" {
		t.Fatalf("after rotation: %s %v", after, err)
	}
	if err := db.DeleteS3Key(ctx, "K1"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteS3Key(ctx, "K1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted twice: %v", err)
	}
	// A deleted user's keys go with it.
	if err := db.CreateUser(ctx, User{ID: "alice", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.ListS3Keys(ctx, ""); len(list) != 0 {
		t.Fatalf("left after the user's delete: %+v", list)
	}
}
