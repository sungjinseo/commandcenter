package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"commandcenter/internal/importer"
	"commandcenter/internal/meta"
)

// ---- import.nexus (plan 15) ----

// importParams validates the stored params of an import: importer.Params,
// which has no password field (an unknown field such as "password" is
// refused, so a password can never reach the task row).
func importParams(s *Service, raw json.RawMessage, check bool) (string, []string) {
	if s.Import == nil {
		return "", []string{"import is not available"}
	}
	var p importer.Params
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error()}
	}
	var errs []string
	if check {
		p, errs = s.Import.Normalize(p)
	} else {
		p, errs = p.Normalize(s.now(), nil)
	}
	if len(errs) > 0 {
		return "", errs
	}
	return encodeParams(p), nil
}

// StartImport starts an import.nexus run for actor in the background. The
// params must not hold the password: password is handed to the run in
// memory only. It returns the run id and the job. A *repo.ValidationError
// for bad params, a *BusyError when a task is running.
func (s *Service) StartImport(params json.RawMessage, password, actor string) (runID, job string, err error) {
	runID, canonical, err := s.startAdHoc(TypeImportNexus, params, actor, true, password)
	if err != nil {
		return "", "", err
	}
	var p importer.Params
	_ = json.Unmarshal([]byte(canonical), &p)
	return runID, p.Job, nil
}

// takeSecret returns the in-memory secret of the run being started and
// forgets it.
func (s *Service) takeSecret() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.secret
	s.secret = ""
	return v
}

func runImport(ctx context.Context, s *Service, t meta.Task, actor string, log *RunLog) (summary, error) {
	cred := importer.Credentials{Password: s.takeSecret()}
	if s.Import == nil {
		return summary{msg: "import is not available"}, errors.New("import is not available")
	}
	var p importer.Params
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	rep, err := s.Import.Run(ctx, p, cred, actor, log)
	if rep == nil {
		return summary{msg: "job " + p.Job + " did not start"}, err
	}
	return summary{msg: rep.Summary(), bytes: rep.Totals.Bytes}, err
}
