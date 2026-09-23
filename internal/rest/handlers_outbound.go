package rest

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

type outboundDeliveryResponse struct {
	ID             uint       `json:"id"`
	IdempotencyKey string     `json:"idempotency_key"`
	EventType      string     `json:"event_type"`
	TargetName     string     `json:"target_name"`
	TargetType     string     `json:"target_type"`
	SubjectType    string     `json:"subject_type"`
	SubjectKey     string     `json:"subject_key"`
	PayloadVersion string     `json:"payload_version"`
	Status         string     `json:"status"`
	AttemptCount   int        `json:"attempt_count"`
	MaxAttempts    int        `json:"max_attempts"`
	LeaseOwner     string     `json:"lease_owner,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	NextAttemptAt  *time.Time `json:"next_attempt_at,omitempty"`
	LastAttemptAt  *time.Time `json:"last_attempt_at,omitempty"`
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
	LastHTTPStatus int        `json:"last_http_status,omitempty"`
	LastErrorCode  string     `json:"last_error_code,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	RepoFullName   string     `json:"repo_full_name,omitempty"`
	PRNumber       int        `json:"pr_number,omitempty"`
	ExternalRepo   string     `json:"external_repo,omitempty"`
	ExternalNumber int        `json:"external_number,omitempty"`
	MergeSHA       string     `json:"merge_sha,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func outboundDeliveryJSON(row db.OutboundDelivery) outboundDeliveryResponse {
	return outboundDeliveryResponse{
		ID:             row.ID,
		IdempotencyKey: row.IdempotencyKey,
		EventType:      row.EventType,
		TargetName:     row.TargetName,
		TargetType:     row.TargetType,
		SubjectType:    row.SubjectType,
		SubjectKey:     row.SubjectKey,
		PayloadVersion: row.PayloadVersion,
		Status:         row.Status,
		AttemptCount:   row.AttemptCount,
		MaxAttempts:    row.MaxAttempts,
		LeaseOwner:     row.LeaseOwner,
		LeaseExpiresAt: row.LeaseExpiresAt,
		NextAttemptAt:  row.NextAttemptAt,
		LastAttemptAt:  row.LastAttemptAt,
		DeliveredAt:    row.DeliveredAt,
		LastHTTPStatus: row.LastHTTPStatus,
		LastErrorCode:  row.LastErrorCode,
		LastError:      string(row.LastError),
		RepoFullName:   row.RepoFullName,
		PRNumber:       row.PRNumber,
		ExternalRepo:   row.ExternalRepo,
		ExternalNumber: row.ExternalNumber,
		MergeSHA:       row.MergeSHA,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

func outboundDeliveriesJSON(rows []db.OutboundDelivery) []outboundDeliveryResponse {
	out := make([]outboundDeliveryResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, outboundDeliveryJSON(row))
	}
	return out
}

// ListOutboundDeliveries handles GET /api/v3/outbound/deliveries.
func (d *Deps) ListOutboundDeliveries(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := d.Svc.ListOutboundDeliveries(r.Context(), service.OutboundDeliveryFilter{
		EventType:    r.URL.Query().Get("event"),
		Status:       r.URL.Query().Get("status"),
		RepoFullName: r.URL.Query().Get("repo"),
		Limit:        limit,
	})
	if err != nil {
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, outboundDeliveriesJSON(rows))
}

// RetryOutboundDelivery handles POST /api/v3/outbound/deliveries/{delivery_id}/retry.
func (d *Deps) RetryOutboundDelivery(w http.ResponseWriter, r *http.Request) {
	id, ok := mustUintParam(w, r, "delivery_id")
	if !ok {
		return
	}
	row, err := d.Svc.RetryOutboundDelivery(r.Context(), id)
	if err != nil {
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, outboundDeliveryJSON(row))
}

// ReplayPullRequestMergeOutbound handles POST /api/v3/repos/{owner}/{repo}/pulls/{pull_number}/outbound/replay-merge.
func (d *Deps) ReplayPullRequestMergeOutbound(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "true" || r.URL.Query().Get("force") == "1"
	pullNumber, ok := mustIntParam(w, r, "pull_number")
	if !ok {
		return
	}
	rows, err := d.Svc.ReplayPullRequestMergedOutbound(r.Context(), repoFullName(r), pullNumber, force)
	if err != nil {
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, outboundDeliveriesJSON(rows))
}
