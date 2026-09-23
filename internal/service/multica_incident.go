package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/ngaut/agent-git-service/internal/db"
)

const (
	MulticaExternalSource                = "multica"
	MulticaTaskFailedEventType           = "multica.task_failed.v1"
	RepoIncidentStatusOpen               = "open"
	RepoIncidentStatusExpired            = "expired"
	MulticaIncidentWindowDays            = 30
	multicaIncidentSystemLogin           = "ags-bot"
	multicaIncidentRecentMaxRows         = 20
	MulticaIncidentDefaultNotifyThrottle = 15 * time.Minute
)

// MulticaTaskFailedInput is the AGS-accepted fact shape for a Multica failed
// runtime task. Multica remains the external runtime source; AGS stores this
// as an ExternalEvent and projects it into a repo incident issue.
type MulticaTaskFailedInput struct {
	RepoFullName      string
	WorkspaceID       string
	MulticaIssueKey   string
	MulticaIssueTitle string
	TaskID            string
	AgentID           string
	AgentName         string
	FailureReason     string
	Error             string
	RuntimeProvider   string
	RuntimeID         string
	Attempt           int
	MaxAttempts       int
	OccurredAt        time.Time
	MulticaIssueURL   string
	AGSPrURL          string
}

type MulticaIncidentResult struct {
	Event                  db.ExternalEvent
	Incident               db.RepoIncident
	Issue                  db.Issue
	EventAccepted          bool
	IssueCreated           bool
	NotificationSent       bool
	NotificationSuppressed bool
	NotificationError      string
}

// MulticaIncidentNotifier delivers accepted incident projections to an external
// notification sink. Implementations must send summaries, not raw event logs.
type MulticaIncidentNotifier interface {
	NotifyMulticaIncident(context.Context, MulticaIncidentNotification) error
}

// MulticaIncidentNotification is the compact downstream notification shape.
type MulticaIncidentNotification struct {
	RepoFullName      string
	AggregateKey      string
	IssueNumber       int
	IssueTitle        string
	IssueURL          string
	MulticaIssueKey   string
	MulticaIssueTitle string
	MulticaIssueURL   string
	AgentID           string
	AgentName         string
	FailureReason     string
	Error             string
	RuntimeProvider   string
	RuntimeID         string
	TaskID            string
	Attempt           int
	MaxAttempts       int
	OccurredAt        time.Time
	EventCount30d     int
	TotalEventCount   int
}

type MulticaIncidentGCResult struct {
	Scanned int
	Closed  int
}

type multicaIncidentRender struct {
	Body                string
	WindowCount         int
	TotalCount          int
	EventsOutsideWindow int
	Latest              db.ExternalEvent
}

