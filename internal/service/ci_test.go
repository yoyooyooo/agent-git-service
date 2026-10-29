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

func TestCIChecksForgejoPRProjectionBindsCurrentHead(t *testing.T) {
	s, ctx, repo, pr, backend, config := ciFixture(t)
	selected := config.Backends["builder"]
	selected.Kind = "forgejo"
	config.Backends["builder"] = selected
	backend.runs[0].Branch = "#42"
	backend.runs[0].PullRequests = []int{42}
	backend.jobs[0].Name = "check-linux"
	required := []string{"check-linux"}
	binding := config.Repositories[repo.FullName]
	binding.RequiredChecks = &required
	config.Repositories[repo.FullName] = binding
	registry, err := cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
	if err != nil {
		t.Fatal(err)
	}
	s.CI = registry
	projection := db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: "forgejo", ExternalRepo: "ci/project", ExternalNumber: 42, ExternalURL: "https://api.example.test/ci/project/pulls/42", LastSyncedSHA: pr.HeadSHA}
	if err = s.DB.Create(&projection).Error; err != nil {
		t.Fatal(err)
	}
	observed, err := s.ReadCIChecks(ctx, pr)
	if err != nil || len(observed.Checks) != 1 || observed.Checks[0].Name != "check-linux" || observed.Checks[0].Conclusion != "success" || !observed.Checks[0].Required {
		t.Fatalf("lost projected check-linux: %#v %v", observed, err)
	}
	for _, updates := range []map[string]any{
		{"external_number": 43},
		{"external_number": 42, "last_synced_sha": strings.Repeat("b", 40)},
		{"last_synced_sha": pr.HeadSHA, "external_url": "https://wrong.example.test/ci/project/pulls/42"},
	} {
		if err = s.DB.Model(&projection).Updates(updates).Error; err != nil {
			t.Fatal(err)
		}
		observed, err = s.ReadCIChecks(ctx, pr)
		if err != nil || len(observed.Checks) != 1 || observed.Checks[0].Status != "queued" || observed.Checks[0].Conclusion != "" {
			t.Fatalf("unbound CI evidence passed: %#v %v", observed, err)
		}
	}
}

func TestCIChecksLaterWorkflowDispatchReplacesFailedPullRequestRun(t *testing.T) {
	s, ctx, repo, pr, backend, config := ciFixture(t)
	selected := config.Backends["builder"]
	selected.Kind = "forgejo"
	config.Backends["builder"] = selected
	required := []string{"check-linux"}
	binding := config.Repositories[repo.FullName]
	binding.RequiredChecks = &required
	config.Repositories[repo.FullName] = binding
	registry, err := cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
	if err != nil {
		t.Fatal(err)
	}
	s.CI = registry
	projection := db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: "forgejo", ExternalRepo: "ci/project", ExternalNumber: 42, ExternalURL: "https://api.example.test/ci/project/pulls/42", LastSyncedSHA: pr.HeadSHA}
	if err = s.DB.Create(&projection).Error; err != nil {
		t.Fatal(err)
	}
	failedAt := time.Now().UTC().Add(-time.Hour)
	dispatchAt := failedAt.Add(30 * time.Minute)
	backend.runs = []cibackend.Run{
		{Key: "289", Workflow: "check.yml", Name: "check.yml", Branch: "#42", HeadSHA: pr.HeadSHA, Event: "pull_request_sync", Status: "completed", Conclusion: "failure", CreatedAt: failedAt, UpdatedAt: failedAt, Number: 289, Attempt: 1, PullRequests: []int{42}},
		{Key: "290", Workflow: "check.yml", Name: "check.yml", Branch: pr.HeadRef, HeadSHA: pr.HeadSHA, Event: "workflow_dispatch", Status: "completed", Conclusion: "success", CreatedAt: dispatchAt, UpdatedAt: dispatchAt, Number: 290, Attempt: 1},
	}
	backend.jobs = []cibackend.Job{
		{Key: "289:1", Run: "289", Name: "check-linux", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "failure"},
		{Key: "290:1", Run: "290", Name: "check-linux", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "success"},
	}
	observed, err := s.ReadCIChecks(ctx, pr)
	if err != nil || len(observed.Checks) != 1 || observed.Checks[0].Name != "check-linux" || observed.Checks[0].Conclusion != "success" || observed.Checks[0].Event != "workflow_dispatch" {
		t.Fatalf("later workflow_dispatch did not replace the failed pull_request check: %#v %v", observed, err)
	}
	if err = s.enforceConfiguredCIForMerge(ctx, pr); err != nil {
		t.Fatal("current successful check-linux blocked configured CI preflight", err)
	}
	backend.runs[0], backend.runs[1] = backend.runs[1], backend.runs[0]
	observed, err = s.ReadCIChecks(ctx, pr)
	if err != nil || len(observed.Checks) != 1 || observed.Checks[0].Event != "workflow_dispatch" {
		t.Fatalf("run listing order changed selected evidence: %#v %v", observed, err)
	}
	for _, state := range []struct{ status, conclusion string }{
		{"completed", "failure"}, {"in_progress", ""},
	} {
		backend.runs[0].Status, backend.runs[0].Conclusion = state.status, state.conclusion
		backend.jobs[1].Status, backend.jobs[1].Conclusion = state.status, state.conclusion
		observed, err = s.ReadCIChecks(ctx, pr)
		if err != nil || len(observed.Checks) != 1 || observed.Checks[0].Event != "workflow_dispatch" || observed.Checks[0].Status != state.status || observed.Checks[0].Conclusion != state.conclusion {
			t.Fatalf("latest dispatch state was not retained: %#v %v", observed, err)
		}
		if err = s.enforceConfiguredCIForMerge(ctx, pr); err == nil {
			t.Fatal("failed or pending latest dispatch allowed CI preflight")
		}
	}
}

