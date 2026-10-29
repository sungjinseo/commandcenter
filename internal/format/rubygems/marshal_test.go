package rubygems

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
	"time"
)

var testDate = time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)

// TestMarshalFixtures: the writer produces what Marshal.dump does for the
// documented encodings (fixnums in every length form, strings with their
// encoding ivar, symbols and symbol links, arrays, hashes, Gem::Version),
// and the reader turns them back.
func TestMarshalFixtures(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"nil", nil, "\x04\x080"},
		{"true", true, "\x04\x08T"},
		{"zero", 0, "\x04\x08i\x00"},
		{"small", 1, "\x04\x08i\x06"},
		{"122", 122, "\x04\x08i\x7f"},
		{"123", 123, "\x04\x08i\x01\x7b"},
		{"255", 255, "\x04\x08i\x01\xff"},
		{"300", 300, "\x04\x08i\x02\x2c\x01"},
		{"70000", 70000, "\x04\x08i\x03\x70\x11\x01"},
		{"-1", -1, "\x04\x08i\xfa"},
		{"-123", -123, "\x04\x08i\x80"},
		{"-124", -124, "\x04\x08i\xff\x84"},
		{"-256", -256, "\x04\x08i\xff\x00"},
		{"-300", -300, "\x04\x08i\xfe\xd4\xfe"},
		{"string", "a", "\x04\x08I\"\x06a\x06:\x06ET"},
		{"array", []any{1, nil, true}, "\x04\x08[\x08i\x060T"},
		{"symbol hash", hash{{sym("a"), 1}}, "\x04\x08{\x06:\x06ai\x06"},
		{"symbol links", []any{"a", "b"}, "\x04\x08[\x07I\"\x06a\x06:\x06ETI\"\x06b\x06;\x00T"},
		{"Gem::Version", gemVersion("1.0"), "\x04\x08U:\x11Gem::Version[\x06I\"\x081.0\x06:\x06ET"},
		{"object", object{"Gem::Platform", hash{{sym("@cpu"), nil}, {sym("@os"), "java"}, {sym("@version"), nil}}},
			"\x04\x08o:\x12Gem::Platform\x08:\t@cpu0:\x08@osI\"\tjava\x06:\x06ET:\r@version0"},
		{"_dump", userDump{class: "T", data: []byte("xy")}, "\x04\x08u:\x06T\x07xy"},
		{"_dump with ivars", userDump{"T", []byte("xy"), hash{{sym("zone"), "UTC"}}}, "\x04\x08Iu:\x06T\x07xy\x06:\tzoneI\"\x08UTC\x06:\x06ET"},
	}
	for _, c := range cases {
		got, err := marshalDump(c.v)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(got) != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		back, err := marshalLoad(got)
		if err != nil {
			t.Fatalf("%s: load: %v", c.name, err)
		}
		if !reflect.DeepEqual(back, c.v) {
			t.Errorf("%s: round trip %#v, want %#v", c.name, back, c.v)
		}
	}
}

