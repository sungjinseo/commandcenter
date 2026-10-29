package vercmp

import "testing"

// Each row is ordered: a < b in the ecosystem; equal pairs say so.
func TestCompare(t *testing.T) {
	for _, c := range []struct {
		eco, a, b string
		want      int
	}{
		// semver: npm, crates.io, Go (with its "v")
		{"npm", "1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"npm", "1.0.0-alpha.beta", "1.0.0-beta", -1},
		{"npm", "1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"npm", "1.0.0-rc.1", "1.0.0", -1},
		{"npm", "1.9.0", "1.10.0", -1},
		{"npm", "1.0.0+build.1", "1.0.0", 0},
		{"crates.io", "0.9.9", "0.10.0", -1},
		{"Go", "v1.2.3", "1.2.3", 0},
		{"Go", "v0.0.0-20230101000000-abcdefabcdef", "v0.0.1", -1},
		{"Go", "v2.0.0+incompatible", "v2.0.1", -1},

		// PEP 440
		{"PyPI", "1.0.dev0", "1.0a1", -1},
		{"PyPI", "1.0a1", "1.0b1", -1},
		{"PyPI", "1.0b2", "1.0rc1", -1},
		{"PyPI", "1.0rc1", "1.0", -1},
		{"PyPI", "1.0", "1.0.post1", -1},
		{"PyPI", "1.0.post1.dev0", "1.0.post1", -1},
		{"PyPI", "1.0", "1.0+local.1", -1},
		{"PyPI", "1.0+abc", "1.0+1", -1},
		{"PyPI", "1.0", "1.0.0", 0},
		{"PyPI", "1.0-1", "1.0.post1", 0},
		{"PyPI", "1.0alpha1", "1.0a1", 0},
		{"PyPI", "1.0c1", "1.0rc1", 0},
		{"PyPI", "1!0.1", "2.0", 1},
		{"PyPI", "2.0", "10.0", -1},
		{"PyPI", "1.0a1.dev1", "1.0a1", -1},

		// Maven ComparableVersion
		{"Maven", "1.0-alpha-1", "1.0-beta-1", -1},
		{"Maven", "1.0-rc1", "1.0", -1},
		{"Maven", "1.0-SNAPSHOT", "1.0", -1},
		{"Maven", "1.0", "1.0.0", 0},
		{"Maven", "1.0", "1.0-sp1", -1},
		{"Maven", "1.9", "1.10", -1},
		{"Maven", "2.13.4.2", "2.13.5", -1},

		// NuGet
		{"NuGet", "1.0", "1.0.0.0", 0},
		{"NuGet", "1.0.0-beta", "1.0.0", -1},
		{"NuGet", "1.0.0-Beta.2", "1.0.0-beta.10", -1},
		{"NuGet", "1.0.0.1", "1.0.1", -1},
		{"NuGet", "1.0.0-1", "1.0.0-alpha", -1},
		{"NuGet", "1.0.0+meta", "1.0.0", 0},

		// RubyGems
		{"RubyGems", "1.0.a", "1.0", -1},
		{"RubyGems", "1.0.0.pre.1", "1.0.0", -1},
		{"RubyGems", "1.0.0-rc1", "1.0.0", -1},
		{"RubyGems", "1.0", "1.0.0", 0},
		{"RubyGems", "1.0.b1", "1.0.a2", 1},
		{"RubyGems", "5.2.4.3", "5.2.5", -1},

		// Packagist (Composer)
		{"Packagist", "1.0.0-dev", "1.0.0-alpha1", -1},
		{"Packagist", "1.0.0-alpha1", "1.0.0-beta1", -1},
		{"Packagist", "1.0.0-beta1", "1.0.0-RC1", -1},
		{"Packagist", "1.0.0-RC1", "1.0.0", -1},
		{"Packagist", "1.0.0", "1.0.0-p1", -1},
		{"Packagist", "v1.2", "1.2.0.0", 0},
		{"Packagist", "1.2.3", "1.10.0", -1},
		{"Packagist", "1.0.0-beta", "1.0.0-beta2", -1},

		// CRAN and the generic order
		{"CRAN", "1.2-3", "1.2-10", -1},
		{"CRAN", "1.2.3", "1.2-3", 0},
		{"CRAN", "0.9", "0.10", -1},
		{"ConanCenter", "1.2.11", "1.2.13", -1},
		{"ConanCenter", "3.0.0", "3.0", 0},
	} {
		if got := Compare(c.eco, c.a, c.b); got != c.want {
			t.Errorf("Compare(%s, %q, %q) = %d, want %d", c.eco, c.a, c.b, got, c.want)
		}
		if got := Compare(c.eco, c.b, c.a); got != -c.want {
			t.Errorf("Compare(%s, %q, %q) = %d, want %d (reversed)", c.eco, c.b, c.a, got, -c.want)
		}
	}
}
