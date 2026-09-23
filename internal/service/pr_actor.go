package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/executioncontext"
	"github.com/ngaut/agent-git-service/internal/workloadidentity"
)

// PullRequestAGSActor is the immutable workload snapshot displayed for a PR
// created through a delegated session. It intentionally excludes assertion,
// credential, fingerprint, and policy-snapshot material.
type PullRequestContextAttributionUser struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type PullRequestAGSActor struct {
	Type                string                             `json:"type"`
	Provider            string                             `json:"provider"`
	WorkspaceID         string                             `json:"workspace_id"`
	Workspace           string                             `json:"workspace,omitempty"`
	AgentID             string                             `json:"agent_id"`
	AgentName           string                             `json:"agent_name"`
	TaskID              string                             `json:"task_id"`
	RunID               string                             `json:"run_id,omitempty"`
	IssueID             string                             `json:"issue_id,omitempty"`
	IssueKey            string                             `json:"issue_key,omitempty"`
	TriggerID           string                             `json:"trigger_id,omitempty"`
	RuntimeID           string                             `json:"runtime_id,omitempty"`
	Role                string                             `json:"role,omitempty"`
	IssuerInstanceID    string                             `json:"issuer_instance_id,omitempty"`
	IssuerSubject       string                             `json:"issuer_subject,omitempty"`
	BindingID           string                             `json:"binding_id,omitempty"`
	TraceQuality        string                             `json:"trace_quality,omitempty"`
	MissingFields       []string                           `json:"missing_fields,omitempty"`
	Operation           string                             `json:"operation,omitempty"`
	SessionID           string                             `json:"session_id"`
	SessionState        string                             `json:"session_state"`
	SessionCreatedAt    time.Time                          `json:"session_created_at"`
	TargetInstance      string                             `json:"target_instance"`
	DisplayName         string                             `json:"display_name"`
	AttributionSource   string                             `json:"attribution_source,omitempty"`
	AttributionPrecise  bool                               `json:"attribution_precise"`
	Initiator           *PullRequestContextAttributionUser `json:"initiator,omitempty"`
	Originator          *PullRequestContextAttributionUser `json:"originator,omitempty"`
	DelegatedFromTaskID string                             `json:"delegated_from_task_id,omitempty"`
	RetryOfTaskID       string                             `json:"retry_of_task_id,omitempty"`
	RerunOfTaskID       string                             `json:"rerun_of_task_id,omitempty"`
}

type PullRequestDelegatorIdentity struct {
	ID       uint   `json:"id"`
	Login    string `json:"login"`
	UserKind string `json:"user_kind,omitempty"`
}

// PullRequestDelegatedBy separates authorization identity from display actor.
// Principal is always the AGS authorization subject. Human is an AGS-owned
// binding snapshot; BindingSource distinguishes issuance from migration.
type PullRequestDelegatedBy struct {
	Principal     PullRequestDelegatorIdentity  `json:"principal"`
	Human         *PullRequestDelegatorIdentity `json:"human"`
	BindingSource string                        `json:"binding_source"`
}

type PullRequestAttribution struct {
	AGSActor    PullRequestAGSActor
	DelegatedBy PullRequestDelegatedBy
}

