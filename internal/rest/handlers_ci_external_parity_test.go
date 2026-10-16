package rest_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

type parityCIBackend struct {
	runs []cibackend.Run
	jobs []cibackend.Job
}

func (b *parityCIBackend) Runs(_ context.Context, _ string, query cibackend.Query) (cibackend.Runs, error) {
	items := make([]cibackend.Run, 0, len(b.runs))
	for _, run := range b.runs {
		if query.HeadSHA != "" && run.HeadSHA != query.HeadSHA {
			continue
		}
		items = append(items, run)
	}
	return cibackend.Runs{Items: items, Total: len(items), Complete: true}, nil
}

func (b *parityCIBackend) Run(_ context.Context, _, id string) (cibackend.Run, error) {
	for _, run := range b.runs {
		if run.Key == id {
			return run, nil
		}
	}
	return cibackend.Run{}, cibackend.ErrNotFound
}

func (b *parityCIBackend) Jobs(_ context.Context, _, id string) (cibackend.Jobs, error) {
	items := []cibackend.Job{}
	for _, job := range b.jobs {
		if job.Run == id {
			items = append(items, job)
		}
	}
	return cibackend.Jobs{Items: items, Total: len(items), Complete: true}, nil
}

func (b *parityCIBackend) Job(_ context.Context, _, id string) (cibackend.Job, error) {
	for _, job := range b.jobs {
		if job.Key == id {
			return job, nil
		}
	}
	return cibackend.Job{}, cibackend.ErrNotFound
}

func (b *parityCIBackend) JobLogs(context.Context, string, string) ([]byte, error) {
	return []byte("job log\n"), nil
}

func (b *parityCIBackend) RunLogs(context.Context, string, string) ([]byte, error) {
	return nil, cibackend.ErrUnsupported
}

func (b *parityCIBackend) Action(context.Context, string, string, string) error { return nil }

func (b *parityCIBackend) Workflows(context.Context, string, int, int) (cibackend.Workflows, error) {
	return cibackend.Workflows{}, cibackend.ErrUnsupported
}

func (b *parityCIBackend) Workflow(context.Context, string, string) (cibackend.Workflow, error) {
	return cibackend.Workflow{}, cibackend.ErrUnsupported
}

func bindParityCI(t *testing.T, h *testharness.Harness, repo db.Repository, backend *parityCIBackend) {
	t.Helper()
	required := []string{"unit"}
	config := cibackend.Config{
		Backends: map[string]cibackend.BackendConfig{
			"builder": {Kind: "forgejo", URL: "https://ci.example.test", TokenFile: "fixture-only"},
		},
		Repositories: map[string]cibackend.Binding{
			repo.FullName: {Backend: "builder", Repository: "ci/" + repo.Name, RequiredChecks: &required},
		},
	}
	registry, err := cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
	if err != nil {
		t.Fatal(err)
	}
	h.Svc.CI = registry
}

func TestExternalCheckRunsAndEmptyArtifacts(t *testing.T) {
	h := testharness.New(t)
	repo, headSHA := seedCheckRunsRepo(t, h, "external-checks")
	now := time.Now().UTC()
	backend := &parityCIBackend{
		runs: []cibackend.Run{{
			Key: "17", Workflow: "ci.yml", Name: "CI", Branch: "#4", HeadSHA: headSHA,
			Status: "completed", Conclusion: "success", CreatedAt: now, UpdatedAt: now, Number: 9, Attempt: 1,
		}},
		jobs: []cibackend.Job{{
			Key: "41", Run: "17", Name: "unit", HeadSHA: headSHA, Status: "completed", Conclusion: "success",
		}},
	}
	bindParityCI(t, h, repo, backend)

	checks := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/commits/%s/check-runs", repo.FullName, headSHA), nil)
	assertStatusCode(t, checks, 200)
	body := testharness.DecodeJSON(t, checks)
	rows, _ := body["check_runs"].([]any)
	if len(rows) != 1 {
		t.Fatalf("check_runs=%d body=%s", len(rows), checks.Body.String())
	}
	row, _ := rows[0].(map[string]any)
	if row["name"] != "unit" || row["head_sha"] != headSHA || row["conclusion"] != "success" {
		t.Fatalf("check run shape: %#v", row)
	}

	branch := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/commits/%s/check-runs", repo.FullName, repo.DefaultBranch), nil)
	assertStatusCode(t, branch, 200)

	runs := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/actions/runs?head_sha=%s", repo.FullName, headSHA), nil)
	assertStatusCode(t, runs, 200)
	runBody := testharness.DecodeJSON(t, runs)
	listed, _ := runBody["workflow_runs"].([]any)
	if len(listed) != 1 {
		t.Fatalf("runs=%s", runs.Body.String())
	}
	runID := int(listed[0].(map[string]any)["id"].(float64))
	// CheckSuite identifies the parent run, never the job backing this check.
	suite := row["check_suite"].(map[string]any)
	if int(suite["id"].(float64)) != runID {
		t.Fatalf("check suite did not identify parent run: %s", checks.Body.String())
	}
	detail := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/actions/runs/%d", repo.FullName, runID), nil)
	assertStatusCode(t, detail, 200)
	if testharness.DecodeJSON(t, detail)["head_sha"] != headSHA {
		t.Fatal("check's parent run lost its head SHA")
	}
	artifacts := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/actions/runs/%d/artifacts", repo.FullName, runID), nil)
	assertStatusCode(t, artifacts, 200)
	artifactBody := testharness.DecodeJSON(t, artifacts)
	if int(artifactBody["total_count"].(float64)) != 0 || artifactBody["artifacts"] == nil {
		t.Fatalf("artifacts=%s", artifacts.Body.String())
	}
	if _, ok := artifactBody["artifacts"].([]any); !ok {
		t.Fatalf("artifacts is not a list: %s", artifacts.Body.String())
	}
	repoArtifacts := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/actions/artifacts", repo.FullName), nil)
	assertStatusCode(t, repoArtifacts, 200)
	if !strings.Contains(repoArtifacts.Body.String(), `"artifacts":[]`) && !strings.Contains(repoArtifacts.Body.String(), `"artifacts": []`) {
		t.Fatalf("repo artifacts=%s", repoArtifacts.Body.String())
	}
	download := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/actions/artifacts/1/zip", repo.FullName), nil)
	if download.Code != http.StatusNotImplemented {
		t.Fatalf("download status=%d body=%s", download.Code, download.Body.String())
	}
}
