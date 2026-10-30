package ui

import (
	"reflect"
	"strings"
	"testing"
)

// YAML output reads back as the same JSON values: strings that look like
// other types stay strings.
func TestConfRenderRoundTrip(t *testing.T) {
	obj := map[string]any{"a": "true", "b": "123", "c": "null", "d": "~", "e": "line1\nline2\n", "f": "2026-10-30", "g": "*.local",
		"h": "0x10", "i": "1e3", "j": "", "k": "-1", "l": "yes", "m": []any{}, "n": nil, "o": 1 << 40, "p": false, "q": "a: b", "r": "#x"}
	for _, format := range []string{"yaml", "json"} {
		b, err := renderConf([]confKV{{"version", confVersion}, {"settings", obj}}, format)
		if err != nil {
			t.Fatal(err)
		}
		top, err := parseConf(b)
		if err != nil {
			t.Fatalf("%s: %v\n%s", format, err, b)
		}
		want, _ := toConfAny(obj)
		if !reflect.DeepEqual(top["settings"], want) {
			t.Fatalf("%s round trip:\n got %#v\nwant %#v\n%s", format, top["settings"], want, b)
		}
	}
}

func TestConfSecretsBox(t *testing.T) {
	payload := map[string]map[string]map[string]string{"smtp": {"": {"password": "plain-secret-value"}}}
	box, err := sealConfigSecrets(payload, "a long passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(box.Data, "plain-secret") || box.N != 1<<15 || box.R != 8 || box.P != 1 {
		t.Fatalf("box %+v", box)
	}
	got, err := openConfigSecrets(box, "a long passphrase")
	if err != nil || !reflect.DeepEqual(got, payload) {
		t.Fatalf("open: %v %v", got, err)
	}
	if _, err := openConfigSecrets(box, "another passphrase"); err == nil || !strings.Contains(err.Error(), "do not decrypt") {
		t.Fatalf("wrong passphrase: %v", err)
	}
	big := box
	big.N = 1 << 20 // a document must not make the server spend 1 GiB
	if _, err := openConfigSecrets(big, "a long passphrase"); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("huge N: %v", err)
	}
	tampered := box
	tampered.Data = box.Data[:len(box.Data)-4] + "AAAA"
	if _, err := openConfigSecrets(tampered, "a long passphrase"); err == nil {
		t.Fatal("a changed block opened")
	}
	if checkPassphrase("short") == nil {
		t.Fatal("a short passphrase accepted")
	}
}