// TestMarshalLinks: object links ('@') from Ruby's writer resolve in
// registration order (arrays and hashes before their contents, _dump
// objects after theirs), and malformed data is an error, not a panic.
func TestMarshalLinks(t *testing.T) {
	// [["a"], @2 (the "a"), @1 (the inner array)] as Ruby dumps repeated objects
	v, err := marshalLoad([]byte("\x04\x08[\x08[\x06I\"\x06a\x06:\x06ET@\x07@\x06"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v, []any{[]any{"a"}, "a", []any{"a"}}) {
		t.Fatalf("links: %#v", v)
	}
	// Iu: the ivar string registers before the _dump object: [Iu..., @1 (ivar string), @2 (the Time)]
	tm := rubyTime(testDate)
	raw, _ := marshalDump([]any{tm})
	raw = append(raw[:3], append([]byte{0x08}, raw[4:]...)...)
	raw = append(raw, "@\x06@\x07"...)
	v, err = marshalLoad(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v, []any{tm, "UTC", tm}) {
		t.Fatalf("Iu links: %#v", v)
	}
	for _, bad := range []string{"", "\x04\x07", "\x04\x08", "\x04\x08[\x07i\x06", "\x04\x08f\x061", "\x04\x08@\x06", "\x04\x08;\x00", "\x04\x08i\x06x"} {
		if _, err := marshalLoad([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// TestGemSpecification: the gemspec is Gem::Specification#_dump's 19 fields
// as _load reads them, with the Gem::Requirement, Gem::Dependency,
// Gem::Platform and Time encodings.
func TestGemSpecification(t *testing.T) {
	a := gemAttrs{
		Name: "demo", Version: "1.0.0", Platform: "x86_64-linux", Authors: []string{"Jane"}, Email: []string{"jane@example.org"},
		Summary: "A demo", Description: "A demo gem", Homepage: "https://example.org/demo", Licenses: []string{"MIT"},
		RequiredRubyVersion: ">= 2.7.0", RequiredRubygemsVersion: "", Date: "2026-10-27", RubygemsVersion: "3.5.0",
		Dependencies: []gemDep{{"rake", "~> 13.0, >= 13.0.1", "development"}, {"rack", ">= 2.0", "runtime"}},
		Metadata:     map[string]string{"source_code_uri": "https://git.example.org/demo", "allowed_push_host": "https://cc"},
	}
	spec, err := gemSpecification(a)
	if err != nil {
		t.Fatal(err)
	}
	if spec.class != "Gem::Specification" || len(spec.ivars) != 0 {
		t.Fatalf("spec: %+v", spec)
	}
	v, err := marshalLoad(spec.data)
	if err != nil {
		t.Fatal(err)
	}
	fields, ok := v.([]any)
	if !ok || len(fields) != 19 {
		t.Fatalf("fields: %#v", v)
	}
	req := func(reqs ...any) userObj { return userObj{"Gem::Requirement", []any{reqs}} }
	pair := func(op, v string) any { return []any{op, gemVersion(v)} }
	want := []any{
		"3.5.0", 4, "demo", gemVersion("1.0.0"), rubyTime(testDate), "A demo",
		req(pair(">=", "2.7.0")), req(pair(">=", "0")), "x86_64-linux",
		[]any{
			object{"Gem::Dependency", hash{{sym("@name"), "rake"}, {sym("@requirement"), req(pair("~>", "13.0"), pair(">=", "13.0.1"))}, {sym("@type"), sym("development")}, {sym("@prerelease"), false}}},
			object{"Gem::Dependency", hash{{sym("@name"), "rack"}, {sym("@requirement"), req(pair(">=", "2.0"))}, {sym("@type"), sym("runtime")}, {sym("@prerelease"), false}}},
		},
		"", []any{"jane@example.org"}, []any{"Jane"}, "A demo gem", "https://example.org/demo", true,
		object{"Gem::Platform", hash{{sym("@cpu"), "x86_64"}, {sym("@os"), "linux"}, {sym("@version"), nil}}},
		[]any{"MIT"}, hash{{"allowed_push_host", "https://cc"}, {"source_code_uri", "https://git.example.org/demo"}},
	}
	for i := range want {
		if !reflect.DeepEqual(fields[i], want[i]) {
			t.Errorf("field %d: %#v, want %#v", i, fields[i], want[i])
		}
	}
	// the Time: 2026-10-27 00:00:00 UTC in Time#_dump's two words
	tm := fields[4].(userDump)
	p, s := binary.LittleEndian.Uint32(tm.data[:4]), binary.LittleEndian.Uint32(tm.data[4:])
	if p>>31 != 1 || p>>30&1 != 1 || (p>>14)&0xffff != 2026-1900 || (p>>10)&0xf != 9 || (p>>5)&0x1f != 27 || p&0x1f != 0 || s != 0 {
		t.Errorf("time words %08x %08x", p, s)
	}
	if !reflect.DeepEqual(gemPlatform("java"), object{"Gem::Platform", hash{{sym("@cpu"), nil}, {sym("@os"), "java"}, {sym("@version"), nil}}}) {
		t.Errorf("java platform: %#v", gemPlatform("java"))
	}
	if gemPlatform("ruby") != "ruby" || !reflect.DeepEqual(gemPlatform("arm64-darwin-22"), object{"Gem::Platform", hash{{sym("@cpu"), "arm64"}, {sym("@os"), "darwin"}, {sym("@version"), "22"}}}) {
		t.Errorf("platforms: %#v %#v", gemPlatform("ruby"), gemPlatform("arm64-darwin-22"))
	}
	// the whole document: a 'u' object whose data is the dump above
	raw, _ := marshalDump(spec)
	if !bytes.HasPrefix(raw, []byte("\x04\x08u:\x17Gem::Specification")) {
		t.Errorf("document: %q", raw[:30])
	}
}

func TestVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0.0", 0}, {"1.0", "1.1", -1}, {"1.10", "1.9", 1}, {"1.0.0.pre.1", "1.0.0", -1}, {"1.0.0.a", "1.0.0.b", -1},
		{"1.0.0.rc1", "1.0.0.rc2", -1}, {"1.0.0.beta", "1.0.0.beta.1", -1}, {"2", "10", -1}, {"1.0.a", "1.0", -1}, {"1.a", "1", -1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("%s <=> %s = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := compareVersions(c.b, c.a); got != -c.want {
			t.Errorf("%s <=> %s = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
	if !prerelease("1.0.0.pre") || prerelease("1.0.0") {
		t.Error("prerelease")
	}
}

func TestRoutes(t *testing.T) {
	for p, want := range map[string]Route{
		"versions": {Kind: KindVersions}, "names": {Kind: KindNames}, "info/rack-test": {Kind: KindInfo, Name: "rack-test"},
		"specs.4.8.gz": {Kind: KindSpecs, File: "specs.4.8.gz"}, "latest_specs.4.8.gz": {Kind: KindSpecs, File: "latest_specs.4.8.gz"},
		"gems/rack-test-1.0.0.gem": {Kind: KindGem, Full: "rack-test-1.0.0"}, "gems/nokogiri-1.15.0-x86_64-linux.gem": {Kind: KindGem, Full: "nokogiri-1.15.0-x86_64-linux"},
		"quick/Marshal.4.8/ruby-2d-0.1.0.gemspec.rz": {Kind: KindGemspec, Full: "ruby-2d-0.1.0"},
		"api/v1/dependencies":                        {Kind: KindDependencies}, "api/v1/gems": {Kind: KindPush}, "api/v1/gems/yank": {Kind: KindYank},
	} {
		if got, err := ParseRoute(p); err != nil || got != want {
			t.Errorf("%s: %+v %v, want %+v", p, got, err, want)
		}
	}
	for _, p := range []string{"", "info/", "info/a/b", "gems/rack.gem", "gems/rack-1.0.0.tgz", "gems/-1.0.gem", "quick/Marshal.4.8/x.gemspec.rz", "specs.4.8", "api/v1/gems/x", "gems/a/b-1.0.gem"} {
		if _, err := ParseRoute(p); err == nil {
			t.Errorf("%s: no error", p)
		}
	}
	if got := candidates("a-1-2.0-java"); !reflect.DeepEqual(got, [][2]string{{"a-1", "2.0-java"}}) { // "2.0-java" is no platform
		t.Errorf("candidates: %v", got)
	}
	if got := candidates("a-1-java-2.0"); !reflect.DeepEqual(got, [][2]string{{"a-1-java", "2.0"}, {"a", "1-java-2.0"}}) {
		t.Errorf("candidates: %v", got)
	}
}
