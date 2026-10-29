package composer

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"1.2.3":           "1.2.3.0",
		"v1.2.3":          "1.2.3.0",
		"1.2":             "1.2.0.0",
		"1":               "1.0.0.0",
		"1.2.3.4":         "1.2.3.4",
		"1.0.0-beta1":     "1.0.0.0-beta1",
		"1.0.0-beta.2":    "1.0.0.0-beta2",
		"1.0.0-b3":        "1.0.0.0-beta3",
		"1.0.0-RC1":       "1.0.0.0-RC1",
		"1.0.0-rc1":       "1.0.0.0-RC1",
		"1.0.0-alpha":     "1.0.0.0-alpha",
		"1.0.0a1":         "1.0.0.0-alpha1",
		"1.0.0-p1":        "1.0.0.0-patch1",
		"1.0.0-pl2":       "1.0.0.0-patch2",
		"1.0.0-stable":    "1.0.0.0",
		"1.0-dev":         "1.0.0.0-dev",
		"2.0.0-beta1-dev": "2.0.0.0-beta1-dev",
		"dev-main":        "dev-main",
		"dev-feature/x":   "dev-feature/x",
		"DEV-Main":        "dev-Main",
		"master":          "9999999-dev",
		"trunk":           "9999999-dev",
		"1.x-dev":         "1.9999999.9999999.9999999-dev",
		"1.0.x-dev":       "1.0.9999999.9999999-dev",
		"2.*-dev":         "2.9999999.9999999.9999999-dev",
		"v3.x-dev":        "3.9999999.9999999.9999999-dev",
		" 1.0.0 ":         "1.0.0.0",
	}
	for in, want := range cases {
		got, ok := Normalize(in)
		if !ok || got != want {
			t.Errorf("Normalize(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "x", "1.0.0-nope", "latest", "1.2.3.4.5", "feature", "1.0.0 as 2.0"} {
		if got, ok := Normalize(bad); ok {
			t.Errorf("Normalize(%q) = %q, want invalid", bad, got)
		}
	}
	for v, dev := range map[string]bool{"dev-main": true, "9999999-dev": true, "1.0.0.0-dev": true, "1.0.0.0": false, "1.0.0.0-beta1": false} {
		if IsDev(v) != dev {
			t.Errorf("IsDev(%q) = %v", v, !dev)
		}
	}
}

func TestParseRoute(t *testing.T) {
	cases := map[string]Route{
		"packages.json":                                  {Kind: KindRoot},
		"search.json":                                    {Kind: KindSearch},
		"p2/Acme/Demo.json":                              {Kind: KindMetadata, Name: "acme/demo"},
		"p2/acme/demo~dev.json":                          {Kind: KindMetadata, Name: "acme/demo", Dev: true},
		"dist/acme/demo/1.0.0.0.zip":                     {Kind: KindDist, Name: "acme/demo", Version: "1.0.0.0"},
		"dist/acme/demo/dev-main.zip":                    {Kind: KindDist, Name: "acme/demo", Version: "dev-main"},
		"dist/acme/demo/1.0.0.0/" + sha40 + ".zip":       {Kind: KindDist, Name: "acme/demo", Version: "1.0.0.0", Ref: sha40},
		"dist/acme/demo/dev-feature/x/" + sha40 + ".zip": {Kind: KindDist, Name: "acme/demo", Version: "dev-feature/x", Ref: sha40},
		"packages/upload/acme/demo/v1.0":                 {Kind: KindUpload, Name: "acme/demo", Version: "v1.0"},
	}
	for path, want := range cases {
		got, err := ParseRoute(path)
		if err != nil || got != want {
			t.Errorf("ParseRoute(%q) = %+v %v, want %+v", path, got, err, want)
		}
	}
	for _, bad := range []string{"", "p2/demo.json", "p2/acme/demo", "p2/../x.json", "dist/acme/demo.zip", "dist/acme/demo/1.0/x.zip",
		"dist/acme/demo/../" + sha40 + ".zip", "dist/acme/demo/1.0/" + sha40, "packages/upload/acme/demo", "packages/upload/acme/demo/1/2", "p/acme/demo.json", "index.json"} {
		if rt, err := ParseRoute(bad); err == nil {
			t.Errorf("ParseRoute(%q) = %+v, want an error", bad, rt)
		}
	}
}

const sha40 = "0123456789abcdef0123456789abcdef01234567"
