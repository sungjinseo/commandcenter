package repo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"commandcenter/internal/config"
)

// policyHook serves policy formats from a map, like cleanup.PolicyLookup.
func policyHook(formats map[string]string) func(context.Context, string) (string, bool, error) {
	return func(_ context.Context, name string) (string, bool, error) {
		f, ok := formats[name]
		return f, ok, nil
	}
}

func wantValidation(t *testing.T, err error, want string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Error(), want) {
		t.Fatalf("want validation error containing %q, got %v", want, err)
	}
}

func TestRegistryCleanupPolicyValidation(t *testing.T) {
	reg, db := newRegistry(t, baseDefs()...)
	reg.Policies = policyHook(map[string]string{"any": "all", "mvn": "maven", "npm-only": "npm"})
	ctx := context.Background()

	if err := reg.Create(ctx, config.Repository{Name: "h2", Format: "maven", Type: "hosted", CleanupPolicy: "mvn"}); err != nil {
		t.Fatalf("matching format: %v", err)
	}
	if err := reg.Create(ctx, config.Repository{Name: "n", Format: "npm", Type: "hosted", CleanupPolicy: "any"}); err != nil {
		t.Fatalf("format all: %v", err)
	}
	wantValidation(t, reg.Create(ctx, config.Repository{Name: "h3", Format: "maven", Type: "hosted", CleanupPolicy: "npm-only"}),
		`cleanup policy "npm-only" is for npm repositories`)
	wantValidation(t, reg.Create(ctx, config.Repository{Name: "h4", Format: "maven", Type: "hosted", CleanupPolicy: "ghost"}),
		`unknown cleanup policy "ghost"`)
	wantValidation(t, reg.Create(ctx, config.Repository{Name: "c", Format: "cargo", Type: "hosted", CleanupPolicy: "any"}),
		`cleanup is not supported for cargo hosted repositories`)
	wantValidation(t, reg.Update(ctx, "g", config.Repository{Members: []string{"h", "p"}, WriteMember: "h", CleanupPolicy: "any"}, ClearSecrets{}),
		`cleanupPolicy is only valid for hosted and proxy repositories`)

	// update: set, then clear
	if err := reg.Update(ctx, "p", config.Repository{Remote: "https://x.example/maven2", CleanupPolicy: "mvn"}, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	if reg.Get("p").CleanupPolicy != "mvn" {
		t.Fatalf("not published: %+v", reg.Get("p").Repository)
	}
	stored, _ := db.ListRepositories(ctx)
	for _, d := range stored {
		if d.Name == "p" && d.CleanupPolicy != "mvn" {
			t.Fatalf("not stored: %+v", d)
		}
	}
	wantValidation(t, reg.Update(ctx, "p", config.Repository{Remote: "https://x.example/maven2", CleanupPolicy: "npm-only"}, ClearSecrets{}),
		`cleanup policy "npm-only" is for npm repositories`)
	if reg.Get("p").CleanupPolicy != "mvn" {
		t.Fatal("rejected update was published")
	}

	// without a lookup no policy can be checked, so none is accepted
	reg.Policies = nil
	wantValidation(t, reg.Create(ctx, config.Repository{Name: "h5", Format: "maven", Type: "hosted", CleanupPolicy: "mvn"}),
		"cleanup policies are not available")
}

func TestRegistryPolicyLookupError(t *testing.T) {
	reg, _ := newRegistry(t, baseDefs()...)
	boom := errors.New("db down")
	reg.Policies = func(context.Context, string) (string, bool, error) { return "", false, boom }
	if err := reg.Create(context.Background(), config.Repository{Name: "h2", Format: "maven", Type: "hosted", CleanupPolicy: "x"}); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if reg.Get("h2") != nil {
		t.Fatal("created despite the lookup error")
	}
}

func TestRegistryWithWriteLock(t *testing.T) {
	reg, _ := newRegistry(t, baseDefs()...)
	var got []string
	err := reg.WithWriteLock(func(s *Snapshot) error {
		for _, rp := range s.Sorted {
			got = append(got, rp.Name)
		}
		return errors.New("stop")
	})
	if err == nil || err.Error() != "stop" || len(got) != 3 {
		t.Fatalf("got %v %v", got, err)
	}
	// the lock is released afterwards
	if err := reg.Create(context.Background(), config.Repository{Name: "h2", Format: "maven", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryUpdateKeepCleanupPolicy(t *testing.T) {
	reg, _ := newRegistry(t, baseDefs()...)
	reg.Policies = policyHook(map[string]string{"mvn": "maven"})
	ctx := context.Background()
	if err := reg.Update(ctx, "h", config.Repository{CleanupPolicy: "mvn"}, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	// Keep ignores def's value, even a different one.
	if err := reg.Update(ctx, "h", config.Repository{WritePolicy: "deny"}, ClearSecrets{KeepCleanupPolicy: true}); err != nil {
		t.Fatal(err)
	}
	if got := reg.Get("h"); got.CleanupPolicy != "mvn" || got.WritePolicy != "deny" {
		t.Fatalf("kept: %+v", got.Repository)
	}
	if err := reg.Update(ctx, "h", config.Repository{}, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	if got := reg.Get("h").CleanupPolicy; got != "" {
		t.Fatalf("cleared: %q", got)
	}
}
