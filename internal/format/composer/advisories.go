package composer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
)

const (
	// maxAdvisoryForm bounds the form of a POST to the advisories API. Composer
	// 2.11 sends 500 names at a time, about 30 KiB.
	maxAdvisoryForm = 256 << 10
	// advisoryLookups bounds the proxies asked at once who has a package:
	// Composer gives up on the API after 10 seconds.
	advisoryLookups = 16
)

// advisoryConfig is the security-advisories key of the packages.json of a
// repository served at base: Composer POSTs the names it wants to know about to
// the API, for `composer audit` and for the versions `composer update` considers
// (audit.block-insecure), and reads no advisories out of the package metadata.
// "metadata": true (Packagist's) would have it read p2/<name>.json at every
// repository for every name it knows: a client with a second repository would
// ask the proxy about the names it has from there, and a GET cannot be told from
// one that wants the public package of that name, so the upstream would hear them.
func advisoryConfig(base string) map[string]any {
	return map[string]any{"metadata": false, "api-url": base + "/" + advisoriesPath}
}

// advisories answers POST api/security-advisories/ with Composer's form
// (packages[]=<vendor>/<package>, or packages[<n>]) as an upstream does:
// {"advisories": {name: [advisory, ...]}}. Each proxy is asked only about the
// names it may hear of (advisoryOwner). The answer is not kept, as a stale one
// would hide a new advisory. An upstream that fails or answers something else
// fails the request, never an empty list: `composer audit` then stops (exit 100)
// and `composer update` warns (Composer's policy.ignore-unreachable). A hosted
// repository and a group without a proxy have no data and answer an empty list
// (their packages.json does not offer it).
func (h *Handler) advisories(w http.ResponseWriter, r *http.Request, rp *repo.Repository) error {
	if r.Method != http.MethodPost {
		return repo.MethodNotAllowed(http.MethodPost, "method %s not allowed", r.Method)
	}
	names, err := advisoryNames(w, r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	asks, err := h.advisoryAsks(ctx, rp, names)
	if err != nil {
		return err
	}
	return repo.ServeStream(w, r, ctJSON, time.Time{}, func(w io.Writer) error {
		io.WriteString(w, `{"advisories":{`)
		written := 0
		for _, a := range asks {
			if err := h.writeUpstreamAdvisories(ctx, w, a, &written); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, "}}")
		return err
	})
}

// advisoryNames reads the names of a POST, lower-cased, sorted and each once.
// What cannot be a package name is left out and goes nowhere; a form without
// the parameter is a 400, as on Packagist.
func advisoryNames(w http.ResponseWriter, r *http.Request) ([]string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdvisoryForm)
	if err := r.ParseForm(); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) || errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, err
		}
		return nil, repo.Errorf(http.StatusBadRequest, "invalid form: %v", err)
	}
	found := false
	var names []string
	for k, vs := range r.PostForm {
		if rest, ok := strings.CutPrefix(k, "packages["); !ok || !strings.HasSuffix(rest, "]") {
			continue
		}
		found = true
		for _, v := range vs {
			vendor, pkg, ok := strings.Cut(v, "/")
			if ok && segRE.MatchString(vendor) && segRE.MatchString(pkg) {
				names = append(names, strings.ToLower(v))
			}
		}
	}
	if !found {
		return nil, repo.Errorf(http.StatusBadRequest, "missing array of package names as the packages parameter")
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// advisoryAsk is one question to one upstream.
type advisoryAsk struct {
	rp    *repo.Repository
	url   string
	names []string
}

// proxyMembers counts rp's proxies, nested groups included: those a request
// reaches, and those an offline member keeps it from.
func proxyMembers(rp *repo.Repository) (online, offline int) {
	for _, m := range rp.MemberRepos {
		on, off := 0, 0
		switch m.Type {
		case "proxy":
			on = 1
		case "group":
			on, off = proxyMembers(m)
		}
		if !m.IsOnline() {
			on, off = 0, on+off
		}
		online += on
		offline += off
	}
	return online, offline
}

// advisoryAsks finds what to ask rp's proxies. A proxy without the API, or
// whose routing rule refuses the path, has nothing to say about its names. A
// proxy member that is offline cannot answer for a name no hosted repository
// has: that is a 503, as such a member is in a read of the package, not "no
// advisories".
func (h *Handler) advisoryAsks(ctx context.Context, rp *repo.Repository, names []string) ([]advisoryAsk, error) {
	if len(names) == 0 {
		return nil, nil
	}
	held, err := h.Store.DB.HostedPackages(ctx, "composer", names)
	if err != nil {
		return nil, err
	}
	if _, offline := proxyMembers(rp); offline > 0 && slices.ContainsFunc(names, func(n string) bool { return len(held[n]) == 0 }) {
		return nil, repo.Errorf(http.StatusServiceUnavailable, "a proxy member of group %q is offline", rp.Name)
	}
	leaves := resolve.Leaves(rp)
	lastProxy := -1
	for i, m := range leaves {
		if m.Type == "proxy" {
			lastProxy = i
		}
	}
	if lastProxy < 0 {
		return nil, nil
	}
	owners := make([]*repo.Repository, len(names))
	var g errgroup.Group
	g.SetLimit(advisoryLookups)
	for i, name := range names {
		g.Go(func() (err error) {
			owners[i], err = h.advisoryOwner(ctx, rp, leaves, lastProxy, name, held[name])
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	var asks []advisoryAsk
	for _, m := range leaves {
		var mine []string
		for i, owner := range owners {
			if owner == m {
				mine = append(mine, names[i])
			}
		}
		if len(mine) == 0 {
			continue
		}
		up, _, err := h.upstreamFor(ctx, m)
		if err != nil {
			return nil, memberFailure(rp, m, err)
		}
		if up.advisoryURL != "" && h.routeAllowed(m, advisoriesPath) {
			asks = append(asks, advisoryAsk{m, up.advisoryURL, mine})
		}
	}
	return asks, nil
}

// advisoryOwner is the proxy that may be asked about name, or nil: the
// private-name rule. A POST names packages, and Composer sends every name it
// has to every repository that offers the API, wherever the package came from,
// so this decides what leaves for an upstream:
//
//   - Members are walked in order, as a read does (owner), and the first that
//     has the package answers for it: a hosted one means nobody is asked.
//   - A name any hosted repository of this server holds (held), a member or not,
//     is private: no upstream hears of it, not even a GET to learn if it has the
//     package. Only a proxy that already holds the package's metadata, so a read
//     through it found the same name upstream, may be asked about it.
//   - A proxy's routing rule refusing the metadata path keeps the name from it.
//   - A proxy with another behind it is first asked if it has the package, as a
//     read does. The last one is not: that question would name the package as
//     well, and it has no one to pass it on to.
//
// A name this server does not know (a package of a VCS repository) cannot be
// told from a public one and goes, as it would to Packagist.
func (h *Handler) advisoryOwner(ctx context.Context, rp *repo.Repository, leaves []*repo.Repository, lastProxy int, name string, held []string) (*repo.Repository, error) {
	key := metadataPath(name, false)
	for i, m := range leaves {
		if m.Type != "proxy" {
			if slices.Contains(held, m.Name) {
				return nil, nil
			}
			continue
		}
		if !h.routeAllowed(m, key) {
			continue
		}
		if len(held) > 0 {
			switch _, err := h.Store.Get(ctx, m.Name, key); {
			case err == nil:
				return m, nil
			case !errors.Is(err, repo.ErrNotFound):
				return nil, err
			}
			continue
		}
		if i == lastProxy {
			return m, nil
		}
		has, err := h.has(ctx, m, name)
		if err != nil {
			return nil, memberFailure(rp, m, err)
		}
		if has {
			return m, nil
		}
	}
	return nil, nil
}

// routeAllowed reports whether rp's routing rule lets path go to its remote.
func (h *Handler) routeAllowed(rp *repo.Repository, path string) bool {
	return h.Fetcher.RouteAllowed == nil || h.Fetcher.RouteAllowed(rp, "/"+path)
}

// writeUpstreamAdvisories asks a's upstream and writes the entries of its answer
// to w, one package at a time, after the written before them.
func (h *Handler) writeUpstreamAdvisories(ctx context.Context, w io.Writer, a advisoryAsk, written *int) error {
	form := url.Values{"packages[]": a.names}.Encode()
	header := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Accept": {ctJSON}}
	resp, err := h.Fetcher.Post(ctx, a.rp, a.url, header, form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return proxy.ErrRateLimited
	default:
		return repo.Errorf(http.StatusBadGateway, "the security advisories API of repository %q answered %d", a.rp.Name, resp.StatusCode)
	}
	if err := copyAdvisories(w, io.LimitReader(resp.Body, maxDoc), written); err != nil {
		return repo.Errorf(http.StatusBadGateway, "the security advisories API of repository %q gave no usable answer: %v", a.rp.Name, err)
	}
	return nil
}

// copyAdvisories copies the entries of an upstream's {"advisories": {...}} to w.
// An answer without that key is an error, not an empty list. PHP writes an
// empty object as an array.
func copyAdvisories(w io.Writer, src io.Reader, written *int) error {
	dec := json.NewDecoder(src)
	if open, err := dec.Token(); err != nil {
		return err
	} else if open != json.Delim('{') {
		return fmt.Errorf("the answer is %v, not an object", open)
	}
	found := false
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		if key != "advisories" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
			continue
		}
		found = true
		open, err := dec.Token()
		if err != nil {
			return err
		}
		if open != json.Delim('{') && open != json.Delim('[') {
			return fmt.Errorf("advisories is %v", open)
		}
		for dec.More() {
			if open == json.Delim('[') {
				var skip json.RawMessage
				if err := dec.Decode(&skip); err != nil {
					return err
				}
				continue
			}
			name, err := dec.Token()
			if err != nil {
				return err
			}
			var list json.RawMessage
			if err := dec.Decode(&list); err != nil {
				return err
			}
			if *written++; *written > 1 {
				io.WriteString(w, ",")
			}
			k, err := json.Marshal(name)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "%s:%s", k, list); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if !found {
		return errors.New("no advisories key")
	}
	return nil
}
