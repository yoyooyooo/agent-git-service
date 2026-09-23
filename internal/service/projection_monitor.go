package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

const (
	ProjectionStatusActive                   = "active"
	ProjectionStatusResolved                 = "resolved"
	ProjectionAuthorityAGS                   = "ags"
	ProjectionFailureProviderAdmissionDenied = "provider_admission_denied"
	projectionZeroSHA                        = "0000000000000000000000000000000000000000"
)

type ProjectionStatus struct {
	Repo   string                 `json:"repo"`
	Refs   []ProjectionRefStatus  `json:"refs"`
	Jobs   []ProjectionJobStatus  `json:"jobs"`
	Events []ProjectionEventEntry `json:"events"`
}

type ProjectionRefStatus struct {
	Provider     string     `json:"provider"`
	Generation   uint       `json:"generation"`
	TargetRepo   string     `json:"target_repo"`
	TargetID     int64      `json:"target_id,omitempty"`
	HTMLURL      string     `json:"html_url,omitempty"`
	CloneURL     string     `json:"clone_url,omitempty"`
	Ref          string     `json:"ref"`
	Branch       string     `json:"branch"`
	Type         string     `json:"type"`
	Status       string     `json:"status"`
	Authority    string     `json:"authority"`
	AGSSHA       string     `json:"ags_sha"`
	ForgejoSHA   string     `json:"forgejo_sha,omitempty"`
	ExternalSHA  string     `json:"external_sha"`
	ErrorSummary string     `json:"error_summary,omitempty"`
	FirstSeenAt  time.Time  `json:"first_seen_at"`
	LastSeenAt   time.Time  `json:"last_seen_at"`
	ResolvedAt   *time.Time `json:"resolved_at,omitempty"`
}

type ProjectionEventEntry struct {
	Provider     string     `json:"provider"`
	TargetRepo   string     `json:"target_repo"`
	Ref          string     `json:"ref"`
	Branch       string     `json:"branch"`
	Type         string     `json:"type"`
	Status       string     `json:"status"`
	Authority    string     `json:"authority"`
	AGSSHA       string     `json:"ags_sha"`
	ForgejoSHA   string     `json:"forgejo_sha,omitempty"`
	ExternalSHA  string     `json:"external_sha"`
	ErrorSummary string     `json:"error_summary,omitempty"`
	OccurredAt   time.Time  `json:"occurred_at"`
	ResolvedAt   *time.Time `json:"resolved_at,omitempty"`
}

type ProjectionJobStatus struct {
	JobID                 uint                         `json:"job_id"`
	Provider              string                       `json:"provider"`
	Trigger               string                       `json:"trigger"`
	ActionGeneration      uint                         `json:"action_generation,omitempty"`
	CorrelationID         string                       `json:"correlation_id,omitempty"`
	PullRequestID         uint                         `json:"pull_request_id"`
	AGSPRNumber           int                          `json:"ags_pr_number"`
	HeadRef               string                       `json:"head_ref"`
	BaseRef               string                       `json:"base_ref"`
	Phase                 string                       `json:"phase"`
	Status                string                       `json:"status"`
	Attempt               int                          `json:"attempt"`
	LastErrorType         string                       `json:"last_error_type,omitempty"`
	LastError             string                       `json:"last_error,omitempty"`
	ExternalRepo          string                       `json:"external_repo,omitempty"`
	ExternalNumber        int                          `json:"external_number,omitempty"`
	ExternalURL           string                       `json:"external_url,omitempty"`
	RemoteRef             string                       `json:"remote_ref,omitempty"`
	RemoteSHA             string                       `json:"remote_sha,omitempty"`
	PreflightAGSHeadSHA   string                       `json:"preflight_ags_head_sha,omitempty"`
	PreflightBaseSHA      string                       `json:"preflight_base_sha,omitempty"`
	ExpectedForgejoOldSHA string                       `json:"expected_forgejo_old_sha,omitempty"`
	DesiredAGSHeadSHA     string                       `json:"desired_ags_head_sha,omitempty"`
	ObservedForgejoSHA    string                       `json:"observed_forgejo_sha,omitempty"`
	LastSyncedAGSHeadSHA  string                       `json:"last_synced_ags_head_sha,omitempty"`
	NextRunAt             *time.Time                   `json:"next_run_at,omitempty"`
	StartedAt             *time.Time                   `json:"started_at,omitempty"`
	FinishedAt            *time.Time                   `json:"finished_at,omitempty"`
	NextRepairAction      string                       `json:"next_repair_action"`
	UpdatedAt             time.Time                    `json:"updated_at"`
	Attempts              []ProjectionJobAttemptStatus `json:"attempts,omitempty"`
}

