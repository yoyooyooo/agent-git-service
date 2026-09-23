package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestForgejoProvenanceValueIsSingleLine(t *testing.T) {
	if got := forgejoProvenanceValue("agent\n- forged: value\tend"); got != "agent - forged: value end" {
		t.Fatalf("forgejoProvenanceValue=%q", got)
	}
}

func TestForgejoPullRequestBodyIncludesExternalLinksForAuthoritativeMulticaLink(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.Migrate(gdb); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := &Service{
		DB:      gdb,
		BaseURL: "http://primary.example.test:6666",
		MulticaProjection: multicaprojection.New(multicaprojection.Config{
			Enabled:           true,
			AppURL:            "https://primary.example.test:37445",
			CompletionOnMerge: multicaprojection.CompletionOnMergeConfig{Enabled: true, Mode: "leaf_child_only"},
		}),
	}
	human := db.User{Login: "operator", Name: "Operator", Type: db.TypeUser, UserKind: db.UserKindHuman, Status: db.UserStatusActive}
	principal := db.User{Login: "automation-principal", Name: "Automation Principal", Type: db.TypeUser, UserKind: db.UserKindAgent, Status: db.UserStatusActive}
	if err := gdb.Create(&human).Error; err != nil {
		t.Fatalf("create human: %v", err)
	}
	if err := gdb.Create(&principal).Error; err != nil {
		t.Fatalf("create principal: %v", err)
	}
	humanID := human.ID
	session := db.DelegatedAgentSession{
		ID: "session-actor-1", CredentialHash: strings.Repeat("a", 64), CredentialPrefix: "prefix",
		PrincipalUserID: principal.ID, PrincipalLogin: principal.Login, PrincipalUser: principal, Issuer: "multica", AssertionVersion: 1,
		AssertionPurpose: "ags_session_exchange", AssertionJTI: "session-actor-jti", AssertionAudience: "urn:ags:workload-session-exchange:v1",
		IssuerWorkspaceID: "ws-mini", IssuerWorkspace: "example-workspace", ExternalAgentID: "agent-1", ExternalAgentName: "example-implementer-a", ExternalTaskID: "task-1",
		ExternalIssueID: "issue-379", ExternalIssueKey: "MINI-379", TargetInstance: "primary-a", RepositoryID: 7,
		GrantedCapabilities: []string{"pr:create", "repo:read"}, PolicyVersion: "2026-07-14.1", PolicySnapshotHash: strings.Repeat("b", 64),
		DelegatedByUserID: &humanID, DelegatedByLogin: human.Login, DelegatedBySource: "session_snapshot", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(30 * time.Minute),
	}
	pr := db.PullRequest{ID: 42, RepositoryID: 7, Number: 4, Body: db.LargeText("Implements the change.\n\nMultica: primary-a/MINI-379"), AgentSessionID: &session.ID, AgentSession: &session}
	if err := gdb.Create(&db.PullRequestMulticaLink{
		PullRequestID:    pr.ID,
		RepositoryID:     pr.RepositoryID,
		Workspace:        "primary-a",
		WorkspaceID:      "ws-mini",
		IssueID:          "issue-379",
		IssueKey:         "MINI-379",
		IssueURL:         "https://primary.example.test:37445/primary-a/issues/MINI-379",
		Confidence:       db.MulticaLinkConfidenceAuthoritative,
		Source:           db.MulticaLinkSourceTaskToken,
		CompletionIntent: true,
	}).Error; err != nil {
		t.Fatalf("create link: %v", err)
	}

	body := svc.forgejoPullRequestBody(context.Background(), "operator/team-share-fixture", pr)
	for _, want := range []string{
		"Created from AGS PR #4: http://primary.example.test:6666/operator/team-share-fixture/pull/4",
		"Multica: primary-a/MINI-379",
		"External links:",
		"- AGS PR: http://primary.example.test:6666/operator/team-share-fixture/pull/4",
		"- Multica issue: primary-a/MINI-379",
		"- Multica issue URL: https://primary.example.test:37445/primary-a/issues/MINI-379",
		"- Multica link: authoritative via task_token",
		"- Completion integration: Multica-owned on merge",
		"<!-- ags-delegated-workload:start -->",
		"Delegated workload:",
		"- Actor: example-implementer-a [Multica Agent]",
		"- Agent ID: agent-1",
		"- Delegated by: operator via AGS principal automation-principal",
		"- Target: Multica workspace example-workspace (ws-mini) -> AGS primary-a",
		"- Task: task-1",
		"- Issue: MINI-379 (issue-379)",
		"- Session: session-actor-1",
		"<!-- ags-delegated-workload:end -->",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if actorIndex, bodyIndex := strings.Index(body, "<!-- ags-delegated-workload:start -->"), strings.Index(body, "Implements the change."); actorIndex < 0 || bodyIndex < 0 || actorIndex > bodyIndex {
		t.Fatalf("authoritative delegated section must precede user body:\n%s", body)
	}
	for _, forbidden := range []string{"session-actor-jti", strings.Repeat("a", 64), "AssertionJTI", "credential"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("body leaked delegated credential metadata %q:\n%s", forbidden, body)
		}
	}

	durableBody := svc.forgejoPullRequestBody(context.Background(), "operator/team-share-fixture", db.PullRequest{ID: 43, RepositoryID: 7, Number: 5, Body: db.LargeText("Durable PR")})
	if strings.Contains(durableBody, "Delegated workload:") {
		t.Fatalf("durable PR received delegated provenance:\n%s", durableBody)
	}
}
