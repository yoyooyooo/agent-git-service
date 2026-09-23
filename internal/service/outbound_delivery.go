package service

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	OutboundEventPullRequestMerged         = "pull_request_merged"
	OutboundEventProjectionDrift           = "projection_drift"
	OutboundEventMulticaIncident           = "multica_incident"
	OutboundEventMulticaExternalPRTerminal = "multica_external_pr_terminal"

	OutboundTargetTypeFeishuWebhook     = "feishu_webhook"
	OutboundTargetTypeMulticaExternalPR = "multica_external_pr"
	OutboundTargetNameMulticaExternalPR = "multica_external_pr"

	OutboundDeliveryStatusPending    = "pending"
	OutboundDeliveryStatusDelivering = "delivering"
	OutboundDeliveryStatusDelivered  = "delivered"
	OutboundDeliveryStatusRetryWait  = "retry_wait"
	OutboundDeliveryStatusDeadLetter = "dead_letter"
)

const (
	defaultOutboundMaxAttempts = 7
	// OutboundDeliveryLease is the maximum claim window. Typed dispatch
	// timeouts must remain strictly below it; only explicitly idempotent typed
	// Multica rows may be reclaimed after expiry.
	OutboundDeliveryLease = 2 * time.Minute
)

var errOutboundDeliveryLeaseLost = errors.New("outbound delivery lease lost")

type OutboundTarget struct {
	Name        string
	Type        string
	DisplayName string
}

type OutboundDeliveryIntent struct {
	EventType      string
	TargetName     string
	TargetType     string
	SubjectType    string
	SubjectKey     string
	IdempotencyKey string
	PayloadVersion string
	PayloadJSON    string

	RepoFullName   string
	PRNumber       int
	ExternalRepo   string
	ExternalNumber int
	MergeSHA       string
	MaxAttempts    int
}

type OutboundDispatchCall struct {
	Delivery db.OutboundDelivery
	Target   OutboundTarget
}

type OutboundDeliveryResult struct {
	Delivered  bool
	Retryable  bool
	HTTPStatus int
	Code       string
	Message    string
}

// OutboundWorkerHealth is the small readiness/logging surface for the
// in-process durable outbound poller. It deliberately exposes only heartbeat
// timestamps and a secret-free error summary, not payloads or credentials.
type OutboundWorkerHealth struct {
	LastPollAt    time.Time
	LastSuccessAt time.Time
	LastError     string
}

type OutboundDispatcher interface {
	DispatchOutbound(context.Context, OutboundDispatchCall) OutboundDeliveryResult
}

// OutboundDispatcherMux routes each durable row to a dispatcher selected by
// its typed target. A row can never make the Feishu dispatcher handle a
// Multica envelope (or vice versa).
type OutboundDispatcherMux struct {
	byType map[string]OutboundDispatcher
}

func NewOutboundDispatcherMux(dispatchers map[string]OutboundDispatcher) *OutboundDispatcherMux {
	byType := make(map[string]OutboundDispatcher, len(dispatchers))
	for targetType, dispatcher := range dispatchers {
		targetType = strings.TrimSpace(strings.ToLower(targetType))
		if targetType != "" && dispatcher != nil {
			byType[targetType] = dispatcher
		}
	}
	return &OutboundDispatcherMux{byType: byType}
}

func (m *OutboundDispatcherMux) DispatchOutbound(ctx context.Context, call OutboundDispatchCall) OutboundDeliveryResult {
	if m == nil {
		return OutboundDeliveryResult{Code: "outbound_dispatcher_missing", Message: "outbound dispatcher is not configured"}
	}
	targetType := strings.TrimSpace(strings.ToLower(call.Delivery.TargetType))
	dispatcher := m.byType[targetType]
	if dispatcher == nil {
		return OutboundDeliveryResult{Code: "outbound_target_type_unsupported", Message: "outbound target type is not configured"}
	}
	return dispatcher.DispatchOutbound(ctx, call)
}

