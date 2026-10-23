package cargo

import (
	"context"
	"errors"
	"testing"

	"commandcenter/internal/component"
	"commandcenter/internal/repo/repotest"
)

func TestDeleteComponentCargoRefused(t *testing.T) {
	e := newEnv(t)
	e.seedHosted(t, "serde", "1.0.0", "crate")
	sink := &repotest.Sink{}
	e.h.Store.Events = sink
	c := component.Component{Format: "cargo", Name: "serde", Version: "1.0.0"}
	_, err := e.h.DeleteComponent(context.Background(), e.repos["cargo-hosted"], c, "system")
	if !errors.Is(err, component.ErrNotSupported) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.h.Store.DB.GetAsset(context.Background(), "cargo-hosted", crateKey("serde", "1.0.0")); err != nil {
		t.Fatalf("crate removed: %v", err)
	}
	if pkgs, _ := e.h.Store.DB.ListPackages(context.Background(), "cargo-hosted", "serde"); len(pkgs) != 1 {
		t.Fatalf("rows = %+v", pkgs)
	}
	if len(sink.Events()) != 0 {
		t.Fatalf("events: %v", sink.Events())
	}
}

func TestDeleteComponentCargoGuardFirst(t *testing.T) {
	e := newEnv(t)
	c := component.Component{Format: "npm", Name: "serde", Version: "1.0.0"}
	_, err := e.h.DeleteComponent(context.Background(), e.repos["cargo-hosted"], c, "system")
	if err == nil || errors.Is(err, component.ErrNotSupported) {
		t.Fatalf("wrong-format component: %v", err)
	}
}
