package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

type providerEvidenceForgejoClient struct {
	runs []forgejointegration.WorkflowRun
}

func (f *providerEvidenceForgejoClient) EnsureRepository(context.Context, string, string, bool) error {
	return nil
}

func (f *providerEvidenceForgejoClient) EnsurePullRequest(context.Context, forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}

func (f *providerEvidenceForgejoClient) UpdatePullRequestState(context.Context, string, string, int, string) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}

func (f *providerEvidenceForgejoClient) ListWorkflowRuns(context.Context, string, string, int) ([]forgejointegration.WorkflowRun, error) {
	return f.runs, nil
}

func (f *providerEvidenceForgejoClient) GetWorkflowRunLogs(context.Context, string, string, int64) ([]byte, error) {
	return nil, os.ErrNotExist
}

func TestReadProviderCIRunLogsAcceptsExactMergedPullRequest(t *testing.T) {
	svc := setupProjectionStateService(t)
	if err := svc.DB.AutoMigrate(&db.AuditLogEntry{}); err != nil {
		t.Fatalf("migrate audit log: %v", err)
	}
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("read repo: %v", err)
	}
	head := "1383fd076c3f47df627580932b00955065cdb957"
	pr := db.PullRequest{
		Number: 101, RepositoryID: repo.ID, Repository: repo, HeadRepositoryID: repo.ID,
		Title: "merged evidence", State: "closed", Merged: true, HeadRef: "fix/evidence", HeadSHA: head, BaseRef: "main",
	}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatalf("create pull request: %v", err)
	}
	projection := db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo,
		ExternalRepo: "forgejo/demo-ci", ExternalNumber: 98, ExternalURL: "http://forgejo.local/forgejo/demo-ci/pulls/98",
	}
	if err := svc.DB.Create(&projection).Error; err != nil {
		t.Fatalf("create projection: %v", err)
	}
	logRoot := t.TempDir()
	logPath := filepath.Join(logRoot, "forgejo", "demo-ci", "25", "6949.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("checkout git fetch timed out\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	client := &providerEvidenceForgejoClient{runs: []forgejointegration.WorkflowRun{{
		ID: 6949, Name: "Backend Smoke", Status: "failure", HeadBranch: "#98", HeadSHA: head,
	}}}
	enabled := true
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, ActionsLogDir: logRoot,
		RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo-ci", Enabled: &enabled}},
	}, client, nil)
	viewer := db.User{Login: "example-owner", Type: "User"}
	if err := svc.DB.Where("login = ?", viewer.Login).First(&viewer).Error; err != nil {
		t.Fatalf("read viewer: %v", err)
	}
	ctx := ContextWithUser(context.Background(), viewer)

	got, err := svc.ReadProviderCIRunLogs(ctx, pr, 6949)
	if err != nil {
		t.Fatalf("ReadProviderCIRunLogs: %v", err)
	}
	if got.Schema != ProviderCIRunLogsSchema || got.AGSPR != 101 || got.RunID != 6949 || got.Text != "checkout git fetch timed out\n" {
		t.Fatalf("unexpected merged PR log evidence: %#v", got)
	}
}

func TestProviderWorkflowRunObservationsBindSourceRefToPullRequest(t *testing.T) {
	head := "1383fd076c3f47df627580932b00955065cdb957"
	runs := []forgejointegration.WorkflowRun{
		{ID: 1, Name: "app-build-test", WorkflowID: "app-ci.yml", Status: "failure", Event: "pull_request", HeadBranch: "#183", HeadSHA: head, URL: "http://forgejo.local/example-owner/demo/actions/runs/17"},
		{ID: 2, Name: "app-build-test", WorkflowID: "app-ci.yml", Status: "success", Event: "workflow_dispatch", HeadBranch: "fed-424-apollo-config-authority", HeadSHA: head},
	}

	observations, err := providerWorkflowRunObservations(runs, 183, "fed-424-apollo-config-authority")
	if err != nil {
		t.Fatalf("providerWorkflowRunObservations: %v", err)
	}
	if len(observations) != 2 {
		t.Fatalf("observations=%#v, want two runs", observations)
	}
	for _, observation := range observations {
		if observation.PullRequestNumber != 183 || observation.HeadBranch != "#183" {
			t.Fatalf("observation=%#v, want authoritative PR binding", observation)
		}
	}
	if observations[0].HTMLURL != "http://forgejo.local/example-owner/demo/actions/runs/17" {
		t.Fatalf("workflow run display URL=%q", observations[0].HTMLURL)
	}
	if observations[1].SourceHeadBranch != "fed-424-apollo-config-authority" {
		t.Fatalf("workflow dispatch source branch=%q", observations[1].SourceHeadBranch)
	}
}

func TestProviderObservationReceiptReportsAccessGrantTransportAuthentication(t *testing.T) {
	ctx := ContextWithDelegatedSession(context.Background(), db.DelegatedAgentSession{
		ID:                "0cc98cc8-fef5-4d5e-9f82-9413cdf768e3",
		CredentialMode:    accessGrantTransportCredentialMode,
		PrincipalUserID:   55,
		PrincipalLogin:    "fixture-maintainer",
		IssuerInstanceID:  "multica-mini",
		IssuerWorkspaceID: "706cdb59-eb9d-4a08-88cb-6238f048e972",
		ExternalAgentID:   "471eb377-df42-4280-8a16-0cf420f7b0c9",
		ExternalTaskID:    "066fb7bd-cd29-41b2-bc57-42facb7c08e3",
		ExternalRunID:     "9cd0f582-df23-46ed-ab5f-220eef610f90",
		ExternalIssueID:   "d4c40019-8ed3-47e1-a2fd-8454cbd318a9",
		ExternalIssueKey:  "MINI-1542",
		ExternalRuntimeID: "23ab3479-80b3-4144-9532-95865b00ded1",
		CorrelationID:     "1d1b4f25-2930-48ac-837b-4e4424fc71be",
	})
	pr := db.PullRequest{Number: 62, Repository: db.Repository{FullName: "operator/agent-git-service"}}

	receipt := newProviderObservationReceipt(ctx, pr, "pr.read")

	if receipt.Authentication.Mode != accessGrantTransportCredentialMode {
		t.Fatalf("authentication mode = %q, want %q", receipt.Authentication.Mode, accessGrantTransportCredentialMode)
	}
	if receipt.Authentication.PrincipalID != 55 || receipt.Authentication.PrincipalLogin != "fixture-maintainer" {
		t.Fatalf("unexpected authentication principal: %#v", receipt.Authentication)
	}
	if receipt.Authentication.SessionID == "" {
		t.Fatal("expected secret-safe transport Session ID")
	}
	if receipt.Workload.AgentID == "" || receipt.Workload.TaskID == "" || receipt.Workload.RunID == "" {
		t.Fatalf("expected transport workload provenance: %#v", receipt.Workload)
	}
}
