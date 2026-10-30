package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
)

// config export and apply work on the database directly, as the other
// commands do, through the API's handlers.
func TestConfigCommand(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, configYAML(t, data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"config", "export", "-config", cfgPath}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.HasPrefix(s, "version: 1\n") || !strings.Contains(s, "name: default") || strings.Contains(s, "logging:") {
		t.Fatalf("export: %s", s) // a CLI process has no running log level
	}

	doc := filepath.Join(dir, "doc.yaml")
	os.WriteFile(doc, []byte("version: 1\nroutingRules:\n  - {name: r1, mode: block, matchers: [x]}\n"+
		"repositories:\n  - {name: raw1, format: raw, type: hosted, routingRule: \"\"}\n"), 0o600)
	out.Reset()
	if err := run([]string{"config", "apply", "-config", cfgPath, "-f", doc, "-dry-run"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"create routingRules r1\n", "create repositories raw1\n", "planned: 2 to create, 0 to update, 0 to delete"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("dry run output lacks %q:\n%s", want, out.String())
		}
	}
	db, err := meta.Open(metatest.URL(t, data))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if rules, _ := db.ListRoutingRules(context.Background()); len(rules) != 0 {
		t.Fatal("the dry run created a rule")
	}
	out.Reset()
	if err := run([]string{"config", "apply", "-config", cfgPath, "-f", doc}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "create routingRules r1 [applied]") || !strings.Contains(out.String(), "applied: 2 to create") {
		t.Fatalf("apply output: %s", out.String())
	}
	if rules, _ := db.ListRoutingRules(context.Background()); len(rules) != 1 || rules[0].Mode != "BLOCK" {
		t.Fatalf("rules: %+v", rules)
	}
	evs, _ := db.ListEvents(context.Background(), 50, 0, "")
	var kinds []string
	for _, e := range evs {
		if e.Actor == "cli:config" {
			kinds = append(kinds, e.Kind)
		}
	}
	if !strings.Contains(strings.Join(kinds, ","), "repo.create") || !strings.Contains(strings.Join(kinds, ","), "configuration.apply") {
		t.Fatalf("events of the CLI: %v", kinds)
	}

	// Again: nothing to do; then an invalid document from stdin.
	out.Reset()
	if err := run([]string{"config", "apply", "-config", cfgPath, "-f", doc, "-dry-run"}, nil, &out); err != nil ||
		!strings.Contains(out.String(), "planned: 0 to create, 0 to update, 0 to delete") {
		t.Fatalf("second dry run: %v %s", err, out.String())
	}
	err = run([]string{"config", "apply", "-config", cfgPath, "-f", "-"}, strings.NewReader("version: 1\nrepositories:\n  - {name: x, format: nope, type: hosted}\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("invalid document: %v", err)
	}

	file := filepath.Join(dir, "export.json")
	if err := run([]string{"config", "export", "-config", cfgPath, "-format", "json", "-o", file, "-include", "routingRules,repositories"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 || !strings.Contains(string(b), `"r1"`) || strings.Contains(string(b), "blobStores") {
		t.Fatalf("export file (%v): %s", st.Mode(), b)
	}
	if err := run([]string{"config", "export", "-config", cfgPath, "-secrets", "encrypted"}, nil, &out); err == nil {
		t.Fatal("an encrypted export without a passphrase")
	}
	if err := run([]string{"config"}, nil, &out); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("usage: %v", err)
	}
}
