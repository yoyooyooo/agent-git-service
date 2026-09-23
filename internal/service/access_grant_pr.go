package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/executioncontext"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
)

// accessGrantPRContext is loaded only from the immutable snapshot bound to the
// current transport Session. Caller-provided PR text never participates in the
// association or attribution decision.
type accessGrantPRContext struct {
	Grant    db.AccessGrant
	Snapshot db.ExecutionContextSnapshot
	Source   executioncontext.SourceRef
	Current  executioncontext.CurrentContext
}

func isAccessGrantTransportSession(session db.DelegatedAgentSession) bool {
	return session.CredentialMode == accessGrantTransportCredentialMode && strings.TrimSpace(session.AccessGrantID) != "" && session.ActorUserID != 0
}

func (s *Service) accessGrantPRAuthor(ctx context.Context, session db.DelegatedAgentSession) (db.User, error) {
	if !isAccessGrantTransportSession(session) {
		return db.User{}, fmt.Errorf("access grant transport Session is required")
	}
	var actor db.User
	if err := s.DBForCtx(ctx).First(&actor, "id = ?", session.ActorUserID).Error; err != nil {
		return db.User{}, err
	}
	if actor.ID == 0 || !isUserStatusActive(actor.Status) {
		return db.User{}, ErrForbidden
	}
	return actor, nil
}

func (s *Service) accessGrantPRContextForSession(ctx context.Context, session db.DelegatedAgentSession) (accessGrantPRContext, error) {
	if !isAccessGrantTransportSession(session) {
		return accessGrantPRContext{}, fmt.Errorf("access grant transport Session is required")
	}
	var grant db.AccessGrant
	if err := s.DBForCtx(ctx).First(&grant, "id = ?", session.AccessGrantID).Error; err != nil {
		return accessGrantPRContext{}, err
	}
	record, source, current, err := s.GetExecutionContextSnapshot(ctx, grant.SnapshotID)
	if err != nil {
		return accessGrantPRContext{}, err
	}
	if grant.ID != session.AccessGrantID || grant.ActorUserID != session.ActorUserID || grant.ExecutorUserID != session.PrincipalUserID ||
		grant.RepositoryID != session.RepositoryID || grant.RepositoryFullName != session.Repository.FullName ||
		grant.SourceInstanceID != source.SourceInstanceID || grant.ExternalWorkspaceID != source.WorkspaceID ||
		grant.ExternalAgentID != source.AgentID || grant.ExternalTaskID != source.TaskID || grant.ExternalRunID != source.RunID ||
		current.Workspace.ID != source.WorkspaceID || current.Agent.ID != source.AgentID || current.Task.ID != source.TaskID || current.Run.ID != source.RunID {
		return accessGrantPRContext{}, ErrAccessGrantConflict
	}
	return accessGrantPRContext{Grant: grant, Snapshot: record, Source: source, Current: current}, nil
}