// RecordMulticaTaskFailure accepts one Multica task failure, deduplicates it,
// and creates or updates the corresponding repo incident issue. The issue body
// is a rolling 30-day projection; the ExternalEvent row is the durable fact.
func (s *Service) RecordMulticaTaskFailure(ctx context.Context, in MulticaTaskFailedInput) (MulticaIncidentResult, error) {
	if strings.TrimSpace(in.RepoFullName) == "" {
		return MulticaIncidentResult{}, fmt.Errorf("multica task failure: repo full name required")
	}
	if strings.TrimSpace(in.TaskID) == "" {
		return MulticaIncidentResult{}, fmt.Errorf("multica task failure: task id required")
	}
	if in.OccurredAt.IsZero() {
		in.OccurredAt = time.Now().UTC()
	} else {
		in.OccurredAt = in.OccurredAt.UTC()
	}
	in.FailureReason = normalizeIncidentValue(in.FailureReason, "agent_error.unknown")
	in.RuntimeProvider = normalizeIncidentValue(in.RuntimeProvider, "unknown")

	repo, err := s.GetRepo(ctx, in.RepoFullName)
	if err != nil {
		return MulticaIncidentResult{}, fmt.Errorf("multica task failure: repo: %w", err)
	}
	aggregateKey := MulticaFailureAggregateKey(in)
	eventKey := MulticaTaskFailedEventKey(in)
	payload, err := json.Marshal(in)
	if err != nil {
		return MulticaIncidentResult{}, fmt.Errorf("multica task failure: payload: %w", err)
	}

	acceptedAt := time.Now().UTC()
	var result MulticaIncidentResult
	err = s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		txCtx := ContextWithDB(ctx, tx)
		labels, err := s.ensureMulticaIncidentLabels(txCtx, repo.FullName, in.FailureReason)
		if err != nil {
			return err
		}

		event := db.ExternalEvent{
			EventKey:     eventKey,
			Source:       MulticaExternalSource,
			Type:         MulticaTaskFailedEventType,
			RepositoryID: repo.ID,
			AggregateKey: aggregateKey,
			Payload:      db.LargeText(payload),
			OccurredAt:   in.OccurredAt,
			AcceptedAt:   acceptedAt,
		}
		create := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&event)
		if create.Error != nil {
			return fmt.Errorf("create external event: %w", create.Error)
		}
		if create.RowsAffected == 0 {
			if err := tx.Where("event_key = ?", eventKey).First(&event).Error; err != nil {
				return fmt.Errorf("load duplicate external event: %w", err)
			}
			// The same durable event may be observed after mutable Multica metadata
			// (for example ags_repo) moves. A duplicate must resolve through the
			// aggregate accepted with that event, not today's recomputed aggregate.
			incident, issue, loadErr := s.loadIncidentByAggregate(txCtx, event.AggregateKey)
			if loadErr != nil {
				return loadErr
			}
			result = MulticaIncidentResult{Event: event, Incident: incident, Issue: issue, EventAccepted: false, IssueCreated: false}
			return nil
		}
		result.EventAccepted = true
		result.Event = event

		incident, issue, created, err := s.getOrCreateMulticaIncidentIssue(txCtx, repo, aggregateKey, in, labels)
		if err != nil {
			return err
		}
		rendered, err := s.renderMulticaIncident(txCtx, repo, aggregateKey, in.OccurredAt, RepoIncidentStatusOpen)
		if err != nil {
			return err
		}
		if issue.State == db.StateClosed {
			open := db.StateOpen
			if issue, err = s.UpdateIssue(txCtx, repo.FullName, issue.Number, UpdateIssueInput{State: &open, Body: &rendered.Body}); err != nil {
				return fmt.Errorf("reopen incident issue: %w", err)
			}
		} else {
			if issue, err = s.UpdateIssue(txCtx, repo.FullName, issue.Number, UpdateIssueInput{Body: &rendered.Body}); err != nil {
				return fmt.Errorf("update incident issue: %w", err)
			}
		}
		if _, err := s.AddIssueLabels(txCtx, repo.FullName, issue.Number, labels); err != nil {
			return fmt.Errorf("ensure incident labels on issue: %w", err)
		}
		incident.IssueID = issue.ID
		incident.EventCount30d = rendered.WindowCount
		incident.TotalEventCount = rendered.TotalCount
		incident.LastEventKey = event.EventKey
		incident.LastEventAt = &event.OccurredAt
		incident.Status = RepoIncidentStatusOpen
		incident.WindowDays = MulticaIncidentWindowDays
		if err := tx.Save(&incident).Error; err != nil {
			return fmt.Errorf("save repo incident: %w", err)
		}
		result.Incident = incident
		result.Issue = issue
		result.IssueCreated = created
		return nil
	})
	if err != nil {
		return MulticaIncidentResult{}, err
	}
	if result.EventAccepted {
		s.notifyMulticaIncident(ctx, in, &result)
	}
	return result, nil
}

func (s *Service) notifyMulticaIncident(ctx context.Context, in MulticaTaskFailedInput, result *MulticaIncidentResult) {
	if s == nil || s.MulticaIncidentNotifier == nil || result == nil || !result.EventAccepted {
		return
	}
	now := result.Event.AcceptedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	throttle := s.MulticaIncidentNotifyThrottle
	if throttle == 0 {
		throttle = MulticaIncidentDefaultNotifyThrottle
	}
	if throttle > 0 && result.Incident.LastNotifiedAt != nil && !result.Incident.LastNotifiedAt.IsZero() && result.Incident.LastNotifiedAt.Add(throttle).After(now) {
		result.NotificationSuppressed = true
		s.closeMulticaIncidentIssueAfterNotification(ctx, in.RepoFullName, result, "feishu_throttled_covered", now)
		return
	}
	note := s.buildMulticaIncidentNotification(in, *result)
	if err := s.MulticaIncidentNotifier.NotifyMulticaIncident(ctx, note); err != nil {
		result.NotificationError = err.Error()
		slog.Warn("multica incident notification failed", "aggregate_key", result.Incident.AggregateKey, "event_key", result.Event.EventKey, "error", err)
		return
	}
	result.NotificationSent = true
	result.Incident.LastNotifiedAt = &now
	result.Incident.LastNotifiedEventKey = result.Event.EventKey
	if result.Incident.ID == 0 {
		return
	}
	if err := s.DBForCtx(ctx).Model(&db.RepoIncident{}).Where("id = ?", result.Incident.ID).Updates(map[string]any{
		"last_notified_at":        now,
		"last_notified_event_key": result.Event.EventKey,
	}).Error; err != nil {
		result.NotificationError = err.Error()
		slog.Warn("multica incident notification checkpoint failed", "aggregate_key", result.Incident.AggregateKey, "event_key", result.Event.EventKey, "error", err)
		return
	}
	s.closeMulticaIncidentIssueAfterNotification(ctx, in.RepoFullName, result, "feishu_delivered", now)
}

