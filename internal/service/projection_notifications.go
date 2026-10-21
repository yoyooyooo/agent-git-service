package service

import (
	"context"
	"fmt"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
)

// ProjectionFailureAlertMetadata carries action identity that is safe to persist
// in a projection-drift outbound payload.
type ProjectionFailureAlertMetadata struct {
	AGSPRNumber        int
	AGSPRURL           string
	ForgejoPRNumber    int
	ForgejoPRURL       string
	Actor              string
	ActionPhase        string
	AGSOldSHA          string
	AGSNewSHA          string
	ExpectedForgejoSHA string
	ActualForgejoSHA   string
	CorrelationID      string
	RecoveryHint       string
}

// ProjectionDriftNotification is a human-facing alert candidate for one active projection drift.
type ProjectionDriftNotification struct {
	StateID      uint
	Generation   uint
	Provider     string
	RepoFullName string
	TargetRepo   string
	Ref          string
	Branch       string
	Type         string
	Authority    string
	AGSSHA       string
	ForgejoSHA   string
	ErrorSummary string
	FirstSeenAt  time.Time
	LastSeenAt   time.Time
	ResolvedAt   time.Time
	ProjectionFailureAlertMetadata
}

func (s *Service) ListProjectionDriftNotifications(ctx context.Context, throttle, gracePeriod time.Duration, limit int) ([]ProjectionDriftNotification, error) {
	if s == nil {
		return nil, fmt.Errorf("service is nil")
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	now := time.Now().UTC()
	query := s.DBForCtx(ctx).Where("status = ?", ProjectionStatusActive)
	if gracePeriod > 0 {
		query = query.Where("first_seen_at <= ?", now.Add(-gracePeriod))
	}
	if throttle > 0 {
		cutoff := now.Add(-throttle)
		query = query.Where("last_notified_at IS NULL OR last_notified_at < ?", cutoff)
	}
	var rows []db.ProjectionRefState
	if err := query.Order("last_seen_at asc, id asc").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return projectionDriftNotificationsFromRows(rows), nil
}

func (s *Service) ListResolvedProjectionDriftNotifications(ctx context.Context, since time.Time, limit int) ([]ProjectionDriftNotification, error) {
	if s == nil {
		return nil, fmt.Errorf("service is nil")
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if since.IsZero() {
		since = time.Now().UTC()
	}
	var rows []db.ProjectionRefState
	if err := s.DBForCtx(ctx).
		Where("status = ?", ProjectionStatusResolved).
		Where("resolved_at IS NOT NULL").
		Where("resolved_at >= ?", since.UTC()).
		Where("last_notified_at IS NOT NULL").
		Where("resolved_at > last_notified_at").
		Order("resolved_at asc, id asc").
		Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return projectionDriftNotificationsFromRows(rows), nil
}

func projectionDriftNotificationsFromRows(rows []db.ProjectionRefState) []ProjectionDriftNotification {
	out := make([]ProjectionDriftNotification, 0, len(rows))
	for _, row := range rows {
		note := ProjectionDriftNotification{
			StateID:      row.ID,
			Generation:   row.Generation,
			Provider:     row.Provider,
			RepoFullName: row.RepoFullName,
			TargetRepo:   row.TargetRepo,
			Ref:          row.Ref,
			Branch:       row.Branch,
			Type:         row.Type,
			Authority:    row.Authority,
			AGSSHA:       row.AGSSHA,
			ForgejoSHA:   row.ExternalSHA,
			ErrorSummary: string(row.ErrorSummary),
			FirstSeenAt:  row.FirstSeenAt,
			LastSeenAt:   row.LastSeenAt,
		}
		if row.ResolvedAt != nil {
			note.ResolvedAt = *row.ResolvedAt
		}
		out = append(out, note)
	}
	return out
}

func (s *Service) MarkProjectionDriftNotified(ctx context.Context, stateID uint, notifiedAt time.Time) error {
	if s == nil || stateID == 0 {
		return nil
	}
	if notifiedAt.IsZero() {
		notifiedAt = time.Now().UTC()
	}
	return s.DBForCtx(ctx).Model(&db.ProjectionRefState{}).Where("id = ?", stateID).Update("last_notified_at", notifiedAt).Error
}
