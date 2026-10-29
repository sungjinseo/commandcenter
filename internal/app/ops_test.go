package app

import (
	"context"
	"testing"

	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
)

// A change of the http setting reaches the proxy package through the auth
// snapshot's OnSettings hook, so remote fetches pick it up.
func TestHTTPSettingsReachProxy(t *testing.T) {
	a, err := New(testConfig(t, ""), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	t.Cleanup(func() { proxy.SetConfig(proxy.Config{}) })
	if c := proxy.Current(); c.Proxy != nil || c.UserAgentSuffix != "" {
		t.Fatalf("initial %+v", c)
	}
	h := meta.DefaultHTTP()
	h.UserAgentSuffix = "site/1"
	h.Proxy = meta.HTTPProxy{Host: "proxy.corp", Port: 8080, NonProxyHosts: []string{"*.corp"}}
	if err := a.DB.SetSettingJSON(context.Background(), meta.SettingHTTP, h); err != nil {
		t.Fatal(err)
	}
	if err := a.Auth.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := proxy.Current(); c.Proxy == nil || c.Proxy.Host != "proxy.corp:8080" || c.UserAgentSuffix != "site/1" || len(c.NoProxy) != 1 {
		t.Fatalf("after reload %+v", c)
	}
}