func (s *Service) closeMulticaIncidentIssueAfterNotification(ctx context.Context, repoFullName string, result *MulticaIncidentResult, notificationStatus string, at time.Time) {
	if strings.TrimSpace(repoFullName) == "" && result != nil {
		repoFullName = result.Incident.Repository.FullName
	}
	if s == nil || result == nil || result.Issue.Number <= 0 || strings.TrimSpace(repoFullName) == "" {
		return
	}
	body := annotateMulticaIncidentNotificationBody(string(result.Issue.Body), notificationStatus, result.Event.EventKey, at)
	closed := db.StateClosed
	reason := db.StateReasonCompleted
	issue, err := s.UpdateIssue(ctx, repoFullName, result.Issue.Number, UpdateIssueInput{
		Body:        &body,
		State:       &closed,
		StateReason: &reason,
	})
	if err != nil {
		result.NotificationError = err.Error()
		slog.Warn("multica incident notification close failed", "aggregate_key", result.Incident.AggregateKey, "event_key", result.Event.EventKey, "issue", result.Issue.Number, "error", err)
		return
	}
	result.Issue = issue
}

func annotateMulticaIncidentNotificationBody(body, notificationStatus, eventKey string, at time.Time) string {
	marker := "<!-- ags:multica-incident-notification -->"
	trimmed := strings.TrimSpace(body)
	lines := []string{
		marker,
		"## Notification",
		"notification: " + normalizeIncidentValue(notificationStatus, "feishu_delivered"),
	}
	if strings.TrimSpace(eventKey) != "" {
		lines = append(lines, "event: "+strings.TrimSpace(eventKey))
	}
	if !at.IsZero() {
		lines = append(lines, "at: "+at.UTC().Format(time.RFC3339))
	}
	section := strings.Join(lines, "\n")
	if trimmed == "" {
		return section + "\n"
	}
	idx := strings.Index(trimmed, marker)
	if idx >= 0 {
		return strings.TrimSpace(trimmed[:idx]) + "\n\n" + section + "\n"
	}
	return trimmed + "\n\n" + section + "\n"
}

func (s *Service) buildMulticaIncidentNotification(in MulticaTaskFailedInput, result MulticaIncidentResult) MulticaIncidentNotification {
	return MulticaIncidentNotification{
		RepoFullName:      in.RepoFullName,
		AggregateKey:      result.Incident.AggregateKey,
		IssueNumber:       result.Issue.Number,
		IssueTitle:        result.Issue.Title,
		IssueURL:          s.multicaIncidentIssueURL(in.RepoFullName, result.Issue.Number),
		MulticaIssueKey:   in.MulticaIssueKey,
		MulticaIssueTitle: in.MulticaIssueTitle,
		MulticaIssueURL:   in.MulticaIssueURL,
		AgentID:           in.AgentID,
		AgentName:         in.AgentName,
		FailureReason:     in.FailureReason,
		Error:             in.Error,
		RuntimeProvider:   in.RuntimeProvider,
		RuntimeID:         in.RuntimeID,
		TaskID:            in.TaskID,
		Attempt:           in.Attempt,
		MaxAttempts:       in.MaxAttempts,
		OccurredAt:        in.OccurredAt,
		EventCount30d:     result.Incident.EventCount30d,
		TotalEventCount:   result.Incident.TotalEventCount,
	}
}

func (s *Service) multicaIncidentIssueURL(repoFullName string, number int) string {
	baseURL := ""
	if s != nil {
		baseURL = strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	}
	if baseURL == "" || strings.TrimSpace(repoFullName) == "" || number <= 0 {
		return ""
	}
	return fmt.Sprintf("%s/api/v3/repos/%s/issues/%d", baseURL, strings.Trim(strings.TrimSpace(repoFullName), "/"), number)
}

