package meta

import "testing"

func TestRUTTrustsAndValidate(t *testing.T) {
	c := RUT{Header: "REMOTE_USER", TrustedProxies: []string{"10.0.0.0/8", "::1/128"}}
	for addr, want := range map[string]bool{
		"10.2.3.4:5": true, "10.2.3.4": true, "[::1]:80": true, "[::ffff:10.0.0.1]:1": true,
		"11.0.0.1:1": false, "garbage": false, "": false,
	} {
		if got := c.Trusts(addr); got != want {
			t.Errorf("Trusts(%q) = %v", addr, got)
		}
	}
	if err := ValidateRUT(c); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []RUT{
		{Header: "X User", TrustedProxies: []string{"10.0.0.0/8"}},
		{Header: "X-User:", TrustedProxies: []string{"10.0.0.0/8"}},
		{Header: "X-User", TrustedProxies: []string{"10.0.0.1"}},
		{Header: "X-User", GroupMappings: []GroupMapping{{Group: "g"}}},
	} {
		if ValidateRUT(bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if (RUT{Header: "X-User"}).Complete() {
		t.Fatal("complete without a trusted proxy")
	}
}