func (s *Service) EnqueueOutboundDelivery(ctx context.Context, intent OutboundDeliveryIntent) (db.OutboundDelivery, bool, error) {
	if s == nil {
		return db.OutboundDelivery{}, false, fmt.Errorf("service is nil")
	}
	intent.EventType = strings.TrimSpace(intent.EventType)
	intent.TargetName = strings.TrimSpace(intent.TargetName)
	intent.TargetType = strings.TrimSpace(strings.ToLower(intent.TargetType))
	intent.IdempotencyKey = strings.TrimSpace(intent.IdempotencyKey)
	if intent.EventType == "" || intent.TargetName == "" || intent.TargetType == "" || intent.IdempotencyKey == "" {
		return db.OutboundDelivery{}, false, fmt.Errorf("outbound delivery intent requires event type, target name, target type, and idempotency key")
	}
	typedMultica := intent.EventType == OutboundEventMulticaExternalPRTerminal
	typedTarget := intent.TargetType == OutboundTargetTypeMulticaExternalPR
	if typedMultica != typedTarget {
		return db.OutboundDelivery{}, false, fmt.Errorf("Multica terminal event and target must be paired")
	}
	if typedMultica {
		if intent.TargetName != OutboundTargetNameMulticaExternalPR {
			return db.OutboundDelivery{}, false, fmt.Errorf("Multica terminal delivery requires its typed target")
		}
		delivery, err := multicaprojection.DecodeExternalPRTerminalDelivery([]byte(intent.PayloadJSON))
		if err != nil || delivery.IdempotencyKey != intent.IdempotencyKey {
			return db.OutboundDelivery{}, false, fmt.Errorf("Multica terminal delivery payload does not match its typed contract")
		}
	}
	maxAttempts := intent.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultOutboundMaxAttempts
	}
	delivery := db.OutboundDelivery{
		IdempotencyKey: intent.IdempotencyKey,
		EventType:      intent.EventType,
		TargetName:     intent.TargetName,
		TargetType:     intent.TargetType,
		SubjectType:    strings.TrimSpace(intent.SubjectType),
		SubjectKey:     strings.TrimSpace(intent.SubjectKey),
		PayloadVersion: strings.TrimSpace(intent.PayloadVersion),
		PayloadJSON:    db.LargeText(intent.PayloadJSON),
		Status:         OutboundDeliveryStatusPending,
		MaxAttempts:    maxAttempts,
		RepoFullName:   strings.TrimSpace(intent.RepoFullName),
		PRNumber:       intent.PRNumber,
		ExternalRepo:   strings.TrimSpace(intent.ExternalRepo),
		ExternalNumber: intent.ExternalNumber,
		MergeSHA:       strings.TrimSpace(intent.MergeSHA),
	}
	database := s.DBForCtx(ctx)
	res := database.Clauses(clause.OnConflict{DoNothing: true}).Create(&delivery)
	if res.Error != nil {
		return db.OutboundDelivery{}, false, res.Error
	}
	if res.RowsAffected == 1 {
		return delivery, true, nil
	}
	var existing db.OutboundDelivery
	if err := database.Where("idempotency_key = ?", intent.IdempotencyKey).First(&existing).Error; err != nil {
		return db.OutboundDelivery{}, false, err
	}
	if existing.EventType != intent.EventType || existing.TargetName != intent.TargetName || existing.TargetType != intent.TargetType || existing.PayloadVersion != intent.PayloadVersion || string(existing.PayloadJSON) != intent.PayloadJSON {
		return db.OutboundDelivery{}, false, fmt.Errorf("outbound idempotency key conflicts with event, target, type, version, or payload")
	}
	return existing, false, nil
}

