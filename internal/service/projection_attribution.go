package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/attribution"
	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/gorm"
)

// ProjectionAttributionRecorder writes provenance without creating an authorized
// service-user context. Worker attribution must never become authority.
func ProjectionAttributionRecorder(database *gorm.DB) func(context.Context, attribution.Event) error {
	return func(ctx context.Context, event attribution.Event) error {
		gdb := database
		if scoped, ok := DBFromContext(ctx); ok {
			gdb = scoped
		}
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		row := db.AuditLogEntry{Action: "projection." + event.Op, ActorLogin: event.Actor.Login, RepositoryFullName: event.Actor.Repo, TargetLogin: firstNonEmpty(event.TargetLogin, event.SudoAs), Details: string(payload), CreatedAt: time.Now().UTC()}
		if event.Actor.UserID != 0 {
			id := event.Actor.UserID
			row.ActorID = &id
		}
		return gdb.WithContext(ctx).Create(&row).Error
	}
}

// AuthorizeProjectionAttribution projects AGS authority, not an independent
// Forgejo permission model. It reads current AGS grants without cache/backfill;
// persisted provenance selects the identity to check, never the answer. In
// particular, a nonzero actor id or a mapped provider login is not admission.
func (s *Service) AuthorizeProjectionAttribution(ctx context.Context, actor attribution.Actor, repoFullName, op string, targetPR int) error {
	gdb := s.DBForCtx(ctx)

	var user db.User
	if actor.UserID == 0 || gdb.WithContext(ctx).First(&user, "id = ? AND login = ?", actor.UserID, actor.Login).Error != nil || !isUserStatusActive(user.Status) {
		return ErrForbidden
	}
	if viewer, ok := UserFromContext(ctx); ok && viewer.ID != actor.UserID {
		return ErrForbidden
	}
	var repo db.Repository
	if repoFullName == "" || (actor.Repo != "" && actor.Repo != repoFullName) || gdb.WithContext(ctx).Where("full_name = ?", repoFullName).First(&repo).Error != nil || repo.Disabled {
		return ErrForbidden
	}

	switch op {
	case "create_pr", "edit_pr", "close_pr", "reopen_pr", "comment", "label_add", "label_remove":
	default:
		return ErrForbidden
	}
	permission, err := s.currentRepoAccess(ctx, repo.ID, user.ID)
	if err != nil {
		return err
	}
	if !permission.AtLeast(RepoPermissionWrite) {
		return ErrForbidden
	}
	// Existing action authority remains the final authority for its exact bound
	// effect; do not let generic repo write replace a narrower action admission.
	if _, bound := forgejoActionBindingFromContext(ctx); bound {
		return s.revalidateBoundActionProviderWrite(ctx)
	}
	sessionID, delegated := DelegatedSessionIDFromContext(ctx)
	if persisted, ok := ctx.Value(projectionAttributionSessionKey{}).(string); ok && persisted != "" {
		sessionID, delegated = persisted, true
	}
	if delegated {
		var session db.DelegatedAgentSession
		if gdb.WithContext(ctx).Select("id", "operation_name").First(&session, "id = ?", sessionID).Error != nil {
			return ErrForbidden
		}
		operation, capability := "", ""
		switch op {
		case "create_pr":
			operation, capability = "pr.create", "pr:create"
		case "edit_pr":
			operation, capability = "pr.edit", "pr:edit"
		case "close_pr":
			operation, capability = "pr.close", "pr:close"
		case "reopen_pr":
			operation, capability = "pr.reopen", "pr:reopen"
		case "comment":
			operation, capability = "pr.comment", "pr:comment"
		default:
			return ErrForbidden
		}
		constraints := map[string]string{}
		if actor.PR > 0 {
			var pr db.PullRequest
			if gdb.WithContext(ctx).Where("repository_id = ? AND number = ?", repo.ID, actor.PR).First(&pr).Error != nil {
				return ErrForbidden
			}
			constraints = map[string]string{"pull_request_number": fmt.Sprint(pr.Number), "head_ref": pr.HeadRef, "base_ref": pr.BaseRef}
		}
		// A create admission can reconcile its own generated metadata during ensure.
		if op == "edit_pr" && session.OperationName == "pr.create" {
			operation, capability = "pr.create", "pr:create"
		}
		checkCtx := ContextWithDelegatedSession(ContextWithUser(ctx, user), db.DelegatedAgentSession{ID: sessionID})
		_, err := s.RevalidateDelegatedSession(checkCtx, repo.ID, operation, capability, constraints)
		return err
	}
	return nil
}

type projectionAttributionSessionKey struct{}

// ValidateProjectionCommitEmails checks known AGS users without modifying any
// identity. An explicit email assertion requires the AGS user to exist; provider
// linkage is independently checked by forgejointegration.ValidateAttribution.
func ValidateProjectionCommitEmails(ctx context.Context, database *gorm.DB, cfg attribution.Config) error {
	for login, p := range cfg.Principals {
		if p.CommitEmail == "" {
			continue
		}
		var user db.User
		err := database.WithContext(ctx).Where("login = ?", login).First(&user).Error

		if err != nil {
			return err
		}
		if !strings.EqualFold(user.Email, p.CommitEmail) {
			return fmt.Errorf("attribution commit_email does not match AGS user %s", login)
		}
	}
	return nil
}
