package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/transfer"
)

// Repository export and import (internal/transfer): the type ids of Nexus
// Repository Pro's tasks.
const (
	TypeExport = "repository.export"
	TypeImport = "repository.import"
)

// maxImportRepos bounds an import's repository mapping.
const maxImportRepos = 1000

var exportFields = []ParamSpec{
	{Name: "repositories", Type: "repositories", Help: "empty or \"*\": every hosted repository (and proxy with includeProxies)"},
	{Name: "destination", Type: "string", Required: true, Help: "a directory under the export root; the export goes to <destination>/<task id>"},
	{Name: "includeProxies", Type: "bool", Default: false, Help: "export proxies' caches too (they refill themselves)"},
	{Name: "since", Type: "string", Help: "RFC 3339 time: only the files changed after it (a delta export)"},
	{Name: "hardLinks", Type: "bool", Default: true, Help: "hard-link blobs of a file blob store on the same filesystem instead of copying them"},
}

var importFields = []ParamSpec{
	{Name: "source", Type: "string", Required: true, Help: "the export's directory under the export root, <destination>/<export id>"},
	{Name: "repositories", Type: "mapping", Help: "exported name → repository; empty: every exported repository under its own name"},
	{Name: "createMissing", Type: "bool", Default: false, Help: "create a missing repository from the exported definition"},
	{Name: "overwrite", Type: "bool", Default: false, Help: "replace files whose content differs; otherwise they are kept and reported"},
}

// exportParams are repository.export's; hardLinks defaults to true.
type exportParams struct {
	Repositories   names  `json:"repositories"`
	Destination    string `json:"destination"`
	IncludeProxies bool   `json:"includeProxies"`
	Since          string `json:"since,omitempty"`
	HardLinks      *bool  `json:"hardLinks"`
}

// names is a repository list that also takes the string "*".
type names []string

func (n *names) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		if s != "*" {
			return errors.New(`repositories is a list of names, or "*"`)
		}
		*n = names{"*"}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(n))
}

type importParams struct {
	Source        string            `json:"source"`
	Repositories  map[string]string `json:"repositories"`
	CreateMissing bool              `json:"createMissing"`
	Overwrite     bool              `json:"overwrite"`
}

// transferReady answers why export and import cannot run, or nil.
func (s *Service) transferReady() error {
	if s.Transfer == nil {
		return errors.New("repository export and import are not available")
	}
	if s.Transfer.Root == "" {
		return transfer.ErrNoRoot
	}
	return nil
}

func exportParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var p exportParams
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error()}
	}
	var errs []string
	if p.HardLinks == nil {
		t := true
		p.HardLinks = &t
	}
	if p.Since != "" {
		t, err := time.Parse(time.RFC3339Nano, p.Since)
		if err != nil {
			errs = append(errs, "params.since must be an RFC 3339 time, such as 2026-01-31T00:00:00Z")
		} else {
			p.Since = t.UTC().Format(time.RFC3339Nano)
		}
	}
	slices.Sort(p.Repositories)
	p.Repositories = slices.Compact(p.Repositories)
	if slices.Contains(p.Repositories, "*") || len(p.Repositories) == 0 {
		p.Repositories = names{"*"}
	}
	if len(p.Repositories) > maxCleanupRepos {
		errs = append(errs, fmt.Sprintf("params.repositories lists at most %d repositories", maxCleanupRepos))
	}
	if check {
		if err := s.transferReady(); err != nil {
			return "", []string{err.Error()}
		}
		if err := s.Transfer.CheckDir(p.Destination); err != nil {
			errs = append(errs, "params.destination: "+err.Error())
		}
		_, rerrs := s.Transfer.Select(p.Repositories, p.IncludeProxies)
		errs = append(errs, rerrs...)
	}
	return encodeParams(p), errs
}

func importParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var p importParams
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error()}
	}
	var errs []string
	if len(p.Repositories) > maxImportRepos {
		errs = append(errs, fmt.Sprintf("params.repositories maps at most %d repositories", maxImportRepos))
	}
	for from, to := range p.Repositories {
		if from == "" {
			errs = append(errs, "params.repositories: an exported name is empty")
		}
		if to != "" {
			if err := repo.CheckName("repository", to); err != nil {
				errs = append(errs, "params.repositories: "+err.Error())
			}
		}
	}
	slices.Sort(errs)
	if check {
		if err := s.transferReady(); err != nil {
			return "", []string{err.Error()}
		}
		if err := s.Transfer.CheckDir(p.Source); err != nil {
			errs = append(errs, "params.source: "+err.Error())
		}
	}
	return encodeParams(p), errs
}

func runExport(ctx context.Context, s *Service, t meta.Task, _ string, log *RunLog) (summary, error) {
	if err := s.transferReady(); err != nil {
		return summary{msg: err.Error()}, err
	}
	var p exportParams
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	ep := transfer.ExportParams{Repositories: p.Repositories, Destination: p.Destination, IncludeProxies: p.IncludeProxies,
		HardLinks: p.HardLinks == nil || *p.HardLinks}
	if p.Since != "" {
		since, err := time.Parse(time.RFC3339Nano, p.Since)
		if err != nil {
			return summary{msg: "invalid params"}, fmt.Errorf("params.since: %w", err)
		}
		ep.Since = since
	}
	res, err := s.Transfer.Export(ctx, t.ID, ep, log)
	return summary{msg: fmt.Sprintf("exported %d repositories, %d files (%d bytes)", res.Repos, res.Assets, res.Bytes),
		removed: res.Assets, bytes: res.Bytes}, err
}

func runImport(ctx context.Context, s *Service, t meta.Task, actor string, log *RunLog) (summary, error) {
	if err := s.transferReady(); err != nil {
		return summary{msg: err.Error()}, err
	}
	var p importParams
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	res, err := s.Transfer.Import(ctx, transfer.ImportParams{Source: p.Source, Repositories: p.Repositories,
		CreateMissing: p.CreateMissing, Overwrite: p.Overwrite}, actor, log)
	return summary{msg: fmt.Sprintf("imported %d files (%d bytes) into %d repositories; %d components skipped", res.Assets, res.Bytes, res.Repos, res.Failed),
		removed: res.Assets, bytes: res.Bytes}, err
}