func (s *Service) DeliverOutboundDeliveryNow(ctx context.Context, id uint) error {
	if s == nil {
		return fmt.Errorf("service is nil")
	}
	if s.OutboundDispatcher == nil {
		return fmt.Errorf("outbound dispatcher is not configured")
	}
	database := s.DBForCtx(ctx)
	var delivery db.OutboundDelivery
	if err := database.First(&delivery, id).Error; err != nil {
		return err
	}
	if delivery.Status == OutboundDeliveryStatusDelivered || delivery.Status == OutboundDeliveryStatusDeadLetter {
		return nil
	}
	databaseNow, err := databaseCurrentTime(database)
	if err != nil {
		return err
	}
	// A typed delivery that already exhausted its attempt budget must be
	// terminalized without another POST after a crash. The status/expiry
	// predicate is the CAS boundary; a late worker cannot dead-letter a newly
	// reclaimed lease.
	if delivery.TargetType == OutboundTargetTypeMulticaExternalPR && delivery.Status == OutboundDeliveryStatusDelivering && delivery.LeaseExpiresAt != nil && !delivery.LeaseExpiresAt.After(databaseNow) && delivery.AttemptCount >= delivery.MaxAttempts {
		res := database.Model(&db.OutboundDelivery{}).
			Where("id = ? AND status = ? AND target_type = ? AND lease_expires_at <= ? AND attempt_count >= ?", id, OutboundDeliveryStatusDelivering, OutboundTargetTypeMulticaExternalPR, databaseNow, delivery.MaxAttempts).
			Updates(map[string]any{"status": OutboundDeliveryStatusDeadLetter, "lease_owner": "", "lease_expires_at": nil, "next_attempt_at": nil, "last_error_code": "attempt_limit_after_crash", "last_error": db.LargeText("typed delivery attempt limit exhausted before completion")})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errOutboundDeliveryLeaseLost
		}
		return nil
	}
	leaseOwner, err := newOutboundLeaseOwner()
	if err != nil {
		return err
	}
	leaseExpires := databaseNow.Add(OutboundDeliveryLease)
	res := database.Model(&db.OutboundDelivery{}).
		Where("id = ? AND (status IN ? OR (status = ? AND target_type = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)))", id, []string{OutboundDeliveryStatusPending, OutboundDeliveryStatusRetryWait}, OutboundDeliveryStatusDelivering, OutboundTargetTypeMulticaExternalPR, databaseNow).
		Updates(map[string]any{
			"status":           OutboundDeliveryStatusDelivering,
			"last_attempt_at":  databaseNow,
			"attempt_count":    gorm.Expr("attempt_count + 1"),
			"lease_owner":      leaseOwner,
			"lease_expires_at": leaseExpires,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	if err := database.Where("id = ? AND lease_owner = ?", id, leaseOwner).First(&delivery).Error; err != nil {
		return err
	}
	result := s.OutboundDispatcher.DispatchOutbound(ctx, OutboundDispatchCall{
		Delivery: delivery,
		Target:   OutboundTarget{Name: delivery.TargetName, Type: delivery.TargetType},
	})
	updates := map[string]any{
		"last_http_status": result.HTTPStatus,
		"last_error_code":  strings.TrimSpace(result.Code),
		"last_error":       db.LargeText(strings.TrimSpace(result.Message)),
	}
	if result.Delivered {
		updates["status"] = OutboundDeliveryStatusDelivered
		updates["delivered_at"] = databaseNow
		updates["next_attempt_at"] = nil
		updates["lease_owner"] = ""
		updates["lease_expires_at"] = nil
		updates["last_error_code"] = ""
		updates["last_error"] = db.LargeText("")
		res := database.Model(&db.OutboundDelivery{}).Where("id = ? AND lease_owner = ?", id, leaseOwner).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errOutboundDeliveryLeaseLost
		}
		return nil
	}
	if result.Retryable && delivery.AttemptCount < delivery.MaxAttempts {
		next := databaseNow.Add(outboundBackoff(delivery.AttemptCount, result.Code))
		updates["status"] = OutboundDeliveryStatusRetryWait
		updates["next_attempt_at"] = &next
		updates["lease_owner"] = ""
		updates["lease_expires_at"] = nil
		res := database.Model(&db.OutboundDelivery{}).Where("id = ? AND lease_owner = ?", id, leaseOwner).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errOutboundDeliveryLeaseLost
		}
		return nil
	}
	updates["status"] = OutboundDeliveryStatusDeadLetter
	updates["next_attempt_at"] = nil
	updates["lease_owner"] = ""
	updates["lease_expires_at"] = nil
	res = database.Model(&db.OutboundDelivery{}).Where("id = ? AND lease_owner = ?", id, leaseOwner).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return errOutboundDeliveryLeaseLost
	}
	return nil
}

func newOutboundLeaseOwner() (string, error) {
	var token [16]byte
	if _, err := cryptorand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate outbound delivery lease owner: %w", err)
	}
	return "ags-outbound-" + hex.EncodeToString(token[:]), nil
}

