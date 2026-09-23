package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/ngaut/agent-git-service/internal/db"
)

const (
	ProjectionAlertingOutcomeTargetMissing     = "target_missing"
	ProjectionAlertingOutcomeDispatcherMissing = "dispatcher_missing"
	ProjectionAlertingOutcomeDeliveryFailed    = "delivery_failed"
)

// ProjectionAlertingError reports an observable alerting/configuration outcome
// without implying that the durable projection failure itself was rolled back.
type ProjectionAlertingError struct {
	Outcome string
	Cause   error
}

func (e *ProjectionAlertingError) Error() string {
	if e == nil {
		return "projection alerting failed"
	}
	if e.Cause == nil {
		return "projection alerting " + e.Outcome
	}
	return fmt.Sprintf("projection alerting %s: %v", e.Outcome, e.Cause)
}

func (e *ProjectionAlertingError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// IsProjectionAlertingError reports whether durable drift recording succeeded
// but its immediate alert path returned a typed configuration/delivery outcome.
func IsProjectionAlertingError(err error) bool {
	var alertErr *ProjectionAlertingError
	return errors.As(err, &alertErr)
}

func (s *Service) NotifyProjectionDrift(ctx context.Context, note ProjectionDriftNotification) error {
	deliveries, err := s.enqueueProjectionDriftOutboundIntents(ctx, note, false)
	if err != nil {
		return err
	}
	return s.deliverProjectionDriftOutboundNow(ctx, deliveries)
}

func (s *Service) NotifyProjectionDriftResolved(ctx context.Context, note ProjectionDriftNotification) error {
	deliveries, err := s.enqueueProjectionDriftOutboundIntents(ctx, note, true)
	if err != nil {
		return err
	}
	return s.deliverProjectionDriftOutboundNow(ctx, deliveries)
}

func (s *Service) enqueueProjectionDriftOutboundIntents(ctx context.Context, note ProjectionDriftNotification, resolved bool) ([]db.OutboundDelivery, error) {
	if s == nil {
		return nil, &ProjectionAlertingError{Outcome: ProjectionAlertingOutcomeTargetMissing, Cause: fmt.Errorf("service is nil")}
	}
	targets := s.OutboundEventTargets[OutboundEventProjectionDrift]
	if len(targets) == 0 {
		return nil, &ProjectionAlertingError{Outcome: ProjectionAlertingOutcomeTargetMissing, Cause: fmt.Errorf("projection_drift outbound target is not configured")}
	}
	state := "active"
	if resolved {
		state = "resolved"
	}
	generation := note.Generation
	if generation == 0 {
		generation = 1
	}
	provider := strings.TrimSpace(note.Provider)
	if provider == "" {
		provider = ProjectionProviderForgejo
	}
	deliveries := make([]db.OutboundDelivery, 0, len(targets))
	for _, target := range targets {
		text := renderProjectionDriftOutboundText(note, resolved)
		refKey := firstNonEmptyOutboundString(note.Ref, note.Branch, fmt.Sprint(note.StateID))
		intent := OutboundDeliveryIntent{
			EventType:      OutboundEventProjectionDrift,
			TargetName:     strings.TrimSpace(target.Name),
			TargetType:     strings.TrimSpace(strings.ToLower(target.Type)),
			SubjectType:    "projection_ref",
			SubjectKey:     fmt.Sprintf("%s:%s:%s:g%d:%s", provider, strings.TrimSpace(note.RepoFullName), state, generation, refKey),
			IdempotencyKey: fmt.Sprintf("%s:%s:%s:%s:%s:g%d:%s", OutboundEventProjectionDrift, provider, state, strings.TrimSpace(note.RepoFullName), refKey, generation, strings.TrimSpace(target.Name)),
			PayloadVersion: "projection_drift_text_v2",
			PayloadJSON:    outboundTextPayloadJSON(text, false),
			RepoFullName:   strings.TrimSpace(note.RepoFullName),
		}
		if intent.TargetName == "" || intent.TargetType == "" {
			return nil, &ProjectionAlertingError{Outcome: ProjectionAlertingOutcomeTargetMissing, Cause: fmt.Errorf("projection_drift outbound target name/type is empty")}
		}
		delivery, _, err := s.EnqueueOutboundDelivery(ctx, intent)
		if err != nil {
			// Intent persistence is part of the projection-failure transaction.
			// Keep this as an ordinary persistence error so callers do not mistake
			// a rolled-back transaction for a post-commit delivery degradation.
			return nil, fmt.Errorf("enqueue projection drift outbound intent: %w", err)
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, nil
}

func (s *Service) deliverProjectionDriftOutboundNow(ctx context.Context, deliveries []db.OutboundDelivery) error {
	if s == nil || s.OutboundDispatcher == nil {
		return &ProjectionAlertingError{Outcome: ProjectionAlertingOutcomeDispatcherMissing, Cause: fmt.Errorf("outbound dispatcher is not configured")}
	}
	for _, delivery := range deliveries {
		if delivery.Status == OutboundDeliveryStatusDelivered {
			continue
		}
		if err := s.DeliverOutboundDeliveryNow(ctx, delivery.ID); err != nil {
			return &ProjectionAlertingError{Outcome: ProjectionAlertingOutcomeDeliveryFailed, Cause: err}
		}
	}
	return nil
}

func sanitizeProjectionAlertURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func renderProjectionDriftOutboundText(note ProjectionDriftNotification, resolved bool) string {
	title := projectionDriftOutboundTitle(note.Provider, resolved)
	lines := []string{
		title,
		"Repo: " + note.RepoFullName,
		"Provider: " + firstNonEmptyOutboundString(note.Provider, ProjectionProviderForgejo),
		"Ref: " + firstNonEmptyOutboundString(note.Ref, note.Branch),
		"Type: " + note.Type,
	}
	if note.AGSPRNumber != 0 {
		lines = append(lines, fmt.Sprintf("AGS repo/PR: %s#%d", note.RepoFullName, note.AGSPRNumber))
	}
	if note.AGSPRURL != "" {
		lines = append(lines, "AGS PR URL: "+note.AGSPRURL)
	}
	if note.ForgejoPRNumber != 0 || note.ForgejoPRURL != "" {
		lines = append(lines, "Forgejo PR: "+firstNonEmptyOutboundString(note.ForgejoPRURL, fmt.Sprintf("%s#%d", note.TargetRepo, note.ForgejoPRNumber)))
	}
	if note.Actor != "" {
		lines = append(lines, "Actor: "+note.Actor)
	}
	if note.ActionPhase != "" {
		lines = append(lines, "Action phase: "+note.ActionPhase)
	}
	if note.Branch != "" {
		lines = append(lines, "Branch: "+note.Branch)
	}
	for _, sha := range []struct{ label, value string }{
		{"AGS old SHA", note.AGSOldSHA}, {"AGS new SHA", note.AGSNewSHA},
		{"Expected Forgejo SHA", note.ExpectedForgejoSHA}, {"Actual Forgejo SHA", note.ActualForgejoSHA},
	} {
		if sha.value != "" {
			lines = append(lines, sha.label+": "+sha.value)
		}
	}
	if note.CorrelationID != "" {
		lines = append(lines, "Correlation ID: "+note.CorrelationID)
	}
	if note.RecoveryHint != "" {
		lines = append(lines, "Recovery: "+note.RecoveryHint)
	}
	if note.AGSSHA != "" {
		lines = append(lines, "AGS SHA: "+shortOutboundSHA(note.AGSSHA))
	}
	if note.ForgejoSHA != "" {
		lines = append(lines, "External SHA: "+shortOutboundSHA(note.ForgejoSHA))
	}
	if note.ErrorSummary != "" {
		lines = append(lines, "Error: "+note.ErrorSummary)
	}
	if !note.FirstSeenAt.IsZero() {
		lines = append(lines, "First seen: "+note.FirstSeenAt.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	if !note.ResolvedAt.IsZero() {
		lines = append(lines, "Resolved at: "+note.ResolvedAt.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	return strings.Join(lines, "\n")
}

func projectionDriftOutboundTitle(provider string, resolved bool) string {
	if strings.EqualFold(strings.TrimSpace(provider), ProjectionProviderGitLab) {
		if resolved {
			return "AGS GitLab projection drift resolved"
		}
		return "AGS GitLab projection drift detected"
	}
	if strings.EqualFold(strings.TrimSpace(provider), ProjectionProviderGitHub) {
		if resolved {
			return "AGS GitHub projection drift resolved"
		}
		return "AGS GitHub projection drift detected"
	}
	if resolved {
		return "AGS Forgejo projection drift resolved"
	}
	return "AGS Forgejo projection drift detected"
}
