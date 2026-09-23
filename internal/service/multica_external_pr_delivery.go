package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// enqueueMulticaExternalPRTerminalDeliveryTx persists the typed Multica
// handoff in the same transaction as the AGS terminal PR fact. Network I/O is
// intentionally outside this function; the durable worker owns delivery.
func (s *Service) enqueueMulticaExternalPRTerminalDeliveryTx(ctx context.Context, tx *gorm.DB, pr db.PullRequest, provider, externalRepo string, externalNumber int, terminalState string, merged bool, mergedSHA string, observedAt time.Time) error {
	if s == nil || tx == nil || s.MulticaProjection == nil {
		return nil
	}
	if merged != (strings.EqualFold(strings.TrimSpace(terminalState), ProjectionStateMerged)) {
		return fmt.Errorf("terminal delivery state and merged fact disagree")
	}
	target, ok := s.multicaExternalPRTerminalTarget()
	if !ok {
		return nil
	}
	var link db.PullRequestMulticaLink
	if err := tx.Where("pull_request_id = ? AND confidence = ?", pr.ID, db.MulticaLinkConfidenceAuthoritative).First(&link).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return fmt.Errorf("load authoritative Multica link for terminal delivery: %w", err)
	}
	provider = strings.TrimSpace(strings.ToLower(provider))
	externalRepo = strings.TrimSpace(externalRepo)
	var projection db.PullRequestProjection
	if err := tx.Where("pull_request_id = ? AND provider = ? AND external_repo = ? AND external_number = ?", pr.ID, provider, externalRepo, externalNumber).First(&projection).Error; err != nil {
		return fmt.Errorf("load external PR projection for terminal delivery: %w", err)
	}
	if strings.TrimSpace(pr.Repository.FullName) == "" {
		var repo db.Repository
		if err := tx.First(&repo, pr.RepositoryID).Error; err != nil {
			return fmt.Errorf("load AGS repository for terminal delivery: %w", err)
		}
		pr.Repository = repo
	}
	if strings.TrimSpace(pr.Repository.FullName) == "" {
		return fmt.Errorf("terminal delivery requires AGS repository identity")
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	request := multicaprojection.ExternalPRLinkRequest{
		// The canonical Multica link identity is always the AGS repository/PR.
		// Forgejo remains an observed merge fact below, never a second link.
		Provider:         "ags",
		IssueID:          link.IssueID,
		WorkspaceID:      link.WorkspaceID,
		Workspace:        link.Workspace,
		IssueKey:         link.IssueKey,
		ExternalRepo:     pr.Repository.FullName,
		ExternalNumber:   pr.Number,
		ExternalURL:      s.absAGSURL(fmt.Sprintf("/%s/pull/%d", pr.Repository.FullName, pr.Number)),
		MergeProvider:    provider,
		MergeRepo:        projection.ExternalRepo,
		MergeNumber:      projection.ExternalNumber,
		MergeURL:         projection.ExternalURL,
		MergedSHA:        strings.TrimSpace(mergedSHA),
		CompletionIntent: merged && link.CompletionIntent,
		LinkConfidence:   link.Confidence,
		State:            strings.TrimSpace(strings.ToLower(terminalState)),
	}
	// Projection facts are all-or-none. A partial fallback would be rejected by
	// Multica's closed decoder and is worse than omitting the binding assertion.
	if facts, factsOK := s.delegatedPRMergeProjectionFacts(ctx, pr, projection.ExternalRepo, projection.ExternalNumber); factsOK {
		request.TargetInstance = facts.TargetInstance
		request.CanonicalRepositoryID = facts.CanonicalRepositoryID
		request.CanonicalRepository = facts.CanonicalRepository
		request.ProviderBindingID = facts.ProviderBindingID
		request.ProviderBindingRevision = facts.ProviderBindingRevision
		request.ProviderRepository = facts.ProviderRepository
		request.ExpectedHeadSHA = facts.ExpectedHeadSHA
		request.ExpectedBaseSHA = facts.ExpectedBaseSHA
		request.BaseRef = facts.BaseRef
		request.DelegatedMergeMethod = facts.MergeMethod
		request.ProjectionFactsRevision = facts.ProjectionFactsRevision
	}
	delivery, err := multicaprojection.NewExternalPRTerminalDelivery(multicaprojection.ExternalPRTerminalDeliveryInput{
		TargetInstance: s.MulticaProjection.TargetInstance(),
		Request:        request,
		AGSPrID:        fmt.Sprintf("%s#%d", pr.Repository.FullName, pr.Number),
		ObservedAt:     observedAt,
	})
	if err != nil {
		return err
	}
	payload, err := multicaprojection.MarshalExternalPRTerminalDelivery(delivery)
	if err != nil {
		return err
	}
	intent := db.OutboundDelivery{
		IdempotencyKey: delivery.IdempotencyKey,
		EventType:      OutboundEventMulticaExternalPRTerminal,
		TargetName:     target.Name,
		TargetType:     target.Type,
		SubjectType:    "external_pr",
		SubjectKey:     fmt.Sprintf("%s/%s:%s#%d", link.Workspace, link.IssueKey, pr.Repository.FullName, pr.Number),
		PayloadVersion: multicaprojection.ExternalPRTerminalDeliverySchema,
		PayloadJSON:    db.LargeText(payload),
		Status:         OutboundDeliveryStatusPending,
		MaxAttempts:    defaultOutboundMaxAttempts,
		RepoFullName:   pr.Repository.FullName,
		PRNumber:       pr.Number,
		ExternalRepo:   projection.ExternalRepo,
		ExternalNumber: projection.ExternalNumber,
		MergeSHA:       strings.TrimSpace(mergedSHA),
	}
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&intent)
	if result.Error != nil {
		return fmt.Errorf("persist Multica terminal delivery: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return nil
	}
	var existing db.OutboundDelivery
	// The conflict may have waited for a concurrent commit invisible to this
	// transaction's snapshot. A current read observes exactly that winning row.
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("idempotency_key = ?", delivery.IdempotencyKey).First(&existing).Error; err != nil {
		return fmt.Errorf("read existing Multica terminal delivery: %w", err)
	}
	if existing.EventType != intent.EventType || existing.TargetName != intent.TargetName || existing.TargetType != intent.TargetType || existing.PayloadVersion != intent.PayloadVersion || string(existing.PayloadJSON) != payload {
		return fmt.Errorf("Multica terminal delivery idempotency key conflicts with existing payload, target, or event")
	}
	return nil
}