func TestCIChecksSameJobNameInDifferentWorkflowsStaysDistinct(t *testing.T) {
	s, ctx, repo, pr, backend, config := ciFixture(t)
	selected := config.Backends["builder"]
	selected.Kind = "forgejo"
	config.Backends["builder"] = selected
	required := []string{"notify-feishu-on-failure"}
	binding := config.Repositories[repo.FullName]
	binding.RequiredChecks = &required
	config.Repositories[repo.FullName] = binding
	registry, err := cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
	if err != nil {
		t.Fatal(err)
	}
	s.CI = registry
	projection := db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: "forgejo", ExternalRepo: "ci/project", ExternalNumber: 615, ExternalURL: "https://api.example.test/ci/project/pulls/615", LastSyncedSHA: pr.HeadSHA}
	if err = s.DB.Create(&projection).Error; err != nil {
		t.Fatal(err)
	}
	// ux/merdi-express-shipping PR 615: ci.yml and app-ci.yml both define
	// notify-feishu-on-failure. Those are different workflow files, so gh pr checks
	// must see both instead of rejecting the rollup as inconsistent evidence.
	created := time.Now().UTC().Add(-time.Hour)
	backend.runs = []cibackend.Run{
		{Key: "2991", Workflow: "ci.yml", Name: "ci.yml", Branch: "#615", HeadSHA: pr.HeadSHA, Event: "pull_request", Status: "completed", Conclusion: "success", CreatedAt: created, UpdatedAt: created, Number: 2991, Attempt: 1, PullRequests: []int{615}},
		{Key: "2992", Workflow: "app-ci.yml", Name: "app-ci.yml", Branch: "#615", HeadSHA: pr.HeadSHA, Event: "pull_request", Status: "completed", Conclusion: "failure", CreatedAt: created.Add(time.Minute), UpdatedAt: created.Add(time.Minute), Number: 2992, Attempt: 1, PullRequests: []int{615}},
	}
	backend.jobs = []cibackend.Job{
		{Key: "2991:1", Run: "2991", Name: "build", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "success"},
		{Key: "2991:2", Run: "2991", Name: "notify-feishu-on-failure", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "skipped"},
		{Key: "2992:1", Run: "2992", Name: "app", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "failure"},
		{Key: "2992:2", Run: "2992", Name: "notify-feishu-on-failure", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "success"},
	}
	observed, err := s.ReadCIChecks(ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	type observedCheck struct {
		workflow, conclusion string
		required             bool
	}
	got := map[string]observedCheck{}
	for _, check := range observed.Checks {
		key := check.Workflow + "/" + check.Name
		if _, exists := got[key]; exists {
			t.Fatalf("check reported twice: %s %#v", key, observed)
		}
		got[key] = observedCheck{check.Workflow, check.Conclusion, check.Required}
	}
	want := map[string]observedCheck{
		"ci.yml/build":                        {"ci.yml", "success", false},
		"ci.yml/notify-feishu-on-failure":     {"ci.yml", "skipped", true},
		"app-ci.yml/app":                      {"app-ci.yml", "failure", false},
		"app-ci.yml/notify-feishu-on-failure": {"app-ci.yml", "success", true},
	}
	if len(got) != len(want) {
		t.Fatalf("checks = %#v", observed.Checks)
	}
	for key, check := range want {
		if got[key] != check {
			t.Fatalf("%s = %#v, want %#v", key, got[key], check)
		}
	}
	if err = s.enforceConfiguredCIForMerge(ctx, pr); err != nil {
		t.Fatal(err)
	}
	// Commit check-runs omit the branch match that PR rollup uses. The same two
	// workflow files must still be distinct there; gh pr checks is the PR path.
	bySHA, err := s.ReadCIChecksForSHA(ctx, repo, pr.HeadSHA)
	if err != nil || len(bySHA.Checks) != 4 {
		t.Fatalf("SHA check list collapsed duplicate job names: %#v %v", bySHA, err)
	}

	backend.jobs = append(backend.jobs, cibackend.Job{Key: "2991:3", Run: "2991", Name: "notify-feishu-on-failure", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "failure"})
	if _, err = s.ReadCIChecks(ctx, pr); !errors.Is(err, cibackend.ErrInvalid) {
		t.Fatal("same workflow and job name accepted", err)
	}
}

