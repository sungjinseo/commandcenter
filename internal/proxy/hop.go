package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// HopHeader lists the Command Center instances a proxied request has passed
// through (comma-separated, like Via). A repository whose remote leads back to
// the same instance would otherwise stall each request until a timeout.
const HopHeader = "X-Command-Center-Hop"

// InstanceID identifies this process in HopHeader.
var InstanceID = newInstanceID()

func newInstanceID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type hopsKey struct{}

// WithHops records the incoming HopHeader value so fetches made for the
// request extend the chain instead of replacing it.
func WithHops(ctx context.Context, hops string) context.Context {
	ids := cleanHops(hops)
	if len(ids) == 0 {
		return ctx
	}
	return context.WithValue(ctx, hopsKey{}, ids)
}

// maxHops bounds the forwarded chain.
const maxHops = 8

// cleanHops keeps the well-formed ids (16 lowercase hex characters) of a
// HopHeader value, at most the last maxHops of them.
func cleanHops(hops string) []string {
	var ids []string
	for _, id := range strings.Split(hops, ",") {
		id = strings.TrimSpace(id)
		if len(id) != 16 {
			continue
		}
		ok := true
		for i := 0; i < 16; i++ {
			if c := id[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				ok = false
				break
			}
		}
		if ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// HasHop reports whether the HopHeader value lists this instance.
func HasHop(values []string) bool {
	for _, v := range values {
		for _, id := range strings.Split(v, ",") {
			if strings.TrimSpace(id) == InstanceID {
				return true
			}
		}
	}
	return false
}

func hopValue(ctx context.Context) string {
	ids, _ := ctx.Value(hopsKey{}).([]string)
	ids = append(append([]string(nil), ids...), InstanceID)
	if len(ids) > maxHops {
		ids = ids[len(ids)-maxHops:]
	}
	return strings.Join(ids, ", ")
}