type ProjectionJobAttemptStatus struct {
	Attempt    int        `json:"attempt"`
	Phase      string     `json:"phase"`
	Status     string     `json:"status"`
	ErrorType  string     `json:"error_type,omitempty"`
	Error      string     `json:"error,omitempty"`
	RemoteSHA  string     `json:"remote_sha,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

func (s *Service) RecordForgejoProjectionFailure(ctx context.Context, repoFullName string, changes []ForgejoRefChange, err error) error {
	if s == nil || err == nil {
		return nil
	}
	failure := projectionFailureFromError(repoFullName, changes, err)
	failure.ErrorSummary = forgejointegration.SanitizeProjectionErrorSummary(failure.ErrorSummary)
	return s.recordProjectionFailure(ctx, failure)
}

// RecordForgejoProjectionFailureWithAlert atomically records a required Forgejo
// drift generation and its target-specific outbound intents, then attempts delivery.
func (s *Service) RecordForgejoProjectionFailureWithAlert(ctx context.Context, repoFullName string, changes []ForgejoRefChange, err error, metadata ProjectionFailureAlertMetadata) error {
	if s == nil || err == nil {
		return nil
	}
	failure := projectionFailureFromError(repoFullName, changes, err)
	failure.ErrorSummary = forgejointegration.SanitizeProjectionErrorSummary(failure.ErrorSummary)
	metadata.AGSPRURL = sanitizeProjectionAlertURL(metadata.AGSPRURL)
	metadata.ForgejoPRURL = sanitizeProjectionAlertURL(metadata.ForgejoPRURL)
	var deliveries []db.OutboundDelivery
	var alertErr error
	database := s.DBForCtx(ctx)
	if txErr := database.Transaction(func(tx *gorm.DB) error {
		txCtx := ContextWithDB(ctx, tx)
		if err := s.recordProjectionFailure(txCtx, failure); err != nil {
			return err
		}
		var state db.ProjectionRefState
		if err := tx.Where("provider = ? AND repository_id = ? AND ref = ?", failure.Provider, s.projectionRepositoryID(txCtx, failure.RepoFullName), failure.Ref).First(&state).Error; err != nil {
			return fmt.Errorf("load projection drift for outbound: %w", err)
		}
		notes := projectionDriftNotificationsFromRows([]db.ProjectionRefState{state})
		if len(notes) == 0 {
			return nil
		}
		note := notes[0]
		note.ProjectionFailureAlertMetadata = metadata
		if strings.TrimSpace(note.CorrelationID) == "" {
			note.CorrelationID = fmt.Sprintf("projection-drift:%s:%s:g%d", failure.RepoFullName, failure.Ref, state.Generation)
		}
		deliveries, alertErr = s.enqueueProjectionDriftOutboundIntents(txCtx, note, false)
		if alertErr != nil {
			var outcome *ProjectionAlertingError
			if errors.As(alertErr, &outcome) && outcome.Outcome == ProjectionAlertingOutcomeTargetMissing {
				return nil
			}
			return alertErr
		}
		return tx.Model(&db.ProjectionRefState{}).Where("id = ?", state.ID).Update("last_notified_at", failure.OccurredAt).Error
	}); txErr != nil {
		return txErr
	}
	if alertErr != nil {
		return alertErr
	}
	return s.deliverProjectionDriftOutboundNow(ctx, deliveries)
}

type projectionFailureRecord struct {
	Provider     string
	RepoFullName string
	TargetRepo   string
	Ref          string
	Branch       string
	Type         string
	Authority    string
	AGSSHA       string
	ExternalSHA  string
	ErrorSummary string
	OccurredAt   time.Time
}

func projectionFailureFromError(repoFullName string, changes []ForgejoRefChange, err error) projectionFailureRecord {
	if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
		ref, sha := firstProjectionChange(changes)
		return projectionFailureRecord{
			Provider: ProjectionProviderForgejo, RepoFullName: repoFullName,
			Ref: ref, Branch: branchNameFromRef(ref), Type: ProjectionFailureProviderAdmissionDenied,
			Authority: ProjectionAuthorityAGS, AGSSHA: sha,
			ErrorSummary: DelegatedSessionDenialReason(err), OccurredAt: time.Now().UTC(),
		}
	}
	var pe *forgejointegration.ProjectionError
	if errors.As(err, &pe) && pe != nil {
		occurred := time.Now().UTC()
		return projectionFailureRecord{
			Provider:     ProjectionProviderForgejo,
			RepoFullName: firstNonEmpty(pe.Repo, repoFullName),
			TargetRepo:   pe.TargetRepo,
			Ref:          pe.Ref,
			Branch:       firstNonEmpty(pe.Branch, branchNameFromRef(pe.Ref)),
			Type:         firstNonEmpty(pe.Type, forgejointegration.ProjectionFailureUnknown),
			Authority:    ProjectionAuthorityAGS,
			AGSSHA:       pe.ExpectedSHA,
			ExternalSHA:  pe.ActualSHA,
			ErrorSummary: pe.ErrorSummary,
			OccurredAt:   occurred,
		}
	}
	ref, sha := firstProjectionChange(changes)
	classified := forgejointegration.ClassifyProjectionError(ref, sha, err)
	return projectionFailureRecord{
		Provider:     ProjectionProviderForgejo,
		RepoFullName: repoFullName,
		Ref:          ref,
		Branch:       branchNameFromRef(ref),
		Type:         firstNonEmpty(classified.Type, forgejointegration.ProjectionFailureUnknown),
		Authority:    ProjectionAuthorityAGS,
		AGSSHA:       sha,
		ErrorSummary: classified.ErrorSummary,
		OccurredAt:   time.Now().UTC(),
	}
}

func (s *Service) recordProjectionFailure(ctx context.Context, failure projectionFailureRecord) error {
	failure.Provider = firstNonEmpty(failure.Provider, ProjectionProviderForgejo)
	failure.Authority = firstNonEmpty(failure.Authority, ProjectionAuthorityAGS)
	failure.Type = firstNonEmpty(failure.Type, forgejointegration.ProjectionFailureUnknown)
	if failure.OccurredAt.IsZero() {
		failure.OccurredAt = time.Now().UTC()
	}
	repoID := s.projectionRepositoryID(ctx, failure.RepoFullName)
	failure = s.preserveSpecificActiveProjectionFailure(ctx, repoID, failure)
	generation := uint(1)
	firstSeenAt := failure.OccurredAt
	newGeneration := false
	var existingState db.ProjectionRefState
	existingErr := s.DBForCtx(ctx).
		Where("provider = ? AND repository_id = ? AND ref = ?", failure.Provider, repoID, failure.Ref).
		First(&existingState).Error
	if existingErr == nil {
		generation = existingState.Generation
		if generation == 0 {
			generation = 1
		}
		if existingState.Status == ProjectionStatusActive {
			firstSeenAt = existingState.FirstSeenAt
		} else {
			generation++
			newGeneration = true
		}
	} else if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
		return fmt.Errorf("load existing projection state: %w", existingErr)
	}
	event := db.ProjectionEvent{
		Provider:     failure.Provider,
		Type:         failure.Type,
		Status:       ProjectionStatusActive,
		Authority:    failure.Authority,
		RepositoryID: repoID,
		RepoFullName: failure.RepoFullName,
		TargetRepo:   failure.TargetRepo,
		Ref:          failure.Ref,
		Branch:       failure.Branch,
		AGSSHA:       failure.AGSSHA,
		ExternalSHA:  failure.ExternalSHA,
		ErrorSummary: db.LargeText(failure.ErrorSummary),
		OccurredAt:   failure.OccurredAt,
	}
	if err := s.DBForCtx(ctx).Create(&event).Error; err != nil {
		return fmt.Errorf("record projection event: %w", err)
	}
	state := db.ProjectionRefState{
		Provider:     failure.Provider,
		RepositoryID: repoID,
		RepoFullName: failure.RepoFullName,
		TargetRepo:   failure.TargetRepo,
		Ref:          failure.Ref,
		Branch:       failure.Branch,
		Type:         failure.Type,
		Status:       ProjectionStatusActive,
		Generation:   generation,
		Authority:    failure.Authority,
		AGSSHA:       failure.AGSSHA,
		ExternalSHA:  failure.ExternalSHA,
		ErrorSummary: db.LargeText(failure.ErrorSummary),
		FirstSeenAt:  firstSeenAt,
		LastSeenAt:   failure.OccurredAt,
	}
	updates := map[string]any{
		"repo_full_name": failure.RepoFullName,
		"target_repo":    failure.TargetRepo,
		"branch":         failure.Branch,
		"type":           failure.Type,
		"status":         ProjectionStatusActive,
		"generation":     generation,
		"authority":      failure.Authority,
		"ags_sha":        failure.AGSSHA,
		"external_sha":   failure.ExternalSHA,
		"error_summary":  db.LargeText(failure.ErrorSummary),
		"first_seen_at":  firstSeenAt,
		"last_seen_at":   failure.OccurredAt,
		"resolved_at":    nil,
	}
	if newGeneration {
		updates["last_notified_at"] = nil
	}
	return s.DBForCtx(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "provider"}, {Name: "repository_id"}, {Name: "ref"}},
		DoUpdates: clause.Assignments(updates),
	}).Create(&state).Error
}

func (s *Service) preserveSpecificActiveProjectionFailure(ctx context.Context, repoID uint, failure projectionFailureRecord) projectionFailureRecord {
	if s == nil || repoID == 0 || failure.Type != forgejointegration.ProjectionFailureSHADrift || strings.TrimSpace(failure.Ref) == "" {
		return failure
	}
	var existing db.ProjectionRefState
	err := s.DBForCtx(ctx).
		Where("provider = ? AND repository_id = ? AND ref = ? AND status = ?", failure.Provider, repoID, strings.TrimSpace(failure.Ref), ProjectionStatusActive).
		First(&existing).Error
	if err != nil || strings.TrimSpace(existing.Type) == "" || existing.Type == forgejointegration.ProjectionFailureSHADrift {
		return failure
	}
	failure.Type = existing.Type
	if strings.TrimSpace(string(existing.ErrorSummary)) != "" {
		failure.ErrorSummary = string(existing.ErrorSummary)
	}
	return failure
}

func (s *Service) ResolveForgejoProjectionSuccess(ctx context.Context, repoFullName string, changes []ForgejoRefChange) {
	if s == nil {
		return
	}
	now := time.Now().UTC()
	for _, change := range changes {
		ref := strings.TrimSpace(change.Ref)
		if ref == "" {
			continue
		}
		if !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		if change.Deleted || change.After == projectionZeroSHA {
			_ = s.ResolveProjectionRefState(ctx, repoFullName, ProjectionProviderForgejo, ref, "", "", now)
			continue
		}
		_ = s.ResolveProjectionRefState(ctx, repoFullName, ProjectionProviderForgejo, ref, change.After, change.After, now)
		// Branch push already mirrored the head ref. When an open AGS PR maps to
		// that branch, advance last_synced_sha to the confirmed after SHA so
		// action/rebase and CI evidence no longer depend on manual revalidate.
		s.advanceOpenPRProjectionAfterBranchPush(ctx, repoFullName, ProjectionProviderForgejo, strings.TrimPrefix(ref, "refs/heads/"), change.After)
	}
}

func (s *Service) advanceOpenPRProjectionAfterBranchPush(ctx context.Context, repoFullName, provider, headRef, afterSHA string) {
	if s == nil || strings.TrimSpace(headRef) == "" || strings.TrimSpace(afterSHA) == "" || afterSHA == projectionZeroSHA {
		return
	}
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" {
		provider = ProjectionProviderForgejo
	}
	repo, err := s.GetRepo(ctx, repoFullName)
	if err != nil {
		return
	}
	prs, err := s.ListOpenPRsByHead(ctx, repo.ID, headRef)
	if err != nil || len(prs) == 0 {
		return
	}
	for _, pr := range prs {
		var projection db.PullRequestProjection
		err := s.DBForCtx(ctx).
			Where("pull_request_id = ? AND provider = ? AND state = ?", pr.ID, provider, ProjectionStateOpen).
			First(&projection).Error
		if err != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(projection.LastSyncedSHA), strings.TrimSpace(afterSHA)) {
			continue
		}
		if err := s.DBForCtx(ctx).Model(&db.PullRequestProjection{}).
			Where("id = ? AND provider = ? AND state = ?", projection.ID, provider, ProjectionStateOpen).
			Update("last_synced_sha", afterSHA).Error; err != nil {
			slog.WarnContext(ctx, "advance PR projection last_synced_sha after branch push failed",
				"repo", repoFullName, "provider", provider, "pr", pr.Number, "head_sha", afterSHA, "error", err)
		}
	}
}

func (s *Service) ResolveProjectionRefState(ctx context.Context, repoFullName, provider, ref, agsSHA, externalSHA string, resolvedAt time.Time) error {
	if s == nil {
		return nil
	}
	if resolvedAt.IsZero() {
		resolvedAt = time.Now().UTC()
	}
	provider = firstNonEmpty(provider, ProjectionProviderForgejo)
	repoID := s.projectionRepositoryID(ctx, repoFullName)
	var deliveries []db.OutboundDelivery
	var alertErr error
	database := s.DBForCtx(ctx)
	if err := database.Transaction(func(tx *gorm.DB) error {
		var state db.ProjectionRefState
		stateQuery := tx.Where("provider = ? AND ref = ? AND status = ?", provider, strings.TrimSpace(ref), ProjectionStatusActive)
		if repoID != 0 {
			stateQuery = stateQuery.Where("repository_id = ?", repoID)
		} else {
			stateQuery = stateQuery.Where("repo_full_name = ?", strings.TrimSpace(repoFullName))
		}
		if err := stateQuery.First(&state).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		activeAlertWasEnqueued := state.LastNotifiedAt != nil
		if err := tx.Model(&db.ProjectionRefState{}).Where("id = ?", state.ID).Updates(map[string]any{
			"status":       ProjectionStatusResolved,
			"type":         "resolved",
			"ags_sha":      strings.TrimSpace(agsSHA),
			"external_sha": strings.TrimSpace(externalSHA),
			"last_seen_at": resolvedAt,
			"resolved_at":  &resolvedAt,
		}).Error; err != nil {
			return err
		}
		if !activeAlertWasEnqueued || len(s.OutboundEventTargets[OutboundEventProjectionDrift]) == 0 {
			return nil
		}
		state.Status = ProjectionStatusResolved
		state.Type = "resolved"
		state.AGSSHA = strings.TrimSpace(agsSHA)
		state.ExternalSHA = strings.TrimSpace(externalSHA)
		state.LastSeenAt = resolvedAt
		state.ResolvedAt = &resolvedAt
		notes := projectionDriftNotificationsFromRows([]db.ProjectionRefState{state})
		deliveries, alertErr = s.enqueueProjectionDriftOutboundIntents(ContextWithDB(ctx, tx), notes[0], true)
		if alertErr != nil {
			var outcome *ProjectionAlertingError
			if errors.As(alertErr, &outcome) && outcome.Outcome == ProjectionAlertingOutcomeTargetMissing {
				return nil
			}
			return alertErr
		}
		return tx.Model(&db.ProjectionRefState{}).Where("id = ?", state.ID).Update("last_notified_at", resolvedAt).Error
	}); err != nil {
		return err
	}
	if alertErr != nil {
		return alertErr
	}
	if len(deliveries) == 0 {
		return nil
	}
	return s.deliverProjectionDriftOutboundNow(ctx, deliveries)
}

func (s *Service) GetProjectionStatus(ctx context.Context, repoFullName string) (ProjectionStatus, error) {
	if s == nil {
		return ProjectionStatus{}, fmt.Errorf("service is nil")
	}
	repo, err := s.projectionRepository(ctx, repoFullName)
	if err != nil {
		return ProjectionStatus{}, err
	}
	if err := s.requireRepoPermission(ctx, repo.ID, RepoPermissionRead); err != nil {
		return ProjectionStatus{}, err
	}
	var states []db.ProjectionRefState
	if err := s.DBForCtx(ctx).Where("repository_id = ?", repo.ID).Order("provider, ref").Find(&states).Error; err != nil {
		return ProjectionStatus{}, err
	}
	var jobs []db.PullRequestProjectionJob
	if err := s.DBForCtx(ctx).Preload("Attempts").Where("repository_id = ?", repo.ID).Order("updated_at desc, id desc").Find(&jobs).Error; err != nil {
		return ProjectionStatus{}, err
	}
	var events []db.ProjectionEvent
	if err := s.DBForCtx(ctx).Where("repository_id = ?", repo.ID).Order("occurred_at desc, id desc").Limit(50).Find(&events).Error; err != nil {
		return ProjectionStatus{}, err
	}
	var forgejoRepo *forgejointegration.RepositorySnapshot
	var agsDefaultBranchSHA string
	if s.ForgejoIntegration != nil {
		for _, state := range states {
			if state.Provider != ProjectionProviderForgejo {
				continue
			}
			snapshot, found, readErr := s.ForgejoIntegration.RepositorySnapshot(ctx, repo.FullName)
			if readErr != nil {
				slog.WarnContext(ctx, "read Forgejo repository projection identity failed", "repo", repo.FullName, "error", readErr)
			} else if found {
				forgejoRepo = &snapshot
			}
			break
		}
	}
	if forgejoRepo != nil && s.Git != nil && forgejoRepo.DefaultBranch == repo.DefaultBranch {
		liveSHA, readErr := s.Git.HeadSHA(ctx, repo.FullName, repo.DefaultBranch)
		if readErr != nil {
			slog.WarnContext(ctx, "read AGS default branch projection identity failed", "repo", repo.FullName, "error", readErr)
		} else {
			agsDefaultBranchSHA = strings.ToLower(strings.TrimSpace(liveSHA))
		}
	}
	out := ProjectionStatus{Repo: repo.FullName, Refs: make([]ProjectionRefStatus, 0, len(states)+1), Jobs: make([]ProjectionJobStatus, 0, len(jobs)), Events: make([]ProjectionEventEntry, 0, len(events))}
	defaultRef := "refs/heads/" + repo.DefaultBranch
	defaultRefFound := false
	for _, state := range states {
		refStatus := projectionRefStatus(state)
		if forgejoRepo != nil && state.Provider == ProjectionProviderForgejo && (state.TargetRepo == "" || state.TargetRepo == forgejoRepo.FullName) {
			refStatus.TargetRepo = forgejoRepo.FullName
			refStatus.TargetID = forgejoRepo.ID
			refStatus.HTMLURL = forgejoRepo.HTMLURL
			refStatus.CloneURL = forgejoRepo.CloneURL
			if refStatus.Ref == defaultRef {
				defaultRefFound = true
				refStatus = reconcileDefaultBranchProjectionStatus(refStatus, repo.DefaultBranch, agsDefaultBranchSHA, *forgejoRepo)
			}
		}
		out.Refs = append(out.Refs, refStatus)
	}
	if !defaultRefFound && forgejoRepo != nil && isFullHexRevision(agsDefaultBranchSHA) {
		now := time.Now().UTC()
		status := ProjectionRefStatus{
			Provider: ProjectionProviderForgejo, Generation: 1,
			TargetRepo: forgejoRepo.FullName, TargetID: forgejoRepo.ID,
			HTMLURL: forgejoRepo.HTMLURL, CloneURL: forgejoRepo.CloneURL,
			Ref: defaultRef, Branch: repo.DefaultBranch, Authority: ProjectionAuthorityAGS,
			FirstSeenAt: now, LastSeenAt: now,
		}
		out.Refs = append(out.Refs, reconcileDefaultBranchProjectionStatus(status, repo.DefaultBranch, agsDefaultBranchSHA, *forgejoRepo))
	}
	for _, job := range jobs {
		out.Jobs = append(out.Jobs, projectionJobStatus(job))
	}
	for _, event := range events {
		out.Events = append(out.Events, projectionEventEntry(event))
	}
	return out, nil
}

func projectionRefStatus(state db.ProjectionRefState) ProjectionRefStatus {
	return ProjectionRefStatus{
		Provider:     state.Provider,
		Generation:   state.Generation,
		TargetRepo:   state.TargetRepo,
		Ref:          state.Ref,
		Branch:       state.Branch,
		Type:         state.Type,
		Status:       state.Status,
		Authority:    state.Authority,
		AGSSHA:       state.AGSSHA,
		ForgejoSHA:   state.ExternalSHA,
		ExternalSHA:  state.ExternalSHA,
		ErrorSummary: string(state.ErrorSummary),
		FirstSeenAt:  state.FirstSeenAt,
		LastSeenAt:   state.LastSeenAt,
		ResolvedAt:   state.ResolvedAt,
	}
}

func reconcileDefaultBranchProjectionStatus(
	status ProjectionRefStatus,
	defaultBranch string,
	agsSHA string,
	snapshot forgejointegration.RepositorySnapshot,
) ProjectionRefStatus {
	if status.Provider != ProjectionProviderForgejo || status.Ref != "refs/heads/"+strings.TrimSpace(defaultBranch) {
		return status
	}
	agsSHA = strings.ToLower(strings.TrimSpace(agsSHA))
	forgejoSHA := strings.ToLower(strings.TrimSpace(snapshot.DefaultBranchSHA))
	if snapshot.DefaultBranch != strings.TrimSpace(defaultBranch) || !isFullHexRevision(agsSHA) || !isFullHexRevision(forgejoSHA) {
		return status
	}
	status.AGSSHA = agsSHA
	status.ForgejoSHA = forgejoSHA
	status.ExternalSHA = forgejoSHA
	if agsSHA == forgejoSHA {
		status.Type = "resolved"
		status.Status = ProjectionStatusResolved
		status.ErrorSummary = ""
		return status
	}
	status.Type = forgejointegration.ProjectionFailureSHADrift
	status.Status = ProjectionStatusActive
	status.ErrorSummary = "live AGS and Forgejo default branch SHA differ"
	return status
}

func projectionEventEntry(event db.ProjectionEvent) ProjectionEventEntry {
	return ProjectionEventEntry{
		Provider:     event.Provider,
		TargetRepo:   event.TargetRepo,
		Ref:          event.Ref,
		Branch:       event.Branch,
		Type:         event.Type,
		Status:       event.Status,
		Authority:    event.Authority,
		AGSSHA:       event.AGSSHA,
		ForgejoSHA:   event.ExternalSHA,
		ExternalSHA:  event.ExternalSHA,
		ErrorSummary: string(event.ErrorSummary),
		OccurredAt:   event.OccurredAt,
		ResolvedAt:   event.ResolvedAt,
	}
}

func projectionJobStatus(job db.PullRequestProjectionJob) ProjectionJobStatus {
	return ProjectionJobStatus{
		JobID:                 job.ID,
		Provider:              job.Provider,
		Trigger:               job.Trigger,
		ActionGeneration:      job.ActionGeneration,
		CorrelationID:         job.CorrelationID,
		PullRequestID:         job.PullRequestID,
		AGSPRNumber:           job.AGSPRNumber,
		HeadRef:               job.HeadRef,
		BaseRef:               job.BaseRef,
		Phase:                 job.Phase,
		Status:                projectionJobStatusCategory(job.Phase),
		Attempt:               job.Attempt,
		LastErrorType:         job.LastErrorType,
		LastError:             job.LastError,
		ExternalRepo:          job.ExternalRepo,
		ExternalNumber:        job.ExternalNumber,
		ExternalURL:           job.ExternalURL,
		RemoteRef:             job.RemoteRef,
		RemoteSHA:             job.RemoteSHA,
		PreflightAGSHeadSHA:   job.PreflightAGSHeadSHA,
		PreflightBaseSHA:      job.PreflightBaseSHA,
		ExpectedForgejoOldSHA: job.ExpectedForgejoOldHeadSHA,
		DesiredAGSHeadSHA:     job.DesiredAGSHeadSHA,
		ObservedForgejoSHA:    job.ObservedForgejoHeadSHA,
		LastSyncedAGSHeadSHA:  job.HeadSHA,
		NextRunAt:             job.NextRunAt,
		StartedAt:             job.StartedAt,
		FinishedAt:            job.FinishedAt,
		NextRepairAction:      projectionJobNextRepairAction(job),
		UpdatedAt:             job.UpdatedAt,
		Attempts:              projectionJobAttemptStatuses(job.Attempts),
	}
}

func projectionJobAttemptStatuses(rows []db.PullRequestProjectionJobAttempt) []ProjectionJobAttemptStatus {
	if len(rows) == 0 {
		return nil
	}
	sorted := append([]db.PullRequestProjectionJobAttempt(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Attempt == sorted[j].Attempt {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].Attempt < sorted[j].Attempt
	})
	out := make([]ProjectionJobAttemptStatus, 0, len(sorted))
	for _, row := range sorted {
		out = append(out, ProjectionJobAttemptStatus{
			Attempt: row.Attempt, Phase: row.Phase, Status: row.Status,
			ErrorType: row.ErrorType, Error: row.ErrorSummary, RemoteSHA: row.RemoteSHA,
			StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return out
}

func projectionJobStatusCategory(phase string) string {
	switch phase {
	case ForgejoProjectionPhaseQueued:
		return "queued"
	case ForgejoProjectionPhasePreflight,
		ForgejoProjectionPhaseRebasing,
		ForgejoProjectionPhaseProjectionResume,
		ForgejoProjectionPhasePushingRef,
		ForgejoProjectionPhaseVerifyingRef,
		ForgejoProjectionPhaseVerifyingPR,
		ForgejoProjectionPhaseEnsuringPR,
		ForgejoProjectionPhaseRecordingProjection:
		return "running"
	case ForgejoProjectionPhaseProjected:
		return "projected"
	case ForgejoProjectionPhaseNeedsRebase:
		return "needs_rebase"
	case ForgejoProjectionPhaseFailedRetryable:
		return "retryable_failed"
	case ForgejoProjectionPhaseFailedTerminal:
		return "terminal_failed"
	default:
		return strings.TrimSpace(phase)
	}
}

func projectionJobNextRepairAction(job db.PullRequestProjectionJob) string {
	switch job.Phase {
	case ForgejoProjectionPhaseQueued,
		ForgejoProjectionPhasePreflight,
		ForgejoProjectionPhaseRebasing,
		ForgejoProjectionPhaseProjectionResume,
		ForgejoProjectionPhasePushingRef,
		ForgejoProjectionPhaseVerifyingRef,
		ForgejoProjectionPhaseVerifyingPR,
		ForgejoProjectionPhaseEnsuringPR,
		ForgejoProjectionPhaseRecordingProjection:
		return "wait_for_worker_or_call_resume_if_worker_was_disabled"
	case ForgejoProjectionPhaseProjected:
		return "none"
	case ForgejoProjectionPhaseNeedsRebase:
		return "trigger_fresh_rebase_after_base_projection_converges"
	case ForgejoProjectionPhaseFailedRetryable:
		if job.NextRunAt != nil && job.NextRunAt.After(time.Now().UTC()) {
			return "wait_for_next_retry_or_call_projection_retry"
		}
		return "call_projection_retry"
	case ForgejoProjectionPhaseFailedTerminal:
		return "inspect_terminal_error_and_drift_before_manual_ags_authority_repair"
	default:
		return "inspect_projection_status"
	}
}

func (s *Service) projectionRepositoryID(ctx context.Context, repoFullName string) uint {
	repo, err := s.projectionRepository(ctx, repoFullName)
	if err != nil {
		return 0
	}
	return repo.ID
}

func (s *Service) projectionRepository(ctx context.Context, repoFullName string) (db.Repository, error) {
	var repo db.Repository
	if s == nil {
		return repo, fmt.Errorf("service is nil")
	}
	if err := s.DBForCtx(ctx).Where("full_name = ?", strings.TrimSpace(repoFullName)).First(&repo).Error; err != nil {
		return repo, err
	}
	return repo, nil
}

func firstProjectionChange(changes []ForgejoRefChange) (string, string) {
	for _, change := range changes {
		if strings.TrimSpace(change.Ref) == "" {
			continue
		}
		return strings.TrimSpace(change.Ref), strings.TrimSpace(change.After)
	}
	return "", ""
}

func branchNameFromRef(ref string) string {
	ref = strings.TrimSpace(ref)
	return strings.TrimPrefix(ref, "refs/heads/")
}