func databaseCurrentTime(database *gorm.DB) (time.Time, error) {
	var current string
	if err := database.Raw("SELECT CURRENT_TIMESTAMP").Scan(&current).Error; err != nil {
		return time.Time{}, fmt.Errorf("read database clock: %w", err)
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, current); err == nil {
			// The instant comes from the database. Convert only for the
			// timestamp representation used by legacy GORM rows (notably the
			// SQLite test backend); no process clock participates in due/lease
			// decisions.
			return parsed.UTC().In(time.Local), nil
		}
	}
	return time.Time{}, fmt.Errorf("read database clock: invalid timestamp")
}

func (s *Service) ProcessDueOutboundDeliveries(ctx context.Context, limit int) (int, error) {
	if s == nil {
		return 0, fmt.Errorf("service is nil")
	}
	if s.OutboundDispatcher == nil {
		return 0, nil
	}
	if limit <= 0 {
		limit = 20
	}
	database := s.DBForCtx(ctx)
	databaseNow, err := databaseCurrentTime(database)
	if err != nil {
		return 0, err
	}
	var deliveries []db.OutboundDelivery
	if err := database.
		Where("status = ? OR (status = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)) OR (status = ? AND target_type = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?))", OutboundDeliveryStatusPending, OutboundDeliveryStatusRetryWait, databaseNow, OutboundDeliveryStatusDelivering, OutboundTargetTypeMulticaExternalPR, databaseNow).
		Order("created_at ASC, id ASC").
		Limit(limit).
		Find(&deliveries).Error; err != nil {
		return 0, err
	}
	processed := 0
	for _, delivery := range deliveries {
		if err := s.DeliverOutboundDeliveryNow(ctx, delivery.ID); err != nil {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

func (s *Service) OutboundWorkerHealth() OutboundWorkerHealth {
	if s == nil {
		return OutboundWorkerHealth{}
	}
	s.outboundWorkerHealthMu.RLock()
	defer s.outboundWorkerHealthMu.RUnlock()
	return s.outboundWorkerHealth
}

func (s *Service) recordOutboundWorkerPoll(at time.Time, err error) {
	if s == nil {
		return
	}
	s.outboundWorkerHealthMu.Lock()
	defer s.outboundWorkerHealthMu.Unlock()
	s.outboundWorkerHealth.LastPollAt = at
	if err != nil {
		s.outboundWorkerHealth.LastError = err.Error()
		return
	}
	s.outboundWorkerHealth.LastSuccessAt = at
	s.outboundWorkerHealth.LastError = ""
}

func (s *Service) recordOutboundWorkerError(err error) {
	if s == nil || err == nil {
		return
	}
	s.outboundWorkerHealthMu.Lock()
	if s.outboundWorkerHealth.LastError == "" {
		s.outboundWorkerHealth.LastError = err.Error()
	}
	s.outboundWorkerHealthMu.Unlock()
}

func (s *Service) RunOutboundDeliveryWorker(ctx context.Context, interval time.Duration, limit int) {
	if s == nil || s.OutboundDispatcher == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastGC := time.Time{}
	for {
		processed, err := s.ProcessDueOutboundDeliveries(ctx, limit)
		s.pollOutboundDeliveryHealth(processed, err)
		if lastGC.IsZero() || time.Since(lastGC) >= time.Hour {
			if _, gcErr := s.GCOutboundDeliveries(ctx, DefaultOutboundDeliveryGCRetention()); gcErr != nil {
				s.recordOutboundWorkerError(gcErr)
				slog.ErrorContext(ctx, "outbound delivery garbage collection failed", "error", gcErr)
			}
			lastGC = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) pollOutboundDeliveryHealth(processed int, err error) {
	at := time.Now().UTC()
	s.recordOutboundWorkerPoll(at, err)
	if err != nil {
		slog.Error("outbound delivery poll failed", "processed", processed, "error", err)
	}
}

func outboundBackoff(attempt int, code string) time.Duration {
	if strings.EqualFold(strings.TrimSpace(code), "feishu_11232") {
		durations := []time.Duration{2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour}
		idx := attempt - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(durations) {
			idx = len(durations) - 1
		}
		return durations[idx]
	}
	durations := []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour}
	idx := attempt - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(durations) {
		idx = len(durations) - 1
	}
	return durations[idx]
}

func outboundTextPayloadJSON(text string, manualReplay bool) string {
	payload := map[string]any{"text": text}
	if manualReplay {
		payload["manual_replay"] = true
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return `{"text":""}`
	}
	return string(data)
}
