package graphql

import (
	"context"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestPRGQLProjectsDelegatedActorWithoutReplacingAuthor(t *testing.T) {
	server := setupTestServer(t)
	human := db.User{ID: 1, Login: "operator", Name: "Operator", Type: db.TypeUser, UserKind: db.UserKindHuman}
	principal := db.User{ID: 4, Login: "automation-principal", Name: "Automation Principal", Type: db.TypeUser, UserKind: db.UserKindAgent}
	humanID := human.ID
	created := time.Date(2026, 7, 14, 13, 30, 0, 0, time.UTC)
	session := db.DelegatedAgentSession{
		ID: "session-1", PrincipalUserID: principal.ID, PrincipalLogin: principal.Login, PrincipalUser: principal,
		DelegatedByUserID: &humanID, DelegatedByLogin: human.Login, DelegatedBySource: "session_snapshot", Issuer: "multica", IssuerWorkspaceID: "workspace-1",
		IssuerWorkspace: "example-workspace", ExternalAgentID: "agent-1", ExternalAgentName: "example-implementer", ExternalTaskID: "task-1",
		ExternalIssueID: "issue-1", ExternalIssueKey: "TASK-519", TargetInstance: "primary-a", CreatedAt: created, ExpiresAt: created.Add(30 * time.Minute),
	}
	repo := db.Repository{ID: 7, Name: "project-kit", FullName: "operator/project-kit", OwnerID: human.ID, Owner: human, DefaultBranch: "main"}
	pr := db.PullRequest{
		ID: 42, Number: 4, Title: "Delegated PR", State: db.StateOpen, Author: principal, AuthorID: principal.ID,
		RepositoryID: repo.ID, Repository: repo, HeadRepositoryID: repo.ID, HeadRepository: repo,
		HeadRef: "agent/task-541", BaseRef: "main", AgentSessionID: &session.ID, AgentSession: &session, CreatedAt: created, UpdatedAt: created,
	}
	shape := server.prGQL(context.Background(), pr, "agsActor delegatedBy")
	author := shape["author"].(map[string]any)
	actor := shape["agsActor"].(map[string]any)
	delegatedBy := shape["delegatedBy"].(map[string]any)
	if author["login"] != "automation-principal" || actor["agentName"] != "example-implementer" || delegatedBy["bindingSource"] != "session_snapshot" {
		t.Fatalf("delegated GraphQL PR shape mismatch: %#v", shape)
	}

	pr.AgentSessionID, pr.AgentSession = nil, nil
	durable := server.prGQL(context.Background(), pr, "agsActor delegatedBy")
	if durable["agsActor"] != nil || durable["delegatedBy"] != nil {
		t.Fatalf("durable GraphQL PR attribution must stay null: %#v", durable)
	}
}

func TestPullRequestAttributionGQLUsesCamelCaseAndKeepsPrincipalSeparate(t *testing.T) {
	created := time.Date(2026, 7, 14, 13, 30, 0, 0, time.UTC)
	attribution := &service.PullRequestAttribution{
		AGSActor: service.PullRequestAGSActor{
			Type: "multica_agent", Provider: "multica", WorkspaceID: "workspace-1", Workspace: "example-workspace",
			AgentID: "agent-1", AgentName: "example-implementer", TaskID: "task-1",
			IssueID: "issue-1", IssueKey: "TASK-519", SessionID: "session-1",
			SessionState: "revoked", SessionCreatedAt: created, TargetInstance: "primary-a",
			DisplayName: "example-implementer [Multica Agent] via operator · primary-a",
		},
		DelegatedBy: service.PullRequestDelegatedBy{
			Principal:     service.PullRequestDelegatorIdentity{ID: 4, Login: "automation-principal", UserKind: "agent"},
			Human:         &service.PullRequestDelegatorIdentity{ID: 1, Login: "operator", UserKind: "human"},
			BindingSource: "session_snapshot",
		},
	}
	actorValue, delegatedByValue := pullRequestAttributionGQL(attribution)
	actor := actorValue.(map[string]any)
	if actor["workspaceId"] != "workspace-1" || actor["workspace"] != "example-workspace" || actor["agentName"] != "example-implementer" || actor["sessionState"] != "revoked" {
		t.Fatalf("GraphQL actor mismatch: %#v", actor)
	}
	if _, snakeCase := actor["workspace_id"]; snakeCase {
		t.Fatalf("GraphQL actor exposed snake_case field: %#v", actor)
	}
	delegatedBy := delegatedByValue.(map[string]any)
	principal := delegatedBy["principal"].(map[string]any)
	human := delegatedBy["human"].(map[string]any)
	if principal["login"] != "automation-principal" || human["login"] != "operator" || delegatedBy["bindingSource"] != "session_snapshot" {
		t.Fatalf("GraphQL delegatedBy mismatch: %#v", delegatedBy)
	}
}
