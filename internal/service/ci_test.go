package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/db"
)

type ciFixtureBackend struct {
	runs    []cibackend.Run
	jobs    []cibackend.Job
	failure error
	calls   int
	actions int
}

func (f *ciFixtureBackend) Runs(context.Context, string, cibackend.Query) (cibackend.Runs, error) {
	f.calls++
	return cibackend.Runs{Items: f.runs, Total: len(f.runs), Complete: true}, f.failure
}
func (f *ciFixtureBackend) Run(_ context.Context, _ string, id string) (cibackend.Run, error) {
	f.calls++
	if f.failure != nil {
		return cibackend.Run{}, f.failure
	}
	for _, r := range f.runs {
		if r.Key == id {
			return r, nil
		}
	}
	return cibackend.Run{}, cibackend.ErrNotFound
}
func (f *ciFixtureBackend) Jobs(_ context.Context, _ string, id string) (cibackend.Jobs, error) {
	f.calls++
	out := []cibackend.Job{}
	for _, j := range f.jobs {
		if j.Run == id {
			out = append(out, j)
		}
	}
	return cibackend.Jobs{Items: out, Total: len(out), Complete: true}, f.failure
}
func (f *ciFixtureBackend) Job(_ context.Context, _ string, id string) (cibackend.Job, error) {
	f.calls++
	for _, j := range f.jobs {
		if j.Key == id {
			return j, nil
		}
	}
	return cibackend.Job{}, cibackend.ErrNotFound
}
func (f *ciFixtureBackend) JobLogs(context.Context, string, string) ([]byte, error) {
	f.calls++
	return []byte("fixture-only\n"), f.failure
}
func (f *ciFixtureBackend) RunLogs(context.Context, string, string) ([]byte, error) {
	f.calls++
	return nil, cibackend.ErrUnsupported
}
func (f *ciFixtureBackend) Action(context.Context, string, string, string) error {
	f.actions++
	return f.failure
}
func (f *ciFixtureBackend) Workflows(context.Context, string, int, int) (cibackend.Workflows, error) {
	return cibackend.Workflows{}, cibackend.ErrUnsupported
}
func (f *ciFixtureBackend) Workflow(context.Context, string, string) (cibackend.Workflow, error) {
	return cibackend.Workflow{}, cibackend.ErrUnsupported
}

