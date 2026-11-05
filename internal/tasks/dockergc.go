package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// TypeDockerGC removes what no tag of a Docker hosted repository reaches
// (Nexus's repository.docker.gc, "Docker - Delete unused manifests and images").
const TypeDockerGC = "docker.gc"

// DefaultDeployOffset is how many hours a manifest must have gone unused,
// as Nexus's deployOffset: a push of an index stores its manifests first.
const DefaultDeployOffset = 24

// DockerCollector is docker.Handler.CollectUnused.
type DockerCollector interface {
	CollectUnused(ctx context.Context, rp *repo.Repository, before time.Time, preview bool, actor string, logf func(string, ...any)) (int64, error)
}

var dockerGCFields = []ParamSpec{
	{Name: "repository", Type: "repository", Help: `a Docker hosted repository; empty or "*": every one`},
	{Name: "deployOffset", Type: "int", Default: DefaultDeployOffset, Help: "hours a manifest must have gone unstored and unpulled"},
	{Name: "preview", Type: "bool", Default: false, Help: "only log what would be removed"},
}

type dockerGCParams struct {
	Repository   string `json:"repository,omitempty"`
	DeployOffset int    `json:"deployOffset"`
	Preview      bool   `json:"preview,omitempty"`
}

func dockerGCParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var in struct {
		Repository   string `json:"repository"`
		DeployOffset *int   `json:"deployOffset"`
		Preview      bool   `json:"preview"`
	}
	if err := decodeParams(raw, &in); err != nil {
		return "", []string{err.Error()}
	}
	p := dockerGCParams{Repository: in.Repository, DeployOffset: DefaultDeployOffset, Preview: in.Preview}
	if p.Repository == "*" {
		p.Repository = ""
	}
	if in.DeployOffset != nil {
		p.DeployOffset = *in.DeployOffset
	}
	if p.DeployOffset < 0 {
		return "", []string{"params.deployOffset must be at least 0"}
	}
	if check && p.Repository != "" {
		if rp := s.Repos.Get(p.Repository); rp == nil {
			return "", []string{fmt.Sprintf("repository %q does not exist", p.Repository)}
		} else if rp.Format != "docker" || rp.Type != "hosted" {
			return "", []string{fmt.Sprintf("repository %q is %s %s: the task takes a Docker hosted repository", p.Repository, rp.Format, rp.Type)}
		}
	}
	return encodeParams(p), nil
}

func runDockerGC(ctx context.Context, s *Service, t meta.Task, actor string, log *RunLog) (summary, error) {
	var p dockerGCParams
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	if s.Docker == nil {
		return summary{msg: "docker is not available"}, errors.New("docker is not available")
	}
	var repos []*repo.Repository
	if p.Repository != "" {
		rp := s.Repos.Get(p.Repository)
		if rp == nil || rp.Format != "docker" || rp.Type != "hosted" {
			return summary{}, fmt.Errorf("repository %q is no longer a Docker hosted repository: edit or delete this task", p.Repository)
		}
		repos = append(repos, rp)
	} else {
		for _, rp := range s.Repos.Snapshot().Sorted {
			if rp.Format == "docker" && rp.Type == "hosted" {
				repos = append(repos, rp)
			}
		}
	}
	if s.Metrics != nil { // the pulls the offset looks at
		s.flushOthers(ctx)
		if err := s.Metrics.Flush(ctx); err != nil {
			return summary{}, fmt.Errorf("could not write the pending download counts: %w", err)
		}
	}
	before := s.now().Add(-time.Duration(p.DeployOffset) * time.Hour)
	verb := "removed"
	if p.Preview {
		verb = "would remove"
	}
	var removed int64
	for _, rp := range repos {
		n, err := s.Docker.CollectUnused(ctx, rp, before, p.Preview, actor, log.Infof)
		removed += n
		if err != nil {
			return summary{msg: fmt.Sprintf("%s %d unused manifests", verb, removed), removed: removed}, fmt.Errorf("%s: %w", rp.Name, err)
		}
	}
	return summary{msg: fmt.Sprintf("%s %d unused manifests in %d repositories", verb, removed, len(repos)), removed: removed}, nil
}
