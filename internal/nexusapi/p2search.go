package nexusapi

import (
	"cmp"
	"context"
	"net/url"
	"strings"

	"commandcenter/internal/format/p2"
)

// maxP2Plugins bounds the bundles one p2.pluginName search opens: the name is
// in the jar, and a blob store may be a bucket.
const maxP2Plugins = 2000

// filterP2Plugins makes p2.pluginName a test on a component's files: the
// bundle's own name (p2.PluginName), a binary's file name. The bool it returns
// is set when a search asked for more bundles than maxP2Plugins.
func (h *Handler) filterP2Plugins(ctx context.Context, q url.Values, cr *criteria) *bool {
	tooMany := new(bool)
	v := strings.TrimSpace(cmp.Or(q.Get("p2.pluginName"), q.Get("attributes.p2.pluginName")))
	if v == "" {
		return tooMany
	}
	t, opened := newTerm(splitValue(v), false), 0 // words of the name, as Nexus's index has them; a phrase in double quotes
	cr.asset = append(cr.asset, func(c *comp, p string) bool {
		if c.format != "p2" {
			return false
		}
		if c.group == "binary" {
			return t.matches(c.name)
		}
		if opened++; opened > maxP2Plugins {
			*tooMany = true
			return false
		}
		return t.matches(h.p2PluginName(ctx, c.repo, p))
	})
	return tooMany
}

func (h *Handler) p2PluginName(ctx context.Context, repoName, p string) string {
	if h.Store == nil {
		return ""
	}
	a, err := h.Store.Get(ctx, repoName, p)
	if err != nil {
		return ""
	}
	f, err := h.Store.Open(a)
	if err != nil {
		return ""
	}
	defer f.Close()
	return p2.PluginName(f, a.Size)
}
