package config

import (
	"errors"
	"net"
	"strings"
)

// S3 is where the S3 API answers: requests whose Host is Host (path-style
// buckets), or <bucket>.Host when VirtualHost is set, on the main
// listener, and every request on Listen when it is set. Both empty: no S3
// API (the s3 repositories still serve reads under /repository/).
type S3 struct {
	Host        string `yaml:"host"`        // e.g. s3.cc.example.com, without a port
	VirtualHost bool   `yaml:"virtualHost"` // also <bucket>.<host>: needs a wildcard DNS name and certificate
	Listen      string `yaml:"listen"`      // e.g. ":9000"
	Region      string `yaml:"region"`      // the region clients sign for; default us-east-1
}

// Enabled reports whether the S3 API answers anywhere.
func (s S3) Enabled() bool { return s.Host != "" || s.Listen != "" }

func (s S3) validate() error {
	var errs []error
	if s.Host != "" && (strings.ContainsAny(s.Host, ":/ ") || net.ParseIP(s.Host) != nil && s.VirtualHost) {
		errs = append(errs, errors.New("s3.host must be a host name without a port or path"))
	}
	if s.VirtualHost && s.Host == "" {
		errs = append(errs, errors.New("s3.virtualHost needs s3.host"))
	}
	if s.Listen != "" {
		if _, _, err := net.SplitHostPort(s.Listen); err != nil {
			errs = append(errs, errors.New("s3.listen must be an address such as \":9000\""))
		}
	}
	for _, c := range s.Region {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			errs = append(errs, errors.New("s3.region may hold lower-case letters, digits and '-' only"))
			break
		}
	}
	return errors.Join(errs...)
}

// ValidBucketName reports whether name is a valid S3 bucket name (the
// rules for new buckets): 3 to 63 characters of a-z, 0-9, '-' and '.',
// starting and ending with a letter or digit, no "..", ".-" or "-.", not
// an IPv4 address, and none of the prefixes and suffixes AWS reserves.
func ValidBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	alnum := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }
	if !alnum(name[0]) || !alnum(name[len(name)-1]) {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, ".-") || strings.Contains(name, "-.") {
		return false
	}
	if ip := net.ParseIP(name); ip != nil {
		return false
	}
	for _, p := range []string{"xn--", "sthree-", "amzn-s3-demo-"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	for _, sfx := range []string{"-s3alias", "--ol-s3", "--x-s3", ".mrap"} {
		if strings.HasSuffix(name, sfx) {
			return false
		}
	}
	return true
}
