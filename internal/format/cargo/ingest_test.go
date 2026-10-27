package cargo

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"commandcenter/internal/repo"
)

type recSink struct{ got []string }

func (s *recSink) Emit(kind, actor, repoName, subject, detail string) {
	s.got = append(s.got, kind+"|"+actor+"|"+repoName+"|"+subject)
}

func lineFor(name, vers string, crate []byte, yanked bool) []byte {
	return []byte(fmt.Sprintf(`{"name":%q,"vers":%q,"deps":[{"name":"serde","req":"^1","features":[],"optional":false,"default_features":true,"target":null,"kind":"normal"}],"cksum":%q,"features":{"x":[]},"yanked":%v}`,
		name, vers, sum(string(crate)), yanked))
}

func TestIngestFreshIdenticalAndConflict(t *testing.T) {
	e := newEnv(t)
	sink := &recSink{}
	e.h.Store.Events = sink
	ctx := context.Background()
	rp := e.repos["cargo-hosted"]
	crate := testCrate("smoke-hello", "1.0.0", "a")

	res, err := e.h.Ingest(ctx, rp, bytes.NewReader(crate), lineFor("smoke-hello", "1.0.0", crate, false), "system")
	if err != nil || res != repo.Ingested {
		t.Fatalf("fresh: %q %v", res, err)
	}
	l := lines(e.do("GET", "cargo-hosted", "sm/ok/smoke-hello", nil).Body.String())
	if len(l) != 1 || l[0]["cksum"] != sum(string(crate)) || l[0]["yanked"] != false || l[0]["name"] != "smoke-hello" {
		t.Fatalf("index: %v", l)
	}
	if w := e.do("GET", "cargo-hosted", "crates/smoke-hello/1.0.0/download", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), crate) {
		t.Fatalf("download: %d", w.Code)
	}

	res, err = e.h.Ingest(ctx, rp, bytes.NewReader(crate), lineFor("smoke-hello", "1.0.0", crate, false), "system")
	if err != nil || res != repo.Skipped {
		t.Fatalf("identical: %q %v", res, err)
	}
	other := testCrate("smoke-hello", "1.0.0", "different")
	_, err = e.h.Ingest(ctx, rp, bytes.NewReader(other), lineFor("smoke-hello", "1.0.0", other, false), "system")
	if err == nil || !repo.IsIngestConflict(err) {
		t.Fatalf("different crate: %v", err)
	}
	if len(sink.got) != 1 || sink.got[0] != "upload|system|cargo-hosted|smoke-hello@1.0.0" {
		t.Fatalf("events = %v", sink.got)
	}
}

func TestIngestPreservesYankedAndIgnoresWritePolicy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["cargo-hosted"]
	crate := testCrate("Yank_Me", "0.1.0", "y")
	if _, err := e.h.Ingest(ctx, rp, bytes.NewReader(crate), lineFor("Yank_Me", "0.1.0", crate, true), "system"); err != nil {
		t.Fatal(err)
	}
	l := lines(e.do("GET", "cargo-hosted", "ya/nk/yank_me", nil).Body.String())
	if len(l) != 1 || l[0]["yanked"] != true || l[0]["name"] != "Yank_Me" {
		t.Fatalf("index: %v", l)
	}
	pk, _ := e.h.Store.DB.ListPackages(ctx, "cargo-hosted", "yank_me")
	if len(pk) != 1 || !pk[0].Yanked {
		t.Fatalf("row = %+v", pk)
	}
	// A name differing only by case or - / _ is another crate: refused.
	c2 := testCrate("yank-me", "0.2.0", "z")
	if _, err := e.h.Ingest(ctx, rp, bytes.NewReader(c2), lineFor("yank-me", "0.2.0", c2, false), "system"); err == nil || !repo.IsIngestConflict(err) {
		t.Fatalf("name clash: %v", err)
	}
}

func TestIngestRefusesBadInput(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["cargo-hosted"]
	crate := testCrate("smoke-hello", "1.0.0", "a")
	good := lineFor("smoke-hello", "1.0.0", crate, false)

	other := testCrate("smoke-hello", "1.0.0", "b")
	cases := map[string]struct {
		crate []byte
		line  []byte
		want  string
	}{
		"cksum mismatch": {other, good, "cksum"},
		"name mismatch":  {crate, lineFor("smoke-other", "1.0.0", crate, false), "invalid .crate"},
		"vers mismatch":  {crate, lineFor("smoke-hello", "1.0.1", crate, false), "invalid .crate"},
		"bad json":       {crate, []byte("nope"), "invalid index line"},
		"no cksum":       {crate, []byte(`{"name":"smoke-hello","vers":"1.0.0"}`), "cksum"},
		"empty crate":    {nil, lineFor("smoke-hello", "1.0.0", nil, false), "empty crate"},
		"not a crate":    {[]byte("plain"), lineFor("smoke-hello", "1.0.0", []byte("plain"), false), "invalid .crate"},
	}
	for name, c := range cases {
		_, err := e.h.Ingest(ctx, rp, bytes.NewReader(c.crate), c.line, "system")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if pk, _ := e.h.Store.DB.ListPackages(ctx, "cargo-hosted", "smoke-hello"); len(pk) != 0 {
		t.Fatal("row stored by a refused ingest")
	}
}

func TestIngestBoundsIndexLineAndCrate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["cargo-hosted"]
	crate := testCrate("smoke-hello", "1.0.0", "a")
	good := lineFor("smoke-hello", "1.0.0", crate, false)

	oldLine := maxLine
	maxLine = 50
	_, err := e.h.Ingest(ctx, rp, bytes.NewReader(crate), good, "system")
	maxLine = oldLine
	if err == nil || !strings.Contains(err.Error(), "index entry is larger") {
		t.Fatalf("big line: %v", err)
	}
	oldCrate := maxCrate
	maxCrate = 10
	_, err = e.h.Ingest(ctx, rp, bytes.NewReader(crate), good, "system")
	maxCrate = oldCrate
	if err == nil || !strings.Contains(err.Error(), "crate is larger") {
		t.Fatalf("big crate: %v", err)
	}
}

// A reserved name cannot start a new crate through the import either.
func TestIngestRefusesReservedName(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["cargo-hosted"]
	crate := testCrate("std", "1.0.0", "a")
	_, err := e.h.Ingest(ctx, rp, bytes.NewReader(crate), lineFor("std", "1.0.0", crate, false), "system")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("err = %v", err)
	}
	if pk, _ := e.h.Store.DB.ListPackages(ctx, "cargo-hosted", "std"); len(pk) != 0 {
		t.Fatal("row stored")
	}
}
