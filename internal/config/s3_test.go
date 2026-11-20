package config

import (
	"strings"
	"testing"
)

func TestValidBucketName(t *testing.T) {
	for name, want := range map[string]bool{"abc": true, "my-bucket.v2": true, "a1": false, "Abc": false, "my_bucket": false,
		"-abc": false, "abc-": false, "a..b": false, "a.-b": false, "192.168.0.1": false, "xn--abc": false,
		strings.Repeat("a", 63): true, strings.Repeat("a", 64): false} {
		if got := ValidBucketName(name); got != want {
			t.Errorf("%q: %v", name, got)
		}
	}
}

func TestS3Repositories(t *testing.T) {
	ok := []Repository{{Name: "my-bucket", Format: "s3", Type: "hosted"}}
	if err := ValidateRepositories(ok); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Repository{{Name: "My_Bucket", Format: "s3", Type: "hosted"},
		{Name: "proxied", Format: "s3", Type: "proxy", Remote: "http://x/"},
		{Name: "grouped", Format: "s3", Type: "group"}} {
		if err := ValidateRepositories([]Repository{r}); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
	// The bucket rule is the s3 format's alone.
	if err := ValidateRepositories([]Repository{{Name: "My_Raw", Format: "raw", Type: "hosted"}}); err != nil {
		t.Fatal(err)
	}
}

func TestS3Settings(t *testing.T) {
	c, err := Parse([]byte("s3:\n  host: S3.Example.com\n  listen: \":9000\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.S3.Host != "s3.example.com" || c.S3.Region != "us-east-1" || !c.S3.Enabled() {
		t.Fatalf("%+v", c.S3)
	}
	for _, bad := range []string{"s3:\n  virtualHost: true\n", "s3:\n  host: s3.example.com:9000\n", "s3:\n  listen: nine\n", "s3:\n  region: EU\n"} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	c, err = Parse([]byte("listen: \":8081\"\n"))
	if err != nil || c.S3.Enabled() {
		t.Fatalf("enabled by default: %v", err)
	}
}
