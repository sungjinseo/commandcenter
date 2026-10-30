package proxy

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"

	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

// repoContext marks ctx as a request for rp: its remote, whose egress
// policy applies, and its connection settings (egress.RoundTrip).
func repoContext(ctx context.Context, rp *repo.Repository) context.Context {
	ctx = withRemote(ctx, rp.RemoteURL)
	if c := rp.Connection; c != nil {
		ctx = context.WithValue(ctx, connKey{}, c)
	}
	return ctx
}

type connKey struct{}

// connectionOf returns the connection settings repoContext put in ctx, or nil.
func connectionOf(ctx context.Context) *config.Connection {
	c, _ := ctx.Value(connKey{}).(*config.Connection)
	return c
}

// client is f.Client, with rp's cookie jar when its connection keeps
// cookies. The jar holds the cookies of the remote's own scheme and host
// only, so they never go to another host a redirect leads to, as the
// remote's credentials do not (sendsCredentials).
// ponytail: one jar per repository incarnation and node, never pruned.
func (f *Fetcher) client(rp *repo.Repository) *http.Client {
	if c := rp.Connection; c == nil || !c.EnableCookies {
		return f.Client
	}
	f.blockMu.Lock()
	defer f.blockMu.Unlock()
	key := stateKey(rp)
	jar := f.jars[key]
	if jar == nil {
		j, _ := cookiejar.New(nil) // never fails without options
		jar = remoteJar{j, rp}
		if f.jars == nil {
			f.jars = map[string]http.CookieJar{}
		}
		f.jars[key] = jar
	}
	c := *f.Client
	c.Jar = jar
	return &c
}

type remoteJar struct {
	http.CookieJar
	rp *repo.Repository
}

func (j remoteJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if sendsCredentials(j.rp, u) {
		j.CookieJar.SetCookies(u, cookies)
	}
}

func (j remoteJar) Cookies(u *url.URL) []*http.Cookie {
	if !sendsCredentials(j.rp, u) {
		return nil
	}
	return j.CookieJar.Cookies(u)
}
