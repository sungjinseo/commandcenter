package cargo

import (
	"context"
	"fmt"

	"commandcenter/internal/component"
	"commandcenter/internal/repo"
)

// DeleteComponent refuses: hosted crates are immutable and yank is the
// supported way out, so cleanup skips Cargo hosted repositories (plan 11,
// Decision 6). Cargo proxies use the generic proxy deleter instead. A
// component of another repository or format is reported as such first.
func (h *Handler) DeleteComponent(_ context.Context, rp *repo.Repository, c component.Component, _ string) (component.Result, error) {
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	return component.Result{}, fmt.Errorf("cargo %s repository %q cannot delete %s (crates are immutable; yank instead): %w",
		rp.Type, rp.Name, c.Label(), component.ErrNotSupported)
}
