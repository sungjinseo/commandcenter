package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/format/maven"
)

func TestDeletersWired(t *testing.T) {
	a, err := New(testConfig(t, `  - { name: cargo-hosted, format: cargo, type: hosted }
  - { name: npm-proxy, format: npm, type: proxy, remote: "https://registry.example.org" }
`), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	put, _ := http.NewRequest("PUT", ts.URL+"/repository/maven-releases/org/example/app/1.0/app-1.0.jar", strings.NewReader("jar"))
	put.SetBasicAuth("ci", "pw")
	if resp, err := http.DefaultClient.Do(put); err != nil || resp.StatusCode != 201 {
		t.Fatalf("PUT: %v %v", resp, err)
	}

	d, err := a.Deleters.For(a.Registry.Get("maven-releases"))
	if _, ok := d.(*maven.Handler); err != nil || !ok {
		t.Fatalf("maven hosted deleter: %T %v", d, err)
	}
	if d, err := a.Deleters.For(a.Registry.Get("npm-proxy")); err != nil {
		t.Fatalf("npm proxy deleter: %T %v", d, err)
	} else if _, ok := d.(*cleanup.ProxyDeleter); !ok {
		t.Fatalf("npm proxy deleter: %T", d)
	}
	c := component.Component{Format: "cargo", Name: "serde", Version: "1.0.0"}
	if _, err := a.Deleters.Delete(context.Background(), a.Registry.Get("cargo-hosted"), c, "system"); !errors.Is(err, cleanup.ErrNotSupported) {
		t.Fatalf("cargo hosted: %v", err)
	}
	if _, err := a.Deleters.For(a.Registry.Get("maven-public")); !errors.Is(err, cleanup.ErrNotSupported) {
		t.Fatalf("group: %v", err)
	}

	c = component.Component{Format: "maven", Group: "org.example", Name: "app", Version: "1.0"}
	if res, err := a.Deleters.Delete(context.Background(), a.Registry.Get("maven-releases"), c, "system"); err != nil || res != (cleanup.Result{Assets: 1, Bytes: 3}) {
		t.Fatalf("maven delete: %+v %v", res, err)
	}
	resp, err := http.Get(ts.URL + "/repository/maven-releases/org/example/app/1.0/app-1.0.jar")
	if err != nil || resp.StatusCode != 404 {
		t.Fatalf("GET after delete: %v %v", resp, err)
	}
}