func TestCIRerunAuthorizationStateAndJobIdentity(t *testing.T) {
	s, ctx, repo, _, backend, _ := ciFixture(t)
	runs, err := s.CIRuns(ctx, repo.FullName, cibackend.Query{})
	if err != nil {
		t.Fatal(err)
	}
	id := runs.Runs[0].ID
	jobs, err := s.CIJobs(ctx, repo.FullName, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"rerun", "rerun-failed-jobs", "job-rerun"} {
		target := id
		if action == "job-rerun" {
			target = jobs[0].ID
		}
		before := backend.actions
		if err = s.CIAction(ctx, repo.FullName, target, action); err != nil || backend.actions != before+1 {
			t.Fatalf("%s: %v", action, err)
		}
		backend.runs[0].Status = "in_progress"
		if err = s.CIAction(ctx, repo.FullName, target, action); !errors.Is(err, ErrConflict) || backend.actions != before+1 {
			t.Fatalf("active %s: %v", action, err)
		}
		backend.runs[0].Status = "completed"
		if err = s.CIAction(ctx, repo.FullName, CIExternalIDBase+999999, action); !errors.Is(err, ErrNotFound) || backend.actions != before+1 {
			t.Fatalf("unknown %s: %v", action, err)
		}
	}
	reader := db.User{Login: "rerun-reader", Type: db.TypeUser}
	if err = s.DB.Create(&reader).Error; err != nil {
		t.Fatal(err)
	}
	if err = s.DB.Create(&db.Collaborator{RepositoryID: repo.ID, UserID: reader.ID, Permission: "read"}).Error; err != nil {
		t.Fatal(err)
	}
	calls, writes := backend.calls, backend.actions
	for _, target := range []uint64{id, jobs[0].ID} {
		action := "rerun"
		if target == jobs[0].ID {
			action = "job-rerun"
		}
		if err = s.CIAction(ContextWithUser(context.Background(), reader), repo.FullName, target, action); !errors.Is(err, ErrForbidden) {
			t.Fatal(err)
		}
	}
	if backend.calls != calls || backend.actions != writes {
		t.Fatal("read-only caller reached provider")
	}
	backend.failure = cibackend.ErrForbidden
	if err = s.CIAction(ctx, repo.FullName, id, "rerun"); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
}

func TestPreferObservedCIRunOrdersEvidenceNotConclusions(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	current := cibackend.Run{Event: "pull_request", CreatedAt: created, Number: 10, Attempt: 1, Status: "completed", Conclusion: "failure"}
	for _, tc := range []struct {
		name      string
		candidate cibackend.Run
		want      bool
		reverse   bool
	}{
		{"later dispatch success", cibackend.Run{Event: "workflow_dispatch", CreatedAt: created.Add(time.Minute), Number: 11, Attempt: 1, Status: "completed", Conclusion: "success"}, true, false},
		{"later dispatch failure", cibackend.Run{Event: "workflow_dispatch", CreatedAt: created.Add(time.Minute), Number: 11, Attempt: 1, Status: "completed", Conclusion: "failure"}, true, false},
		{"later dispatch pending", cibackend.Run{Event: "workflow_dispatch", CreatedAt: created.Add(time.Minute), Number: 11, Attempt: 1, Status: "queued"}, true, false},
		{"older dispatch success", cibackend.Run{Event: "workflow_dispatch", CreatedAt: created.Add(-time.Minute), Number: 9, Attempt: 1, Status: "completed", Conclusion: "success"}, false, true},
		{"older PR retry", cibackend.Run{Event: "pull_request", CreatedAt: created.Add(-time.Minute), Number: 9, Attempt: 2}, false, true},
		{"same timestamp newer run", cibackend.Run{Event: "workflow_dispatch", CreatedAt: created, Number: 11, Attempt: 1}, true, false},
		{"same run newer attempt", cibackend.Run{Event: "workflow_dispatch", CreatedAt: created, Number: 10, Attempt: 2}, true, false},
		{"equal rank dispatch", cibackend.Run{Event: "workflow_dispatch", CreatedAt: created, Number: 10, Attempt: 1}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := preferObservedCIRun(tc.candidate, current); got != tc.want {
				t.Fatalf("preferObservedCIRun() = %v, want %v", got, tc.want)
			}
			if got := preferObservedCIRun(current, tc.candidate); got != tc.reverse {
				t.Fatalf("reverse preference = %v, want %v", got, tc.reverse)
			}
		})
	}
}