// GCMulticaIncidentIssues expires generated Multica incident issues whose
// aggregate has no events inside the last 30 days. It closes rather than
// deletes; hard deletion can be a later policy layer after digest retention.
func (s *Service) GCMulticaIncidentIssues(ctx context.Context, now time.Time) (MulticaIncidentGCResult, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var incidents []db.RepoIncident
	if err := s.DBForCtx(ctx).Preload("Repository").Preload("Issue").
		Where("source = ? AND status <> ?", MulticaExternalSource, RepoIncidentStatusExpired).
		Find(&incidents).Error; err != nil {
		return MulticaIncidentGCResult{}, err
	}
	res := MulticaIncidentGCResult{Scanned: len(incidents)}
	for _, incident := range incidents {
		rendered, err := s.renderMulticaIncident(ctx, incident.Repository, incident.AggregateKey, now, RepoIncidentStatusOpen)
		if err != nil {
			return res, err
		}
		incident.EventCount30d = rendered.WindowCount
		incident.TotalEventCount = rendered.TotalCount
		if rendered.WindowCount > 0 {
			if rendered.Latest.ID != 0 {
				incident.LastEventKey = rendered.Latest.EventKey
				incident.LastEventAt = &rendered.Latest.OccurredAt
			}
			if err := s.DBForCtx(ctx).Save(&incident).Error; err != nil {
				return res, err
			}
			continue
		}
		expired, err := s.renderMulticaIncident(ctx, incident.Repository, incident.AggregateKey, now, RepoIncidentStatusExpired)
		if err != nil {
			return res, err
		}
		closed := db.StateClosed
		reason := db.StateReasonNotPlanned
		if _, err := s.UpdateIssue(ctx, incident.Repository.FullName, incident.Issue.Number, UpdateIssueInput{State: &closed, StateReason: &reason, Body: &expired.Body}); err != nil {
			return res, fmt.Errorf("close expired incident issue #%d: %w", incident.Issue.Number, err)
		}
		incident.Status = RepoIncidentStatusExpired
		incident.EventCount30d = 0
		incident.TotalEventCount = expired.TotalCount
		if err := s.DBForCtx(ctx).Save(&incident).Error; err != nil {
			return res, err
		}
		res.Closed++
	}
	return res, nil
}

func MulticaTaskFailedEventKey(in MulticaTaskFailedInput) string {
	occurred := in.OccurredAt.UTC().Format(time.RFC3339Nano)
	return "multica:task_failed:" + normalizeIncidentValue(in.WorkspaceID, "unknown-workspace") + ":" + strings.TrimSpace(in.TaskID) + ":" + occurred
}

func MulticaFailureAggregateKey(in MulticaTaskFailedInput) string {
	repo := strings.ToLower(strings.TrimSpace(in.RepoFullName))
	reason := normalizeIncidentValue(in.FailureReason, "agent_error.unknown")
	provider := normalizeIncidentValue(in.RuntimeProvider, "unknown")
	if multicaInfraAggregateReason(reason) {
		return fmt.Sprintf("repo=%s|reason=%s|provider=%s", repo, reason, provider)
	}
	issue := normalizeIncidentValue(in.MulticaIssueKey, "workspace:"+normalizeIncidentValue(in.WorkspaceID, "unknown"))
	agent := normalizeIncidentValue(firstIncidentNonEmpty(in.AgentName, in.AgentID), "unknown-agent")
	return fmt.Sprintf("repo=%s|issue=%s|agent=%s|reason=%s", repo, issue, agent, reason)
}

func multicaInfraAggregateReason(reason string) bool {
	switch reason {
	case "queued_expired", "runtime_offline", "timeout":
		return true
	default:
		return false
	}
}

