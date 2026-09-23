package service

import (
	"context"
	"fmt"
	"strings"
)

func (s *Service) NotifyMulticaIncident(ctx context.Context, note MulticaIncidentNotification) error {
	if s == nil || s.OutboundDispatcher == nil || len(s.OutboundEventTargets[OutboundEventMulticaIncident]) == 0 {
		return nil
	}
	for _, target := range s.OutboundEventTargets[OutboundEventMulticaIncident] {
		key := firstNonEmptyOutboundString(note.AggregateKey, note.TaskID, note.FailureReason)
		intent := OutboundDeliveryIntent{
			EventType:      OutboundEventMulticaIncident,
			TargetName:     strings.TrimSpace(target.Name),
			TargetType:     strings.TrimSpace(strings.ToLower(target.Type)),
			SubjectType:    "multica_incident",
			SubjectKey:     fmt.Sprintf("%s:%s", strings.TrimSpace(note.RepoFullName), key),
			IdempotencyKey: fmt.Sprintf("%s:%s:%s:%s", OutboundEventMulticaIncident, strings.TrimSpace(note.RepoFullName), key, strings.TrimSpace(target.Name)),
			PayloadVersion: "multica_incident_text_v1",
			PayloadJSON:    outboundTextPayloadJSON(renderMulticaIncidentOutboundText(note), false),
			RepoFullName:   strings.TrimSpace(note.RepoFullName),
		}
		if intent.TargetName == "" || intent.TargetType == "" {
			continue
		}
		delivery, created, err := s.EnqueueOutboundDelivery(ctx, intent)
		if err != nil {
			return err
		}
		if !created && delivery.Status == OutboundDeliveryStatusDelivered {
			continue
		}
		if err := s.DeliverOutboundDeliveryNow(ctx, delivery.ID); err != nil {
			return err
		}
	}
	return nil
}

func renderMulticaIncidentOutboundText(note MulticaIncidentNotification) string {
	lines := []string{
		"Multica task failure accepted",
		"Repo: " + note.RepoFullName,
	}
	if note.IssueURL != "" {
		lines = append(lines, fmt.Sprintf("AGS issue: #%d %s", note.IssueNumber, note.IssueURL))
	}
	if note.MulticaIssueURL != "" || note.MulticaIssueKey != "" {
		lines = append(lines, "Multica: "+strings.TrimSpace(note.MulticaIssueKey+" "+note.MulticaIssueURL))
	}
	if note.AgentName != "" || note.AgentID != "" {
		lines = append(lines, "Agent: "+firstNonEmptyOutboundString(note.AgentName, note.AgentID))
	}
	if note.FailureReason != "" {
		lines = append(lines, "Reason: "+note.FailureReason)
	}
	if note.RuntimeProvider != "" || note.RuntimeID != "" {
		lines = append(lines, "Runtime: "+strings.TrimSpace(note.RuntimeProvider+" "+note.RuntimeID))
	}
	if note.TaskID != "" {
		lines = append(lines, "Task: "+note.TaskID)
	}
	if note.Attempt > 0 || note.MaxAttempts > 0 {
		lines = append(lines, fmt.Sprintf("Attempt: %d/%d", note.Attempt, note.MaxAttempts))
	}
	lines = append(lines, fmt.Sprintf("Window: %d recent / %d total", note.EventCount30d, note.TotalEventCount))
	if !note.OccurredAt.IsZero() {
		lines = append(lines, "Occurred: "+note.OccurredAt.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	if note.Error != "" {
		lines = append(lines, "Error: "+note.Error)
	}
	return strings.Join(lines, "\n")
}
