package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestCreatePRBindsAuthoritativeMulticaExternalPRToken(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	setupRepoForTest(t, svc, "alice", "demo")
	svc.MulticaProjection = multicaprojection.New(multicaprojection.Config{
		Enabled:         true,
		AppURL:          "https://multica.example",
		LinkTokenSecret: "secret",
	})

	pr, err := svc.CreatePR(context.Background(), service.CreatePRInput{
		RepoFullName: "alice/demo",
		Title:        "Implement leaf task",
		Body:         "Implementation notes.\n\n<!-- multica-external-pr-link-token: " + signServiceTestPRLinkToken(t, "secret") + " -->",
		HeadRef:      "feature/leaf-task",
		BaseRef:      "main",
		AuthorLogin:  "alice",
	})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if strings.Contains(string(pr.Body), "multica-external-pr-link-token") {
		t.Fatalf("CreatePR() leaked hidden Multica token in PR body: %s", pr.Body)
	}
	if !strings.Contains(string(pr.Body), "Multica: workspace-alpha/HUM-42") {
		t.Fatalf("CreatePR() did not add canonical marker: %s", pr.Body)
	}
	if !strings.Contains(string(pr.Body), "https://multica.example/workspace-alpha/issues/HUM-42") {
		t.Fatalf("CreatePR() did not add issue URL: %s", pr.Body)
	}

	var link db.PullRequestMulticaLink
	if err := svc.DB.Where("pull_request_id = ?", pr.ID).First(&link).Error; err != nil {
		t.Fatalf("Multica link row missing: %v", err)
	}
	if link.Confidence != db.MulticaLinkConfidenceAuthoritative || link.Source != db.MulticaLinkSourceTaskToken || !link.CompletionIntent {
		t.Fatalf("link authority fields = %#v", link)
	}
	if link.Workspace != "workspace-alpha" || link.WorkspaceID != "workspace-id" || link.IssueID != "issue-id" || link.IssueKey != "HUM-42" {
		t.Fatalf("link identity fields = %#v", link)
	}
}

func signServiceTestPRLinkToken(t *testing.T, secret string) string {
	t.Helper()
	now := time.Now()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"aud":          "external-pr-link",
		"iat":          now.Unix(),
		"exp":          now.Add(time.Minute).Unix(),
		"source":       "task_token",
		"workspace":    "workspace-alpha",
		"workspace_id": "workspace-id",
		"issue_id":     "issue-id",
		"issue_key":    "HUM-42",
		"task_id":      "task-id",
		"agent_id":     "agent-id",
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return token
}