func (s *Service) getOrCreateMulticaIncidentIssue(ctx context.Context, repo db.Repository, aggregateKey string, in MulticaTaskFailedInput, labels []string) (db.RepoIncident, db.Issue, bool, error) {
	if incident, issue, err := s.loadIncidentByAggregate(ctx, aggregateKey); err == nil {
		return incident, issue, false, nil
	}
	if err := s.ensureMulticaIncidentActor(ctx); err != nil {
		return db.RepoIncident{}, db.Issue{}, false, err
	}
	title := multicaIncidentTitle(repo.FullName, in)
	body := "<!-- ags:multica-incident pending -->\n"
	issue, err := s.CreateIssue(ctx, CreateIssueInput{
		RepoFullName: repo.FullName,
		Title:        title,
		Body:         body,
		AuthorLogin:  multicaIncidentSystemLogin,
		Labels:       labels,
	})
	if err != nil {
		return db.RepoIncident{}, db.Issue{}, false, fmt.Errorf("create incident issue: %w", err)
	}
	incident := db.RepoIncident{
		Source:       MulticaExternalSource,
		Type:         MulticaTaskFailedEventType,
		AggregateKey: aggregateKey,
		RepositoryID: repo.ID,
		IssueID:      issue.ID,
		WindowDays:   MulticaIncidentWindowDays,
		Status:       RepoIncidentStatusOpen,
	}
	if err := s.DBForCtx(ctx).Create(&incident).Error; err != nil {
		return db.RepoIncident{}, db.Issue{}, false, fmt.Errorf("create repo incident: %w", err)
	}
	return incident, issue, true, nil
}

func (s *Service) loadIncidentByAggregate(ctx context.Context, aggregateKey string) (db.RepoIncident, db.Issue, error) {
	var incident db.RepoIncident
	if err := s.DBForCtx(ctx).Preload("Repository").Preload("Issue").
		Where("source = ? AND aggregate_key = ?", MulticaExternalSource, aggregateKey).
		First(&incident).Error; err != nil {
		return db.RepoIncident{}, db.Issue{}, err
	}
	return incident, incident.Issue, nil
}

func (s *Service) renderMulticaIncident(ctx context.Context, repo db.Repository, aggregateKey string, now time.Time, status string) (multicaIncidentRender, error) {
	cutoff := now.UTC().AddDate(0, 0, -MulticaIncidentWindowDays)
	var total int64
	if err := s.DBForCtx(ctx).Model(&db.ExternalEvent{}).
		Where("source = ? AND aggregate_key = ?", MulticaExternalSource, aggregateKey).
		Count(&total).Error; err != nil {
		return multicaIncidentRender{}, err
	}
	var recent []db.ExternalEvent
	if err := s.DBForCtx(ctx).
		Where("source = ? AND aggregate_key = ? AND occurred_at >= ?", MulticaExternalSource, aggregateKey, cutoff).
		Order("occurred_at desc, id desc").
		Find(&recent).Error; err != nil {
		return multicaIncidentRender{}, err
	}
	latest := db.ExternalEvent{}
	if len(recent) > 0 {
		latest = recent[0]
	}
	display := recent
	if len(display) > multicaIncidentRecentMaxRows {
		display = display[:multicaIncidentRecentMaxRows]
	}
	var b strings.Builder
	b.WriteString("<!-- ags:multica-incident aggregate_key=")
	b.WriteString(aggregateKey)
	b.WriteString(" -->\n")
	b.WriteString("## Rolling Window\n")
	b.WriteString("status: ")
	b.WriteString(status)
	b.WriteString("\n")
	b.WriteString("window: last 30 days\n")
	b.WriteString(fmt.Sprintf("count: %d\n", len(recent)))
	if len(recent) > 0 {
		b.WriteString("latest: ")
		b.WriteString(recent[0].OccurredAt.UTC().Format(time.RFC3339))
		b.WriteString("\n")
	}
	b.WriteString("repo: ")
	b.WriteString(repo.FullName)
	b.WriteString("\n")
	b.WriteString("aggregate_key: ")
	b.WriteString(aggregateKey)
	b.WriteString("\n\n")

	if len(recent) > 0 {
		latestInput := decodeMulticaTaskFailedPayload(recent[0].Payload)
		b.WriteString("## Latest\n")
		writeIncidentField(&b, "task_id", latestInput.TaskID)
		writeIncidentField(&b, "issue", latestInput.MulticaIssueKey)
		writeIncidentField(&b, "agent", firstIncidentNonEmpty(latestInput.AgentName, latestInput.AgentID))
		writeIncidentField(&b, "runtime", firstIncidentNonEmpty(latestInput.RuntimeProvider, latestInput.RuntimeID))
		writeIncidentField(&b, "reason", latestInput.FailureReason)
		if latestInput.Attempt > 0 || latestInput.MaxAttempts > 0 {
			writeIncidentField(&b, "attempt", fmt.Sprintf("%d/%d", latestInput.Attempt, latestInput.MaxAttempts))
		}
		writeIncidentField(&b, "error", truncateForIncident(latestInput.Error, 500))
		b.WriteString("\n")
	}

	b.WriteString("## Recent Events\n")
	if len(display) == 0 {
		b.WriteString("- none in rolling window\n")
	} else {
		for _, ev := range display {
			input := decodeMulticaTaskFailedPayload(ev.Payload)
			b.WriteString("- ")
			b.WriteString(ev.OccurredAt.UTC().Format(time.RFC3339))
			b.WriteString(" task `")
			b.WriteString(input.TaskID)
			b.WriteString("`")
			if input.MulticaIssueKey != "" {
				b.WriteString(" issue ")
				b.WriteString(input.MulticaIssueKey)
			}
			if firstIncidentNonEmpty(input.AgentName, input.AgentID) != "" {
				b.WriteString(" agent ")
				b.WriteString(firstIncidentNonEmpty(input.AgentName, input.AgentID))
			}
			b.WriteString(" reason ")
			b.WriteString(input.FailureReason)
			if input.Error != "" {
				b.WriteString(" — ")
				b.WriteString(truncateForIncident(input.Error, 180))
			}
			b.WriteString("\n")
		}
	}
	outside := int(total) - len(recent)
	if outside < 0 {
		outside = 0
	}
	b.WriteString("\n## Archived Summary\n")
	b.WriteString(fmt.Sprintf("total_events: %d\n", total))
	b.WriteString(fmt.Sprintf("events_outside_window: %d\n", outside))
	return multicaIncidentRender{Body: b.String(), WindowCount: len(recent), TotalCount: int(total), EventsOutsideWindow: outside, Latest: latest}, nil
}

