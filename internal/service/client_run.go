package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/executioncontext"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const ClientRunSchema = "ags.client-run.v1"
const clientRunPrefix = "ags_run_"

type ClientRunContext struct {
	Source string `json:"source,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Task   string `json:"task,omitempty"`
	Run    string `json:"run,omitempty"`
}
type ClientRunSourceInput struct {
	Instance     string                   `json:"instance"`
	EndpointHint string                   `json:"endpoint_hint"`
	Locator      executioncontext.Locator `json:"locator"`
	Token        string                   `json:"token"`
}
type ClientRunInput struct {
	// A caller-generated UUID permits cleanup after a lost issue response;
	// it is not a replay token and never returns a bearer a second time.
	ID         string                `json:"id,omitempty"`
	Context    ClientRunContext      `json:"context"`
	TTLSeconds int                   `json:"ttl_seconds"`
	Source     *ClientRunSourceInput `json:"source,omitempty"`
}
type ClientRunReceipt struct {
	Schema            string           `json:"schema"`
	ID                string           `json:"id"`
	ActorID           uint             `json:"actor_id"`
	ActorLogin        string           `json:"actor_login"`
	Context           ClientRunContext `json:"context"`
	AssociationStatus string           `json:"association_status"`
	ContextTrust      string           `json:"context_trust"`
	ExpiresAt         time.Time        `json:"expires_at"`
	Revoked           bool             `json:"revoked"`
	Permissions       string           `json:"permissions"`
}
type ClientRunIssued struct {
	ClientRunReceipt
	Token string `json:"token"`
}
type clientRunContextKey struct{}

func IsClientRunCredential(raw string) bool { return strings.HasPrefix(raw, clientRunPrefix) }
func clientRunHash(raw string) string {
	s := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(s[:])
}
func ContextWithClientRun(ctx context.Context, run db.ClientRunSession) context.Context {
	return context.WithValue(ctx, clientRunContextKey{}, run)
}
func ClientRunFromContext(ctx context.Context) (db.ClientRunSession, bool) {
	value, ok := ctx.Value(clientRunContextKey{}).(db.ClientRunSession)
	return value, ok
}
func clientRunReceipt(run db.ClientRunSession, user db.User) ClientRunReceipt {
	var source ClientRunContext
	_ = json.Unmarshal([]byte(run.ContextJSON), &source)
	trust := "caller_claimed_not_authority"
	if run.AssociationStatus == "linked" && run.SnapshotID != "" {
		trust = "verified_source_bound_to_native_actor"
	}
	return ClientRunReceipt{Schema: ClientRunSchema, ID: run.ID, ActorID: user.ID, ActorLogin: user.Login, Context: source, AssociationStatus: run.AssociationStatus, ContextTrust: trust, ExpiresAt: run.ExpiresAt, Revoked: run.RevokedAt != nil || run.ParentTokenID == nil, Permissions: "current_native_actor_permissions"}
}

// StartClientRun is task-boundary setup, not a per-command permission grant.
// It inherits exactly one native actor; context claims cannot select another
// actor, executor, role, or repository grant. Partial/empty context is valid.
func (s *Service) StartClientRun(ctx context.Context, parentRaw string, input ClientRunInput) (ClientRunIssued, error) {
	actor, e := s.GetCurrentUser(ctx)
	if e != nil {
		return ClientRunIssued{}, e
	}
	if !isUserStatusActive(actor.Status) || actor.IsAnonymous {
		return ClientRunIssued{}, ErrUnauthorized
	}
	if IsClientRunCredential(parentRaw) || IsDelegatedSessionCredential(parentRaw) {
		return ClientRunIssued{}, fmt.Errorf("%w: run setup requires the actor's native credential", ErrForbidden)
	}
	for _, v := range []string{input.Context.Source, input.Context.Agent, input.Context.Task, input.Context.Run} {
		if len(v) > 256 || strings.TrimSpace(v) != v || strings.ContainsAny(v, "\x00\r\n") || strings.HasPrefix(v, "ags_grant_") || strings.HasPrefix(v, "ags_run_") || strings.HasPrefix(v, "mat_") || strings.HasPrefix(v, "ghp_") {
			return ClientRunIssued{}, fmt.Errorf("%w: invalid context identifier", ErrValidation)
		}
	}
	ttl := input.TTLSeconds
	if ttl == 0 {
		ttl = 3600
	}
	if ttl < 60 || ttl > 8*3600 {
		return ClientRunIssued{}, fmt.Errorf("%w: run lifetime must be 60–28800 seconds", ErrValidation)
	}
	bytes := make([]byte, 32)
	if _, e = rand.Read(bytes); e != nil {
		return ClientRunIssued{}, e
	}
	raw := clientRunPrefix + base64.RawURLEncoding.EncodeToString(bytes)
	status := "unlinked"
	if input.Context != (ClientRunContext{}) {
		status = "provisional"
	}
	snapshot := ""
	if input.Source != nil {
		// Source verification enriches an ALREADY authenticated native actor. It
		// cannot authorize a different actor, and its unavailability does not revoke
		// that actor's ordinary collaboration. The source token is never persisted.
		sourceCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		observed, sourceErr := s.IntakeExecutionContext(sourceCtx, ExecutionContextIntakeInput{SourceInstanceID: input.Source.Instance, RuntimeEndpointHint: input.Source.EndpointHint, Locator: input.Source.Locator, SourceToken: input.Source.Token})
		cancel()
		input.Source.Token = ""
		if sourceErr != nil {
			status = "provisional"
		} else {
			var binding db.UserIdentity
			err := s.DBForCtx(ctx).Where("provider = ? AND subject = ? AND user_id = ?", canonicalActorProvider, canonicalActorSubject(observed.SourceRef.SourceInstanceID, observed.SourceRef.AgentID), actor.ID).First(&binding).Error
			if err != nil {
				status = "conflict"
			} else {
				derived := ClientRunContext{Source: observed.SourceRef.SourceInstanceID, Agent: observed.SourceRef.AgentID, Task: observed.SourceRef.TaskID, Run: observed.SourceRef.RunID}
				conflict := (input.Context.Source != "" && input.Context.Source != derived.Source) || (input.Context.Agent != "" && input.Context.Agent != derived.Agent) || (input.Context.Task != "" && input.Context.Task != derived.Task) || (input.Context.Run != "" && input.Context.Run != derived.Run)
				input.Context = derived
				snapshot = observed.SnapshotID
				status = "linked"
				if conflict {
					status = "conflict"
				}
			}
		}
	}
	payload, e := json.Marshal(input.Context)
	if e != nil {
		return ClientRunIssued{}, ErrValidation
	}
	identifier := input.ID
	if identifier == "" {
		identifier = uuid.NewString()
	} else if parsed, err := uuid.Parse(identifier); err != nil || parsed.String() != identifier {
		return ClientRunIssued{}, ErrValidation
	}
	now := time.Now().UTC()
	record := db.ClientRunSession{ID: identifier, UserID: actor.ID, CredentialHash: clientRunHash(raw), ContextJSON: string(payload), AssociationStatus: status, SnapshotID: snapshot, CreatedAt: now, ExpiresAt: now.Add(time.Duration(ttl) * time.Second)}
	e = s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		var user db.User
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, actor.ID).Error; e != nil {
			return e
		}
		if !isUserStatusActive(user.Status) {
			return ErrUnauthorized
		}
		var parent db.Token
		if e := tx.Where("value = ? AND user_id = ?", parentRaw, actor.ID).First(&parent).Error; e != nil {
			return ErrUnauthorized
		}
		if parent.ExpiresAt != nil {
			if !parent.ExpiresAt.After(now) {
				return ErrUnauthorized
			}
			if parent.ExpiresAt.Before(record.ExpiresAt) {
				record.ExpiresAt = *parent.ExpiresAt
			}
		}
		record.ParentTokenID = &parent.ID
		// Only unreferenced expired setup records are collected. PR provenance is
		// retained as business history, not an ever-growing local installation cache.
		if e := tx.Where("user_id = ? AND expires_at < ? AND id NOT IN (?)", actor.ID, now.Add(-24*time.Hour), tx.Model(&db.ClientRunLink{}).Select("run_id")).Delete(&db.ClientRunSession{}).Error; e != nil {
			return e
		}
		var count int64
		if e := tx.Model(&db.ClientRunSession{}).Where("user_id = ? AND expires_at > ? AND revoked_at IS NULL", actor.ID, now).Count(&count).Error; e != nil {
			return e
		}
		if count >= 64 {
			return fmt.Errorf("%w: too many active run sessions", ErrInvalidState)
		}
		return tx.Create(&record).Error
	})
	if e != nil {
		return ClientRunIssued{}, e
	}
	return ClientRunIssued{ClientRunReceipt: clientRunReceipt(record, actor), Token: raw}, nil
}

func (s *Service) ResolveClientRun(ctx context.Context, raw string) (db.User, db.ClientRunSession, error) {
	var session db.ClientRunSession
	var user db.User
	if !IsClientRunCredential(raw) || len(raw) != len(clientRunPrefix)+43 {
		return user, session, ErrUnauthorized
	}
	if e := s.DBForCtx(ctx).Where("credential_hash = ?", clientRunHash(raw)).First(&session).Error; e != nil {
		return user, session, ErrUnauthorized
	}
	now := time.Now().UTC()
	if session.RevokedAt != nil || !session.ExpiresAt.After(now) || session.ParentTokenID == nil {
		return user, session, ErrUnauthorized
	}
	var parent db.Token
	if e := s.DBForCtx(ctx).First(&parent, *session.ParentTokenID).Error; e != nil || parent.UserID != session.UserID || parent.ExpiresAt != nil && !parent.ExpiresAt.After(now) {
		return user, session, ErrUnauthorized
	}
	if e := s.DBForCtx(ctx).First(&user, session.UserID).Error; e != nil || !isUserStatusActive(user.Status) || user.IsAnonymous {
		return db.User{}, session, ErrUnauthorized
	}
	return user, session, nil
}
func (s *Service) ClientRunStatus(ctx context.Context, id string) (ClientRunReceipt, error) {
	actor, e := s.GetCurrentUser(ctx)
	if e != nil {
		return ClientRunReceipt{}, e
	}
	if id == "current" {
		session, ok := ClientRunFromContext(ctx)
		if !ok {
			return ClientRunReceipt{}, ErrNotFound
		}
		return clientRunReceipt(session, actor), nil
	}
	var session db.ClientRunSession
	if e := s.DBForCtx(ctx).Where("id = ? AND user_id = ?", id, actor.ID).First(&session).Error; e != nil {
		return ClientRunReceipt{}, wrapErr(e)
	}
	return clientRunReceipt(session, actor), nil
}
func (s *Service) RevokeClientRun(ctx context.Context, id string) error {
	actor, e := s.GetCurrentUser(ctx)
	if e != nil {
		return e
	}
	if current, ok := ClientRunFromContext(ctx); ok && current.ID != id {
		return ErrForbidden
	}
	if _, e := uuid.Parse(id); e != nil {
		return ErrValidation
	}
	result := s.DBForCtx(ctx).Model(&db.ClientRunSession{}).Where("id = ? AND user_id = ?", id, actor.ID).Update("revoked_at", time.Now().UTC())
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// ObserveClientRunPR is best-effort enrichment AFTER a business operation. It
// cannot turn an already-created PR into a failed request or trigger a second PR.
func (s *Service) ObserveClientRunPR(ctx context.Context, pr db.PullRequest) {
	session, ok := ClientRunFromContext(ctx)
	if !ok {
		return
	}
	row := db.ClientRunLink{RunID: session.ID, RepositoryID: pr.RepositoryID, PullRequestID: pr.ID, CreatedAt: time.Now().UTC()}
	if e := s.DBForCtx(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; e != nil {
		slog.WarnContext(ctx, "client run association pending", "run_id", session.ID, "pull_request_id", pr.ID)
	}
}
func (s *Service) ClientRunLinks(ctx context.Context, repository string, number int) (map[string]any, error) {
	if _, err := s.GetCurrentUser(ctx); err != nil {
		return nil, err
	}
	pr, e := s.GetPR(ctx, repository, number)
	if e != nil {
		return nil, e
	}
	var links []db.ClientRunLink
	if e := s.DBForCtx(ctx).Where("pull_request_id = ? AND repository_id = ?", pr.ID, pr.RepositoryID).Order("created_at desc, id desc").Limit(101).Find(&links).Error; e != nil {
		return nil, e
	}
	complete := len(links) <= 100
	if !complete {
		links = links[:100]
	}
	out := make([]ClientRunReceipt, 0, len(links))
	for _, link := range links {
		var session db.ClientRunSession
		var user db.User
		if e := s.DBForCtx(ctx).First(&session, "id = ?", link.RunID).Error; e != nil {
			return nil, e
		}
		if e := s.DBForCtx(ctx).First(&user, session.UserID).Error; e != nil {
			return nil, e
		}
		out = append(out, clientRunReceipt(session, user))
	}
	var projections []db.PullRequestProjection
	if err := s.DBForCtx(ctx).Where("pull_request_id = ? AND repository_id = ?", pr.ID, pr.RepositoryID).Find(&projections).Error; err != nil {
		return nil, err
	}
	external := make([]map[string]any, 0, len(projections))
	for _, p := range projections {
		external = append(external, map[string]any{"provider": p.Provider, "repository": p.ExternalRepo, "number": p.ExternalNumber, "url": p.ExternalURL, "state": p.State, "last_synced_sha": p.LastSyncedSHA})
	}
	return map[string]any{"schema": "ags.client-run-links.v1", "repository": repository, "pull_request": number, "head_sha": pr.HeadSHA, "runs": out, "projections": external, "complete": complete, "association_is_permission": false}, nil
}