func (s *Service) enrichPRBodyWithAccessGrantContext(body string, context accessGrantPRContext) string {
	current := context.Current
	workspace := strings.Trim(strings.ToLower(strings.TrimSpace(current.Workspace.Slug)), "/")
	if workspace == "" {
		workspace = strings.Trim(strings.ToLower(strings.TrimSpace(current.Workspace.Name)), "/")
	}
	if current.Issue != nil {
		issueURL := ""
		if s != nil && s.MulticaProjection != nil && workspace != "" {
			issueURL = multicaprojection.IssueURL(s.MulticaProjection.AppURL(), workspace, current.Issue.Key)
		}
		body = enrichBodyWithAuthoritativeMulticaLink(body, multicaprojection.PRLinkClaims{
			Workspace: workspace, WorkspaceID: current.Workspace.ID,
			IssueID: current.Issue.ID, IssueKey: current.Issue.Key, IssueURL: issueURL,
		})
	}
	lines := make([]string, 0, 5)
	actorName := compactPRContextText(current.Agent.Name)
	if actorName == "" {
		actorName = current.Agent.ID
	}
	lines = append(lines, "AGS actor: "+actorName+" ("+current.Agent.ID+")")
	contextRef := strings.TrimSpace(context.Source.SourceInstanceID)
	if workspace != "" {
		contextRef += "/" + workspace
	}
	if current.Issue != nil && strings.TrimSpace(current.Issue.Key) != "" {
		contextRef += "/" + strings.ToUpper(strings.TrimSpace(current.Issue.Key))
	}
	if contextRef != "" {
		lines = append(lines, "Execution context: "+contextRef)
	}
	if current.Attribution != nil {
		if value := formatPRAttributionUser(current.Attribution.Initiator); value != "" {
			lines = append(lines, "Initiated by: "+value)
		}
		if value := formatPRAttributionUser(current.Attribution.Originator); value != "" {
			lines = append(lines, "Originated by: "+value)
		}
		if value := compactPRContextText(current.Attribution.DelegatedFromTaskID); value != "" {
			lines = append(lines, "Delegated from task: "+value)
		}
	}
	return appendUniquePRBodyLines(body, lines)
}

func (s *Service) accessGrantPRIssueURL(context accessGrantPRContext) string {
	if s == nil || s.MulticaProjection == nil || context.Current.Issue == nil {
		return ""
	}
	workspace := strings.Trim(strings.ToLower(strings.TrimSpace(context.Current.Workspace.Slug)), "/")
	if workspace == "" {
		workspace = strings.Trim(strings.ToLower(strings.TrimSpace(context.Current.Workspace.Name)), "/")
	}
	if workspace == "" || strings.TrimSpace(context.Current.Issue.Key) == "" {
		return ""
	}
	return multicaprojection.IssueURL(s.MulticaProjection.AppURL(), workspace, context.Current.Issue.Key)
}

func accessGrantSnapshotMulticaLink(prID, repositoryID uint, context accessGrantPRContext, issueURL string) (db.PullRequestMulticaLink, bool) {
	current := context.Current
	if current.Issue == nil || strings.TrimSpace(current.Issue.ID) == "" || strings.TrimSpace(current.Issue.Key) == "" {
		return db.PullRequestMulticaLink{}, false
	}
	workspace := strings.Trim(strings.ToLower(strings.TrimSpace(current.Workspace.Slug)), "/")
	if workspace == "" {
		workspace = strings.Trim(strings.ToLower(strings.TrimSpace(current.Workspace.Name)), "/")
	}
	return db.PullRequestMulticaLink{
		PullRequestID: prID, RepositoryID: repositoryID,
		Workspace: workspace, WorkspaceID: current.Workspace.ID,
		IssueID: current.Issue.ID, IssueKey: strings.ToUpper(strings.TrimSpace(current.Issue.Key)), IssueURL: strings.TrimSpace(issueURL),
		TaskID: current.Task.ID, AgentID: current.Agent.ID, RunID: current.Run.ID,
		AccessGrantID: context.Grant.ID, SourceSnapshotID: context.Snapshot.ID,
		Confidence: db.MulticaLinkConfidenceAuthoritative, Source: db.MulticaLinkSourceAccessGrantSnapshot,
		CompletionIntent: true,
	}, true
}

func compactPRContextText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func formatPRAttributionUser(user *executioncontext.AttributionUser) string {
	if user == nil {
		return ""
	}
	name := compactPRContextText(user.Name)
	id := compactPRContextText(user.ID)
	switch {
	case name != "" && id != "":
		return name + " (" + id + ")"
	case name != "":
		return name
	default:
		return id
	}
}

func appendUniquePRBodyLines(body string, lines []string) string {
	body = strings.TrimSpace(body)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(body, line) {
			continue
		}
		if body != "" {
			body += "\n\n"
		}
		body += line
	}
	return body
}
