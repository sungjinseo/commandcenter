package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"commandcenter/internal/meta"
)

// ---- security.purge-expired-user-tokens ----

// noParams accepts only an empty params object.
func noParams(_ *Service, raw json.RawMessage, _ bool) (string, []string) {
	var p struct{}
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error() + " (the task takes no params)"}
	}
	return "{}", nil
}

// runPurgeUserTokens deletes the user tokens older than the expiration
// setting: they no longer authenticate, and the list shows them as expired
// until then.
func runPurgeUserTokens(ctx context.Context, s *Service, _ meta.Task, _ string, log *RunLog) (summary, error) {
	var c meta.UserTokens
	if _, err := s.DB.GetSettingJSON(ctx, meta.SettingUserTokens, &c); err != nil {
		return summary{}, err
	}
	if c.ExpirationDays <= 0 {
		sum := summary{msg: "user tokens never expire (expirationDays 0): nothing to purge"}
		log.Infof("%s", sum.msg)
		return sum, nil
	}
	cutoff := s.now().Add(-time.Duration(c.ExpirationDays) * 24 * time.Hour)
	n, err := s.DB.DeleteUserTokensBefore(ctx, cutoff)
	if err != nil {
		return summary{}, err
	}
	sum := summary{msg: fmt.Sprintf("deleted %d user tokens created before %s (%d days)", n, cutoff.UTC().Format(time.RFC3339), c.ExpirationDays), removed: n}
	log.Infof("%s", sum.msg)
	return sum, nil
}
