package tasks

import (
	"context"
	"fmt"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
)

// ---- security.purge-api-keys ----

// TypePurgeAPIKeys deletes the API tokens and user tokens whose owner no
// longer resolves (Nexus security.purge-api-keys). Such tokens already fail
// to authenticate; this is housekeeping.
const TypePurgeAPIKeys = "security.purge-api-keys"

func init() {
	types[TypePurgeAPIKeys] = typeDef{name: "Admin - Delete orphaned API tokens", params: noParams, run: runPurgeAPIKeys, fields: []ParamSpec{}}
}

// runPurgeAPIKeys asks OwnerGone about every token owner and deletes the
// tokens of those that are gone, created before the run began (a user
// re-created meanwhile keeps a token issued since). An owner that cannot
// be checked keeps its tokens.
func runPurgeAPIKeys(ctx context.Context, s *Service, _ meta.Task, _ string, log *RunLog) (summary, error) {
	if s.OwnerGone == nil {
		return summary{}, fmt.Errorf("the token owner check is not available")
	}
	start := s.now()
	owners, err := s.DB.TokenOwners(ctx)
	if err != nil {
		return summary{}, err
	}
	var removed int64
	var gone, unsure int
	for _, o := range owners {
		g, err := s.OwnerGone(ctx, o)
		if err != nil {
			unsure++
			log.Warnf("kept the tokens of %s: %s", o, logsafe.Err(err))
			continue
		}
		if !g {
			continue
		}
		n, err := s.DB.DeleteOwnerTokensBefore(ctx, o, start)
		if err != nil {
			return summary{}, err
		}
		gone++
		removed += n
		log.Infof("deleted %d tokens of %s, who no longer resolves", n, o)
	}
	sum := summary{msg: fmt.Sprintf("deleted %d tokens of %d owners that no longer resolve (%d owners checked, %d could not be checked)", removed, gone, len(owners), unsure), removed: removed}
	log.Infof("%s", sum.msg)
	return sum, nil
}
