package semver

import "testing"

func TestCompare(t *testing.T) {
	ordered := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.2.0", "1.10.0", "2.0.0"}
	for i := 0; i+1 < len(ordered); i++ {
		a, _ := Parse(ordered[i])
		b, _ := Parse(ordered[i+1])
		if Compare(a, b) >= 0 || Compare(b, a) <= 0 {
			t.Errorf("%s should be < %s", ordered[i], ordered[i+1])
		}
	}
	a, _ := Parse("1.0.0+build.5")
	b, _ := Parse("v1.0.0")
	if Compare(a, b) != 0 {
		t.Error("build metadata and v prefix must be ignored")
	}
	if c, _ := Parse("v2.0.0+incompatible"); c.Major != 2 || !c.Release() {
		t.Errorf("+incompatible: %+v", c)
	}
	if _, ok := Parse("1.0"); ok {
		t.Error("1.0 is not semver")
	}
}

func TestNewerRelease(t *testing.T) {
	cases := [][3]string{
		{"", "1.0.0", "1.0.0"},
		{"1.0.0", "1.1.0", "1.1.0"},
		{"1.1.0", "1.0.0", "1.1.0"},
		{"1.0.0", "2.0.0-beta", "1.0.0"},
		{"2.0.0-beta", "1.0.0", "1.0.0"},
		{"1.0.0", "junk", "1.0.0"},
		{"junk", "1.0.0", "1.0.0"},
	}
	for _, c := range cases {
		if got := NewerRelease(c[0], c[1]); got != c[2] {
			t.Errorf("NewerRelease(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestValid(t *testing.T) {
	for _, v := range []string{"0.0.0", "1.0.0", "1.2.3-beta.1", "1.0.0-beta.1+build.5", "10.20.30-rc-1+sha.abc", "1.0.0-0", "1.0.0-0a", "1.0.0-x.0.10", "1.0.0+001", "9007199254740991.9007199254740991.9007199254740991"} {
		if !Valid(v) {
			t.Errorf("Valid(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "latest", "abc", "1.0.0 ", " 1.0.0", "=1.0.0", "v1.0.5", "1.0", "1", "01.0.0", "1.0.0-", "1.0.0-<script>", "1.0.0+", "1.0.0-a..b", "^1.0.0", "1.0.0-01", "1.0.0-beta.007", "99999999999999999999.0.0", "1.9007199254740992.0", "1.0.9007199254740992"} {
		if Valid(v) {
			t.Errorf("Valid(%q) = true, want false", v)
		}
	}
}
