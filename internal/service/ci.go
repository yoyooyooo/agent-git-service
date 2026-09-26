package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Keep externally mapped IDs disjoint from native Actions IDs and within exact
// JSON/JavaScript integer range. Unknown or retired mappings never fall through.
const CIExternalIDBase uint64 = 1 << 52

type CIRun struct {
	ID         uint64
	WorkflowID uint64
	Run        cibackend.Run
	Backend    string
}
type CIJob struct {
	ID      uint64
	RunID   uint64
	Job     cibackend.Job
	Backend string
}
type CIRuns struct {
	Runs     []CIRun
	Total    int
	Complete bool
	Backend  string
}
type CICheck struct {
	ID                                             string
	Name, Status, Conclusion, URL, Workflow, Event string
	Required                                       bool
	StartedAt, CompletedAt                         *time.Time
}
type CIChecks struct {
	Backend, HeadSHA string
	RequiredKnown    bool
	Checks           []CICheck
}

func (s *Service) CISelection(repository string) (cibackend.Selection, error) {
	return s.CI.Select(repository)
}
func (s *Service) ciScope(ctx context.Context, repository string) (db.Repository, cibackend.Selection, error) {
	repo, e := s.GetRepo(ctx, repository)
	if e != nil {
		return repo, cibackend.Selection{}, e
	}
	selected, e := s.CISelection(repository)
	if e != nil {
		return repo, selected, e
	}
	if selected.Name == "native" {
		return repo, selected, cibackend.ErrUnsupported
	}
	return repo, selected, nil
}
func (s *Service) mapCI(ctx context.Context, repo db.Repository, sel cibackend.Selection, kind, id, parent string) (uint64, error) {
	if id == "" || len(id) > 128 || len(parent) > 128 {
		return 0, cibackend.ErrInvalid
	}
	row := db.CIResource{RepositoryID: repo.ID, Namespace: sel.Namespace, ExternalRepository: sel.Binding.Repository, Kind: kind, ExternalID: id, ParentID: parent}
	database := s.DBForCtx(ctx)
	if e := database.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; e != nil {
		return 0, e
	}
	if e := database.Where("repository_id = ? AND namespace = ? AND external_repository = ? AND kind = ? AND external_id = ?", repo.ID, sel.Namespace, sel.Binding.Repository, kind, id).First(&row).Error; e != nil {
		return 0, e
	}
	if row.ParentID != parent || row.ID == 0 || uint64(row.ID) >= CIExternalIDBase {
		return 0, cibackend.ErrInvalid
	}
	return CIExternalIDBase + uint64(row.ID), nil
}
func (s *Service) resolveCI(ctx context.Context, repo db.Repository, sel cibackend.Selection, kind string, id uint64) (db.CIResource, error) {
	var row db.CIResource
	if id <= CIExternalIDBase || id >= 2*CIExternalIDBase {
		return row, ErrNotFound
	}
	if e := s.DBForCtx(ctx).Where("id = ? AND repository_id = ? AND namespace = ? AND external_repository = ? AND kind = ?", id-CIExternalIDBase, repo.ID, sel.Namespace, sel.Binding.Repository, kind).First(&row).Error; e != nil {
		return row, wrapErr(e)
	}
	return row, nil
}
func (s *Service) ciRun(ctx context.Context, repo db.Repository, sel cibackend.Selection, run cibackend.Run) (CIRun, error) {
	id, e := s.mapCI(ctx, repo, sel, "run", run.Key, "")
	if e != nil {
		return CIRun{}, e
	}
	workflow, e := s.mapCI(ctx, repo, sel, "workflow", run.Workflow, "")
	if e != nil {
		return CIRun{}, e
	}
	return CIRun{ID: id, WorkflowID: workflow, Run: run, Backend: sel.Name}, nil
}
func (s *Service) CIRuns(ctx context.Context, repository string, query cibackend.Query) (CIRuns, error) {
	repo, sel, e := s.ciScope(ctx, repository)
	if e != nil {
		return CIRuns{}, e
	}
	if sel.Name == "none" {
		return CIRuns{Runs: []CIRun{}, Backend: "none", Complete: true}, nil
	}
	if query.Workflow != "" {
		id, e := strconv.ParseUint(query.Workflow, 10, 64)
		if e != nil {
			return CIRuns{}, ErrValidation
		}
		m, e := s.resolveCI(ctx, repo, sel, "workflow", id)
		if e != nil {
			return CIRuns{}, e
		}
		query.Workflow = m.ExternalID
	}
	found, e := sel.Backend.Runs(ctx, sel.Binding.Repository, query)
	if e != nil {
		return CIRuns{}, ciError(e)
	}
	out := CIRuns{Runs: []CIRun{}, Total: found.Total, Complete: found.Complete, Backend: sel.Name}
	for _, run := range found.Items {
		// Do not rely on a provider actually honoring its query parameters.
		if query.HeadSHA != "" && run.HeadSHA != query.HeadSHA || query.Branch != "" && run.Branch != query.Branch || query.Event != "" && run.Event != query.Event || query.Status != "" && run.Status != query.Status {
			continue
		}
		r, e := s.ciRun(ctx, repo, sel, run)
		if e != nil {
			return CIRuns{}, e
		}
		out.Runs = append(out.Runs, r)
	}
	return out, nil
}
func (s *Service) CIRun(ctx context.Context, repository string, id uint64) (CIRun, error) {
	repo, sel, e := s.ciScope(ctx, repository)
	if e != nil {
		return CIRun{}, e
	}
	if sel.Backend == nil {
		return CIRun{}, ErrNotFound
	}
	m, e := s.resolveCI(ctx, repo, sel, "run", id)
	if e != nil {
		return CIRun{}, e
	}
	r, e := sel.Backend.Run(ctx, sel.Binding.Repository, m.ExternalID)
	if e != nil {
		return CIRun{}, ciError(e)
	}
	if r.Key != m.ExternalID {
		return CIRun{}, cibackend.ErrInvalid
	}
	return s.ciRun(ctx, repo, sel, r)
}
func (s *Service) CIJobs(ctx context.Context, repository string, id uint64) ([]CIJob, error) {
	run, e := s.CIRun(ctx, repository, id)
	if e != nil {
		return nil, e
	}
	repo, sel, e := s.ciScope(ctx, repository)
	if e != nil {
		return nil, e
	}
	jobs, e := sel.Backend.Jobs(ctx, sel.Binding.Repository, run.Run.Key)
	if e != nil {
		return nil, ciError(e)
	}
	if !jobs.Complete {
		return nil, cibackend.ErrUnavailable
	}
	out := make([]CIJob, 0, len(jobs.Items))
	seen := map[string]bool{}
	for _, j := range jobs.Items {
		if j.Run != run.Run.Key || j.HeadSHA != "" && j.HeadSHA != run.Run.HeadSHA || seen[j.Key] {
			return nil, cibackend.ErrInvalid
		}
		seen[j.Key] = true
		j.HeadSHA = run.Run.HeadSHA
		id, e := s.mapCI(ctx, repo, sel, "job", j.Key, j.Run)
		if e != nil {
			return nil, e
		}
		out = append(out, CIJob{ID: id, RunID: run.ID, Job: j, Backend: sel.Name})
	}
	return out, nil
}
func (s *Service) CIJob(ctx context.Context, repository string, id uint64) (CIJob, error) {
	repo, sel, e := s.ciScope(ctx, repository)
	if e != nil {
		return CIJob{}, e
	}
	if sel.Backend == nil {
		return CIJob{}, ErrNotFound
	}
	m, e := s.resolveCI(ctx, repo, sel, "job", id)
	if e != nil {
		return CIJob{}, e
	}
	job, e := sel.Backend.Job(ctx, sel.Binding.Repository, m.ExternalID)
	if e != nil {
		return CIJob{}, ciError(e)
	}
	if job.Key != m.ExternalID || job.Run != m.ParentID {
		return CIJob{}, cibackend.ErrInvalid
	}
	run, e := sel.Backend.Run(ctx, sel.Binding.Repository, m.ParentID)
	if e != nil {
		return CIJob{}, ciError(e)
	}
	if run.Key != m.ParentID || job.HeadSHA != "" && job.HeadSHA != run.HeadSHA {
		return CIJob{}, cibackend.ErrInvalid
	}
	mapped, e := s.ciRun(ctx, repo, sel, run)
	if e != nil {
		return CIJob{}, e
	}
	job.HeadSHA = run.HeadSHA
	return CIJob{ID: id, RunID: mapped.ID, Job: job, Backend: sel.Name}, nil
}
func (s *Service) CILogs(ctx context.Context, repository string, id uint64, job bool) ([]byte, error) {
	_, sel, e := s.ciScope(ctx, repository)
	if e != nil {
		return nil, e
	}
	if sel.Backend == nil {
		return nil, ErrNotFound
	}
	if job {
		j, e := s.CIJob(ctx, repository, id)
		if e != nil {
			return nil, e
		}
		b, e := sel.Backend.JobLogs(ctx, sel.Binding.Repository, j.Job.Key)
		return b, ciError(e)
	}
	r, e := s.CIRun(ctx, repository, id)
	if e != nil {
		return nil, e
	}
	b, e := sel.Backend.RunLogs(ctx, sel.Binding.Repository, r.Run.Key)
	if e != nil {
		return nil, ciError(e)
	}
	if e = cibackend.ValidateLogArchive(b); e != nil {
		return nil, e
	}
	return b, nil
}
func (s *Service) CIAction(ctx context.Context, repository string, id uint64, action string) error {
	repo, sel, e := s.ciScope(ctx, repository)
	if e != nil {
		return e
	}
	if sel.Backend == nil {
		return ErrNotFound
	}
	user, e := s.GetCurrentUser(ctx)
	if e != nil {
		return e
	}
	permission, e := s.HasRepoAccess(ctx, repo.ID, user.ID)
	if e != nil {
		return e
	}
	if !permission.AtLeast(RepoPermissionWrite) {
		return ErrForbidden
	}
	run, e := s.CIRun(ctx, repository, id)
	if e != nil {
		return e
	}
	// No mutation fallback, no automatic retry. Transport failure is an unknown
	// external result; callers must observe this exact run before another request.
	return ciError(sel.Backend.Action(ctx, sel.Binding.Repository, run.Run.Key, action))
}
func ciError(err error) error {
	if errors.Is(err, cibackend.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

func (s *Service) ReadCIChecks(ctx context.Context, pr db.PullRequest) (CIChecks, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	repo, sel, e := s.ciScope(ctx, pr.Repository.FullName)
	if e != nil {
		return CIChecks{}, e
	}
	required, known, e := s.CIRequiredChecks(ctx, pr)
	if e != nil {
		return CIChecks{}, e
	}
	out := CIChecks{Backend: sel.Name, HeadSHA: pr.HeadSHA, Checks: []CICheck{}, RequiredKnown: known}
	if pr.HeadSHA == "" || sel.Name == "none" {
		for name := range required {
			out.Checks = append(out.Checks, CICheck{ID: "required:" + name, Name: name, Status: "queued", Required: true})
		}
		return out, nil
	}
	pending := map[string]bool{}
	for name := range required {
		pending[name] = true
	}
	seenChecks := map[string]bool{}
	// Bounded traversal must be complete before claiming missing required checks.
	newest := map[string]cibackend.Run{}
	for page := 1; page <= 10; page++ {
		found, e := sel.Backend.Runs(ctx, sel.Binding.Repository, cibackend.Query{HeadSHA: pr.HeadSHA, Page: page, PerPage: 100})
		if e != nil {
			return out, ciError(e)
		}
		for _, run := range found.Items {
			if run.HeadSHA != pr.HeadSHA {
				continue
			}
			if run.Branch != pr.HeadRef {
				// Forgejo may label PR runs #<provider-number>; use a durable projection,
				// never the AGS PR number or a guessed branch name.
				var projection db.PullRequestProjection
				if sel.Kind != "forgejo" {
					continue
				}
				e := s.DBForCtx(ctx).Where("pull_request_id = ? AND repository_id = ? AND provider = ? AND external_repo = ?", pr.ID, repo.ID, "forgejo", sel.Binding.Repository).First(&projection).Error
				if e != nil {
					if errors.Is(e, gorm.ErrRecordNotFound) {
						continue
					}
					return out, e
				}
				origin, parseErr := url.Parse(sel.Origin)
				projected, projectedErr := url.Parse(projection.ExternalURL)
				if parseErr != nil || projectedErr != nil || origin.Scheme != projected.Scheme || !strings.EqualFold(origin.Host, projected.Host) || projection.LastSyncedSHA != pr.HeadSHA {
					continue
				}
				matched := run.Branch == fmt.Sprintf("#%d", projection.ExternalNumber)
				for _, n := range run.PullRequests {
					if n == projection.ExternalNumber {
						matched = true
					}
				}
				if !matched {
					continue
				}
			}
			previous, ok := newest[run.Workflow]
			if !ok || run.CreatedAt.After(previous.CreatedAt) || run.CreatedAt.Equal(previous.CreatedAt) && run.Attempt > previous.Attempt {
				newest[run.Workflow] = run
			}
		}
		if found.Complete {
			break
		}
		if page == 10 {
			return out, fmt.Errorf("%w: incomplete CI run observation", cibackend.ErrUnavailable)
		}
	}
	for _, run := range newest {
		view, e := s.ciRun(ctx, repo, sel, run)
		if e != nil {
			return out, e
		}
		jobs, e := sel.Backend.Jobs(ctx, sel.Binding.Repository, run.Key)
		if e != nil {
			return out, ciError(e)
		}
		if !jobs.Complete {
			return out, cibackend.ErrUnavailable
		}
		if len(jobs.Items) == 0 {
			if seenChecks[run.Name] {
				return out, fmt.Errorf("%w: ambiguous CI check names", cibackend.ErrInvalid)
			}
			seenChecks[run.Name] = true
			out.Checks = append(out.Checks, CICheck{ID: fmt.Sprint(view.ID), Name: run.Name, Status: run.Status, Conclusion: run.Conclusion, URL: run.URL, Workflow: run.Name, Event: run.Event, Required: required[run.Name], StartedAt: run.StartedAt})
			delete(pending, run.Name)
		}
		for _, j := range jobs.Items {
			if j.Run != run.Key || j.HeadSHA != "" && j.HeadSHA != pr.HeadSHA {
				return out, cibackend.ErrInvalid
			}
			if seenChecks[j.Name] {
				return out, fmt.Errorf("%w: ambiguous CI check names", cibackend.ErrInvalid)
			}
			seenChecks[j.Name] = true
			id, e := s.mapCI(ctx, repo, sel, "job", j.Key, j.Run)
			if e != nil {
				return out, e
			}
			out.Checks = append(out.Checks, CICheck{ID: fmt.Sprint(id), Name: j.Name, Status: j.Status, Conclusion: j.Conclusion, URL: j.URL, Workflow: run.Name, Event: run.Event, Required: required[j.Name], StartedAt: j.StartedAt, CompletedAt: j.CompletedAt})
			delete(pending, j.Name)
		}
	}
	for name := range pending {
		out.Checks = append(out.Checks, CICheck{ID: "required:" + name, Name: name, Status: "queued", Required: true})
	}
	return out, nil
}

// Safe status errors deliberately omit provider response bodies and credentials.
func CIErrorMessage(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "CI resource not found in the selected backend"
	case errors.Is(err, cibackend.ErrUnsupported):
		return "Selected CI backend does not support this operation"
	case errors.Is(err, cibackend.ErrInvalid):
		return "Selected CI backend returned inconsistent evidence"
	default:
		return "Selected CI backend could not confirm the requested result"
	}
}