// PullRequestAttributionFor returns nil for durable PRs and a secret-safe
// historical projection for delegated PRs.
func (s *Service) PullRequestAttributionFor(ctx context.Context, pr db.PullRequest) (*PullRequestAttribution, error) {
	if pr.AgentSessionID == nil || strings.TrimSpace(*pr.AgentSessionID) == "" {
		return nil, nil
	}
	session := pr.AgentSession
	if session == nil || session.ID == "" || session.ID != *pr.AgentSessionID {
		var loaded db.DelegatedAgentSession
		if err := s.DBForCtx(ctx).Preload("PrincipalUser").Preload("Repository").First(&loaded, "id = ?", *pr.AgentSessionID).Error; err != nil {
			return nil, fmt.Errorf("load delegated PR session: %w", err)
		}
		session = &loaded
	} else {
		if session.PrincipalUser.ID == 0 || session.PrincipalUser.ID != session.PrincipalUserID {
			if err := s.DBForCtx(ctx).First(&session.PrincipalUser, "id = ?", session.PrincipalUserID).Error; err != nil {
				return nil, fmt.Errorf("load delegated PR principal: %w", err)
			}
		}
		if session.Repository.ID == 0 && isAccessGrantTransportSession(*session) {
			if err := s.DBForCtx(ctx).First(&session.Repository, "id = ?", session.RepositoryID).Error; err != nil {
				return nil, fmt.Errorf("load delegated PR repository: %w", err)
			}
		}
	}

	// Provider is the stable workload source type. The deployment-unique
	// verification issuer remains separately available as IssuerInstanceID and
	// must not be projected as a provider selector.
	provider := workloadidentity.DefaultIssuer
	principalLogin := strings.TrimSpace(session.PrincipalLogin)
	if principalLogin == "" {
		principalLogin = session.PrincipalUser.Login
	}
	delegatorLogin := principalLogin
	bindingSource := db.DelegatedBySourcePrincipalOnly
	var human *PullRequestDelegatorIdentity
	if session.DelegatedByUserID != nil || strings.TrimSpace(session.DelegatedByLogin) != "" {
		human = &PullRequestDelegatorIdentity{Login: session.DelegatedByLogin, UserKind: db.UserKindHuman}
		if session.DelegatedByUserID != nil {
			human.ID = *session.DelegatedByUserID
		}
		if human.Login != "" {
			delegatorLogin = human.Login
		}
		bindingSource = strings.TrimSpace(session.DelegatedBySource)
		if bindingSource == "" {
			bindingSource = db.DelegatedBySourceSessionSnapshot
		}
	}
	actorType, actorLabel := "multica_agent", "Multica Agent"
	var transportContext *accessGrantPRContext
	if isAccessGrantTransportSession(*session) {
		actorType, actorLabel = "runtime_agent", "Runtime Agent"
		if loaded, err := s.accessGrantPRContextForSession(ctx, *session); err == nil {
			transportContext = &loaded
		}
	}
	actorName := strings.TrimSpace(session.ExternalAgentName)
	if actorName == "" {
		actorName = strings.TrimSpace(session.ExternalAgentID)
	}
	actorName = strings.Join(strings.Fields(actorName), " ")
	display := actorName + " [" + actorLabel + "]"
	if delegator := strings.Join(strings.Fields(delegatorLogin), " "); delegator != "" {
		display += " via " + delegator
	}
	if target := strings.Join(strings.Fields(session.TargetInstance), " "); target != "" {
		display += " · " + target
	}
	actor := PullRequestAGSActor{
		Type: actorType, Provider: provider, WorkspaceID: session.IssuerWorkspaceID, Workspace: session.IssuerWorkspace,
		AgentID: session.ExternalAgentID, AgentName: session.ExternalAgentName, TaskID: session.ExternalTaskID,
		RunID: session.ExternalRunID, IssueID: session.ExternalIssueID, IssueKey: session.ExternalIssueKey,
		TriggerID: session.ExternalTriggerID, RuntimeID: session.ExternalRuntimeID, Role: session.ExternalRole,
		IssuerInstanceID: session.IssuerInstanceID, IssuerSubject: session.IssuerSubject,
		BindingID:    session.BindingID,
		TraceQuality: session.TraceQuality, MissingFields: append([]string(nil), session.MissingFields...), Operation: session.OperationName,
		SessionID: session.ID, SessionState: delegatedSessionState(*session, time.Now().UTC()),
		SessionCreatedAt: session.CreatedAt, TargetInstance: session.TargetInstance, DisplayName: display,
	}
	if transportContext != nil && transportContext.Current.Attribution != nil {
		attribution := transportContext.Current.Attribution
		actor.AttributionSource = strings.TrimSpace(attribution.Source)
		actor.AttributionPrecise = attribution.Precise
		actor.Initiator = pullRequestContextAttributionUser(attribution.Initiator)
		actor.Originator = pullRequestContextAttributionUser(attribution.Originator)
		actor.DelegatedFromTaskID = strings.TrimSpace(attribution.DelegatedFromTaskID)
		actor.RetryOfTaskID = strings.TrimSpace(attribution.RetryOfTaskID)
		actor.RerunOfTaskID = strings.TrimSpace(attribution.RerunOfTaskID)
	}
	return &PullRequestAttribution{
		AGSActor: actor,
		DelegatedBy: PullRequestDelegatedBy{
			Principal: PullRequestDelegatorIdentity{ID: session.PrincipalUserID, Login: principalLogin, UserKind: session.PrincipalUser.UserKind},
			Human:     human, BindingSource: bindingSource,
		},
	}, nil
}

func pullRequestContextAttributionUser(user *executioncontext.AttributionUser) *PullRequestContextAttributionUser {
	if user == nil {
		return nil
	}
	id := strings.TrimSpace(user.ID)
	name := strings.Join(strings.Fields(user.Name), " ")
	if id == "" && name == "" {
		return nil
	}
	return &PullRequestContextAttributionUser{ID: id, Name: name}
}
