package main

import (
	"reflect"
	"testing"
)

// The comparison finds what differs, and only that: a missing key, another
// type, another status, another value; not the volatile values or the base URL.
func TestCompare(t *testing.T) {
	base := [2]string{"http://nexus", "http://cc"}
	j := func(status int, body string) reply { return reply{status: status, body: []byte(body)} }
	for _, c := range []struct {
		name string
		st   step
		a, b reply
		want []string
	}{
		{"same", step{}, j(200, `{"a":1,"id":"x","url":"http://nexus/r"}`), j(200, `{"a":1,"id":"y","url":"http://cc/r"}`), nil},
		{"status", step{}, j(200, `{}`), j(404, `{}`), []string{"status: 404 here, 200 in Nexus (here: {}) (Nexus: {})"}},
		{"missing", step{}, j(200, `{"a":1,"b":2}`), j(200, `{"a":1}`), []string{"shape $.b: missing here (Nexus: number)"}},
		{"extra", step{}, j(200, `{"a":1}`), j(200, `{"a":1,"c":true}`), []string{"extra $.c: not in Nexus"}},
		{"type", step{}, j(200, `{"a":1}`), j(200, `{"a":"1"}`), []string{"shape $.a: string here, number in Nexus"}},
		{"value", step{}, j(200, `{"a":[1,2]}`), j(200, `{"a":[1,3]}`), []string{`value $.a[1]: "3" here, "2" in Nexus`}},
		{"length", step{}, j(200, `[1,2]`), j(200, `[1]`), []string{"value $: 1 items here, 2 in Nexus"}},
		{"not json", step{}, j(200, `{}`), j(200, `<html>`), []string{"shape: not JSON here (<html>), JSON in Nexus"}},
		{"status only", step{status: true}, j(200, `{"a":1}`), j(200, `{"a":2}`), nil},
		{"raw", step{raw: true}, j(200, `abc`), j(200, `abd`), []string{`body: differs (here "abd") (Nexus "abc")`}},
	} {
		if got := compare(c.st, c.a, c.b, base); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