func ciFixture(t *testing.T) (*Service, context.Context, db.Repository, db.PullRequest, *ciFixtureBackend, cibackend.Config) {
	t.Helper()
	s, ctx, owner, _ := nativeRunFixture(t)
	repo := db.Repository{OwnerID: owner.ID, Name: "project", FullName: owner.Login + "/project", DefaultBranch: "main", Private: true}
	if e := s.DB.Create(&repo).Error; e != nil {
		t.Fatal(e)
	}
	pr := db.PullRequest{RepositoryID: repo.ID, HeadRepositoryID: repo.ID, AuthorID: owner.ID, Number: 1, Title: "fixture", State: db.StateOpen, HeadRef: "feature", BaseRef: "main", HeadSHA: strings.Repeat("a", 40)}
	if e := s.DB.Create(&pr).Error; e != nil {
		t.Fatal(e)
	}
	pr.Repository = repo
	now := time.Now().UTC()
	backend := &ciFixtureBackend{runs: []cibackend.Run{{Key: "17", Workflow: "ci.yml", Name: "CI", Branch: "feature", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "success", CreatedAt: now, UpdatedAt: now, Number: 9, Attempt: 1}}, jobs: []cibackend.Job{{Key: "41", Run: "17", Name: "unit", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "success"}}}
	required := []string{"unit", "lint"}
	config := cibackend.Config{Backends: map[string]cibackend.BackendConfig{"builder": {Kind: "github-actions", URL: "https://api.example.test", TokenFile: "fixture-only"}}, Repositories: map[string]cibackend.Binding{repo.FullName: {Backend: "builder", Repository: "ci/project", RequiredChecks: &required}}}
	registry, e := cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
	if e != nil {
		t.Fatal(e)
	}
	s.CI = registry
	return s, ctx, repo, pr, backend, config
}
func TestCIBackendSelectionOwnsIDsAndDoesNotWidenRepositoryAccess(t *testing.T) {
	s, ctx, repo, _, backend, config := ciFixture(t)
	found, e := s.CIRuns(ctx, repo.FullName, cibackend.Query{})
	if e != nil || len(found.Runs) != 1 {
		t.Fatal(e)
	}
	id := found.Runs[0].ID
	if id <= CIExternalIDBase || id >= 2*CIExternalIDBase || id == 17 {
		t.Fatal("provider ID escaped without namespacing")
	}
	again, e := s.CIRuns(ctx, repo.FullName, cibackend.Query{})
	if e != nil || again.Runs[0].ID != id {
		t.Fatal("observation identity drift")
	}
	calls := backend.calls
	other := db.User{Login: "unrelated-reader", Type: db.TypeUser}
	if e := s.DB.Create(&other).Error; e != nil {
		t.Fatal(e)
	}
	if _, e = s.CIRuns(ContextWithUser(context.Background(), other), repo.FullName, cibackend.Query{}); e == nil || backend.calls != calls {
		t.Fatal("unauthorized repository triggered backend I/O")
	}
	changed := config.Backends["builder"]
	changed.URL = "https://replacement.example.test"
	config.Backends["builder"] = changed
	registry, e := cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
	if e != nil {
		t.Fatal(e)
	}
	s.CI = registry
	calls = backend.calls
	if _, e = s.CIRun(ctx, repo.FullName, id); !errors.Is(e, ErrNotFound) || backend.calls != calls {
		t.Fatal("old ID was sent to replacement backend", e)
	}
	fresh, e := s.CIRuns(ctx, repo.FullName, cibackend.Query{})
	if e != nil || fresh.Runs[0].ID == id {
		t.Fatal("new backend reused old API ID")
	}
}
func TestCIChecksMissingCurrentHeadRequiredEvidenceRemainsPending(t *testing.T) {
	s, ctx, _, pr, backend, _ := ciFixture(t)
	backend.runs = append(backend.runs, cibackend.Run{Key: "99", Workflow: "lint.yml", Name: "lint", HeadSHA: strings.Repeat("b", 40), Branch: "feature", Status: "completed", Conclusion: "success", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	checks, e := s.ReadCIChecks(ctx, pr)
	if e != nil {
		t.Fatal(e)
	}
	byName := map[string]CICheck{}
	for _, c := range checks.Checks {
		byName[c.Name] = c
	}
	if !checks.RequiredKnown || byName["unit"].Conclusion != "success" || byName["lint"].Status != "queued" || !byName["lint"].Required {
		t.Fatalf("missing/old-head evidence passed: %+v", checks)
	}
	if e = s.enforceConfiguredCIForMerge(ctx, pr); e == nil {
		t.Fatal("missing required lint allowed merge")
	}
	backend.failure = cibackend.ErrUnavailable
	if _, e = s.ReadCIChecks(ctx, pr); e == nil {
		t.Fatal("provider outage became empty checks")
	}
}
func TestCIPolicyUnknownAndDuplicateCheckNamesCannotBecomeGreen(t *testing.T) {
	s, ctx, repo, pr, backend, config := ciFixture(t)
	binding := config.Repositories[repo.FullName]
	binding.RequiredChecks = nil
	config.Repositories[repo.FullName] = binding
	registry, e := cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
	if e != nil {
		t.Fatal(e)
	}
	s.CI = registry
	checks, e := s.ReadCIChecks(ctx, pr)
	if e != nil || checks.RequiredKnown {
		t.Fatal("unknown required policy became known", e)
	}
	backend.jobs = append(backend.jobs, cibackend.Job{Key: "42", Run: "17", Name: "unit", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "failure"})
	if _, e = s.ReadCIChecks(ctx, pr); !errors.Is(e, cibackend.ErrInvalid) {
		t.Fatal("ambiguous required name accepted", e)
	}
}
func TestCICancelUsesCurrentNativeWritePermissionNotBackendSecrets(t *testing.T) {
	s, ctx, repo, _, backend, _ := ciFixture(t)
	list, e := s.CIRuns(ctx, repo.FullName, cibackend.Query{})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.CIAction(ctx, repo.FullName, list.Runs[0].ID, "cancel"); e != nil || backend.actions != 1 {
		t.Fatal(e)
	}
	reader := db.User{Login: "reader", Type: db.TypeUser}
	if e = s.DB.Create(&reader).Error; e != nil {
		t.Fatal(e)
	}
	collaborator := db.Collaborator{RepositoryID: repo.ID, UserID: reader.ID, Permission: "read"}
	if e = s.DB.Create(&collaborator).Error; e != nil {
		t.Fatal(e)
	}
	if e = s.CIAction(ContextWithUser(context.Background(), reader), repo.FullName, list.Runs[0].ID, "cancel"); !errors.Is(e, ErrForbidden) || backend.actions != 1 {
		t.Fatal("reader used server token for mutation", e)
	}
}
