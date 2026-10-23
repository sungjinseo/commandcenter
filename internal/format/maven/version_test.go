package maven

import (
	"sort"
	"strings"
	"testing"
)

func TestCompareOrdering(t *testing.T) {
	want := []string{"1.0-alpha-1", "1.0-beta", "1.0-M1", "1.0-RC1", "1.0-SNAPSHOT", "1.0", "1.0-sp1", "1.0.1", "1.2", "1.10", "2.0"}
	// reverse the input first so sorting does real work
	rev := append([]string(nil), want...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	sort.Slice(rev, func(i, j int) bool { return Compare(rev[i], rev[j]) < 0 })
	if strings.Join(rev, " ") != strings.Join(want, " ") {
		t.Fatalf("got  %v\nwant %v", rev, want)
	}
}

func TestCompareEquivalents(t *testing.T) {
	for _, pair := range [][2]string{{"1.0", "1.0.0"}, {"1.0", "1.0-final"}, {"1.0-ga", "1"}, {"1.0-cr1", "1.0-rc1"},
		{"1.0-SNAPSHOT", "1.0.0-SNAPSHOT"}, {"1.0-beta", "1.0.0-beta"}, {"1.0.0", "1"}} {
		if c := Compare(pair[0], pair[1]); c != 0 {
			t.Errorf("Compare(%q, %q) = %d, want 0", pair[0], pair[1], c)
		}
	}
	if Compare("10000000000000000000001", "9") <= 0 {
		t.Error("big numbers must compare numerically")
	}
}

func TestCompareLess(t *testing.T) {
	for _, pair := range [][2]string{{"1.0.0-rc1", "1.0-rc2"}, {"1.0.0", "1.0-sp1"}, {"0-alpha", "0.1"}, {"0.0.1", "0.1"}} {
		if c := Compare(pair[0], pair[1]); c >= 0 {
			t.Errorf("Compare(%q, %q) = %d, want < 0", pair[0], pair[1], c)
		}
	}
}
