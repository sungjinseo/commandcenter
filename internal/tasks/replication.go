package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"commandcenter/internal/meta"
)

// ---- repository.pull-replication (internal/replication) ----

// TypePullReplication fetches what the source hosted repositories of
// replicating proxies hold ahead of any request.
const TypePullReplication = "repository.pull-replication"

func init() {
	types[TypePullReplication] = typeDef{name: "Pull replication", params: replicationParamsOf, run: runReplication, fields: []ParamSpec{
		{Name: "repository", Type: "repository", Help: "a proxy with pull replication enabled; empty: every one"},
		{Name: "full", Type: "bool", Help: "check every asset the source lists, not only those modified since the last complete run"}}}
}

var errNoReplication = errors.New("pull replication is not available")

type replicationParams struct {
	Repository string `json:"repository,omitempty"`
	Full       bool   `json:"full,omitempty"`
}

func replicationParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var p replicationParams
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error()}
	}
	if check && p.Repository != "" {
		rp := s.Repos.Get(p.Repository)
		switch {
		case rp == nil:
			return "", []string{fmt.Sprintf("repository %q does not exist", p.Repository)}
		case !rp.ReplicationEnabled():
			return "", []string{fmt.Sprintf("repository %q is not a proxy with pull replication enabled", p.Repository)}
		}
	}
	return encodeParams(p), nil
}

func runReplication(ctx context.Context, s *Service, t meta.Task, actor string, log *RunLog) (summary, error) {
	if s.Replication == nil {
		return summary{msg: errNoReplication.Error()}, errNoReplication
	}
	var p replicationParams
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	var names []string
	for _, rp := range s.Repos.Snapshot().Sorted {
		if (p.Repository == "" || rp.Name == p.Repository) && rp.ReplicationEnabled() {
			names = append(names, rp.Name)
		}
	}
	if p.Repository != "" && len(names) == 0 {
		err := fmt.Errorf("repository %q is not a proxy with pull replication enabled", p.Repository)
		return summary{msg: err.Error()}, err
	}
	if len(names) == 0 {
		log.Infof("no proxy has pull replication enabled")
	}
	var fetched, bytes int64
	var failed []string
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return summary{msg: fmt.Sprintf("fetched %d assets (%d bytes)", fetched, bytes)}, err
		}
		rp := s.Repos.Get(name) // as it is now: a run may take long
		switch {
		case rp == nil || !rp.ReplicationEnabled():
			log.Warnf("%s: skipped: no longer a proxy with pull replication enabled", name)
			continue
		case !rp.IsOnline():
			log.Warnf("%s: skipped: the repository is offline", name)
			continue
		case rp.Blocked:
			log.Warnf("%s: skipped: the proxy is blocked", name)
			continue
		}
		res, err := s.Replication.Run(ctx, rp, p.Full, actor, log)
		fetched, bytes = fetched+int64(res.Fetched), bytes+res.Bytes
		if err != nil {
			failed = append(failed, name)
			log.Errorf("%s: %s; %v", name, res, err)
			continue
		}
		log.Infof("%s: %s", name, res)
	}
	sum := summary{msg: fmt.Sprintf("%d proxies: fetched %d assets (%d bytes)", len(names), fetched, bytes)}
	if len(failed) > 0 {
		err := fmt.Errorf("replication failed for %d of %d proxies", len(failed), len(names))
		sum.msg += "; " + err.Error()
		return sum, err
	}
	return sum, nil
}