func (s *Service) enqueueClosedMulticaExternalPRTerminalDeliveryTx(ctx context.Context, tx *gorm.DB, pr db.PullRequest, observedAt time.Time) error {
	if pr.ClosedAt != nil && !pr.ClosedAt.IsZero() {
		observedAt = pr.ClosedAt.UTC()
	}
	var projection db.PullRequestProjection
	if err := tx.Where("pull_request_id = ? AND provider = ?", pr.ID, ProjectionProviderForgejo).First(&projection).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return fmt.Errorf("load Forgejo projection for AGS close terminal delivery: %w", err)
	}
	return s.enqueueMulticaExternalPRTerminalDeliveryTx(ctx, tx, pr, ProjectionProviderForgejo, projection.ExternalRepo, projection.ExternalNumber, ProjectionStateClosed, false, "", observedAt)
}

func (s *Service) enqueueMulticaExternalPRTerminalDelivery(ctx context.Context, pr db.PullRequest, provider, externalRepo string, externalNumber int, terminalState string, merged bool, mergedSHA string, observedAt time.Time) error {
	if s == nil || s.DBForCtx(ctx) == nil {
		return nil
	}
	return s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		return s.enqueueMulticaExternalPRTerminalDeliveryTx(ctx, tx, pr, provider, externalRepo, externalNumber, terminalState, merged, mergedSHA, observedAt)
	})
}

func (s *Service) multicaExternalPRTerminalTarget() (OutboundTarget, bool) {
	if s == nil {
		return OutboundTarget{}, false
	}
	for _, target := range s.OutboundEventTargets[OutboundEventMulticaExternalPRTerminal] {
		target.Name = strings.TrimSpace(target.Name)
		target.Type = strings.TrimSpace(strings.ToLower(target.Type))
		if target.Name != "" && target.Type == OutboundTargetTypeMulticaExternalPR {
			return target, true
		}
	}
	return OutboundTarget{}, false
}