func decodeMulticaTaskFailedPayload(raw db.LargeText) MulticaTaskFailedInput {
	var input MulticaTaskFailedInput
	_ = json.Unmarshal([]byte(raw), &input)
	return input
}

func (s *Service) ensureMulticaIncidentActor(ctx context.Context) error {
	var user db.User
	dbq := s.DBForCtx(ctx)
	if err := dbq.Where("login = ?", multicaIncidentSystemLogin).First(&user).Error; err == nil {
		return nil
	} else if err != nil && err != gorm.ErrRecordNotFound {
		return err
	}
	return dbq.Create(&db.User{Login: multicaIncidentSystemLogin, Name: "AGS Bot", Type: db.TypeUser, UserKind: db.UserKindAgent}).Error
}

func (s *Service) ensureMulticaIncidentLabels(ctx context.Context, repoFullName, reason string) ([]string, error) {
	labels := []string{"source/multica", "kind/runtime-failure", "status/needs-triage"}
	if reason != "" {
		labels = append(labels, "reason/"+safeLabelSuffix(reason))
	}
	colors := map[string]string{
		"source/multica":       "5319e7",
		"kind/runtime-failure": "d73a4a",
		"status/needs-triage":  "fbca04",
	}
	for _, name := range labels {
		if _, err := s.GetLabel(ctx, repoFullName, name); err == nil {
			continue
		}
		color := colors[name]
		if color == "" {
			color = "cfd3d7"
		}
		if _, err := s.CreateLabel(ctx, repoFullName, name, color, "AGS-managed Multica incident label"); err != nil && !strings.Contains(err.Error(), "already exists") {
			return nil, err
		}
	}
	return labels, nil
}

func multicaIncidentTitle(repoFullName string, in MulticaTaskFailedInput) string {
	reason := normalizeIncidentValue(in.FailureReason, "agent_error.unknown")
	if multicaInfraAggregateReason(reason) {
		return fmt.Sprintf("[multica-failure] %s %s %s", repoFullName, normalizeIncidentValue(in.RuntimeProvider, "unknown"), reason)
	}
	parts := []string{"[multica-failure]"}
	if in.MulticaIssueKey != "" {
		parts = append(parts, in.MulticaIssueKey)
	} else {
		parts = append(parts, repoFullName)
	}
	parts = append(parts, normalizeIncidentValue(firstIncidentNonEmpty(in.AgentName, in.AgentID), "unknown-agent"), reason)
	return strings.Join(parts, " ")
}

func writeIncidentField(b *strings.Builder, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\n")
}

func normalizeIncidentValue(v, fallback string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	return v
}

func firstIncidentNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func safeLabelSuffix(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "unknown"
	}
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}

func truncateForIncident(s string, max int) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// stable ordering helper kept here for future rule expansion and tests.
func sortedIncidentLabelNames(labels []db.Label) []string {
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		names = append(names, label.Name)
	}
	sort.Strings(names)
	return names
}
