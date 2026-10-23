package ldap

import "crypto/x509"

// NewTestClient returns a client that trusts roots (tests only).
func NewTestClient(roots *x509.CertPool) *Client { return &Client{rootCAs: roots} }
