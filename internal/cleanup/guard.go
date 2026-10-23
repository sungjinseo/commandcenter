package cleanup

import (
	"fmt"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
)

// ErrChanged is returned by a deleter that skipped a component because it
// changed since it was evaluated (component.ErrChanged).
var ErrChanged = component.ErrChanged

// CheckUnchanged is the deleters' skip-on-change rule. Every deleter calls it
// inside its delete transaction, before deleting, with the asset rows of c it
// is about to delete (current: what the transaction reads now). When c
// carries an evaluation (c.Evaluated, set by Evaluate), the delete must go
// ahead only if the component is still what the policy selected, so
// CheckUnchanged returns an error wrapping ErrChanged when:
//
//   - a current asset is not one the evaluation saw (a SNAPSHOT build, a
//     wheel or a sidecar added since);
//   - an asset was created or replaced after the newest one evaluated;
//   - an asset was downloaded after the newest download evaluated.
//
// Assets the evaluation saw that are gone now are not a change. Without an
// evaluation (a delete that does not come from a policy) nothing is checked.
func CheckUnchanged(c component.Component, current []meta.Asset) error {
	ev := c.Evaluated
	if ev == nil {
		return nil
	}
	for _, a := range current {
		switch {
		case !ev.Paths[a.Path]:
			return fmt.Errorf("%s: %s was added after the evaluation: %w", c.Label(), a.Path, ErrChanged)
		case a.CreatedAt.After(ev.MaxCreated), a.LastModified.After(ev.MaxModified):
			return fmt.Errorf("%s: %s was stored after the evaluation: %w", c.Label(), a.Path, ErrChanged)
		case a.LastDownloaded.After(ev.LastDownloaded):
			return fmt.Errorf("%s: %s was downloaded after the evaluation: %w", c.Label(), a.Path, ErrChanged)
		}
	}
	return nil
}
