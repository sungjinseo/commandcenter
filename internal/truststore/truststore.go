// Package truststore holds the live certificate pool of the SSL trust store
// (the trustedCertificates general setting): the system roots plus the
// admin-added certificates. Outbound TLS clients that have no CA of their
// own (remote fetches, webhooks, OIDC, LDAP, SMTP) read Pool per
// connection, so a change applies without a restart.
package truststore

import (
	"crypto/x509"
	"sync/atomic"
)

var live atomic.Pointer[x509.CertPool]

// Set installs pool as the live pool; nil means the system roots only.
func Set(pool *x509.CertPool) { live.Store(pool) }

// Pool returns the live pool, or nil for the system roots.
func Pool() *x509.CertPool { return live.Load() }

// Build returns the system roots with certs appended, or nil when certs is
// empty (the system roots as they are).
func Build(certs []*x509.Certificate) *x509.CertPool {
	if len(certs) == 0 {
		return nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	for _, c := range certs {
		pool.AddCert(c)
	}
	return pool
}
