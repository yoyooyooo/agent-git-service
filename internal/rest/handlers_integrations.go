package rest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

const (
	forgejoWebhookBodyLimit         = 2 << 20
	forgejoWebhookProcessingTimeout = 2 * time.Minute
)

// AuthorizeOperationContractRevision is the expected contract revision for
// durable operation authorization requests.
const AuthorizeOperationContractRevision = sessionauthority.ContractRevision

// AuthorizeOperation handles POST /api/v3/operations/authorize for durable
// profile authorization. It preserves the caller's normalized constraints and
// uses the current team-authority-v4 resource/operation and native-grant
// evaluator. principal-session-v2 is accepted only as explicit legacy input.
func (d *Deps) AuthorizeOperation(w http.ResponseWriter, r *http.Request) {
	viewer, ok := service.UserFromContext(r.Context())
	if !ok {
		respond.Unauthorized(w, "Bad credentials")
		return
	}
	var body struct {
		ContractRevision string `json:"contract_revision"`
		Resource         struct {
			Service    string `json:"service"`
			Repository string `json:"repository"`
		} `json:"resource"`
		Operation struct {
			Name        string         `json:"name"`
			Constraints map[string]any `json:"constraints"`
		} `json:"operation"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		respond.ValidationFailed(w, "invalid request body")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		respond.ValidationFailed(w, "invalid request body")
		return
	}
	if body.ContractRevision != AuthorizeOperationContractRevision && body.ContractRevision != sessionauthority.LegacyContractRevision {
		respond.ValidationFailed(w, "contract_revision must be "+AuthorizeOperationContractRevision)
		return
	}
	result, err := d.Svc.AuthorizeDurableOperation(r.Context(), viewer, body.Resource.Service, body.Resource.Repository, body.Operation.Name, body.Operation.Constraints)
	if err != nil {
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, result)
}

// ForgejoWebhook handles Forgejo webhooks for merge-authority callbacks.
// RequestForgejoActionRebase persists an exact AGS-owned pr.rebase intent and
// then asks the integration identity to deliver its label event.
func (d *Deps) RequestForgejoActionRebase(w http.ResponseWriter, r *http.Request) {
	viewer, ok := service.UserFromContext(r.Context())
	if !ok {
		respond.Unauthorized(w, "Bad credentials")
		return
	}
	var body struct {
		IdempotencyKey   string   `json:"idempotency_key"`
		ForgejoPRNumber  int      `json:"forgejo_pr_number"`
		ExpectedHeadSHA  string   `json:"expected_head_sha"`
		ExpectedBaseSHA  string   `json:"expected_base_sha"`
		ExpectedLabels   []string `json:"expected_labels"`
		PostLabels       []string `json:"post_labels"`
		ExpiresInSeconds int      `json:"expires_in_seconds"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		respond.ValidationFailed(w, "invalid action request")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		respond.ValidationFailed(w, "invalid action request")
		return
	}
	number, err := strconv.Atoi(pathParam(r, "number"))
	if err != nil || number <= 0 {
		respond.ValidationFailed(w, "invalid pull request number")
		return
	}
	result, err := d.Svc.RequestForgejoActionRebase(r.Context(), viewer, service.ForgejoActionIntentRequest{IdempotencyKey: body.IdempotencyKey, Repository: repoFullName(r), AGSPRNumber: number, ForgejoPRNumber: body.ForgejoPRNumber, ExpectedHeadSHA: body.ExpectedHeadSHA, ExpectedBaseSHA: body.ExpectedBaseSHA, ExpectedLabels: body.ExpectedLabels, PostLabels: body.PostLabels, ExpiresIn: time.Duration(body.ExpiresInSeconds) * time.Second})
	if err != nil {
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusAccepted, result)
}

func (d *Deps) GetForgejoActionRebase(w http.ResponseWriter, r *http.Request) {
	viewer, ok := service.UserFromContext(r.Context())
	if !ok {
		respond.Unauthorized(w, "Bad credentials")
		return
	}
	number, err := strconv.Atoi(pathParam(r, "number"))
	if err != nil || number <= 0 {
		respond.ValidationFailed(w, "invalid pull request number")
		return
	}
	result, err := d.Svc.GetForgejoActionIntent(r.Context(), viewer, repoFullName(r), number, pathParam(r, "intent_id"))
	if err != nil {
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, result)
}

func (d *Deps) ForgejoWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, forgejoWebhookBodyLimit+1))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid webhook body")
		return
	}
	if len(body) > forgejoWebhookBodyLimit {
		respond.Error(w, http.StatusRequestEntityTooLarge, "webhook body too large")
		return
	}
	webhookCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), forgejoWebhookProcessingTimeout)
	defer cancel()
	result, err := d.Svc.HandleForgejoWebhook(webhookCtx, r.Header, body)
	if err != nil {
		slog.WarnContext(r.Context(), "forgejo webhook failed", "error", err)
		respond.Error(w, http.StatusBadRequest, "invalid forgejo webhook")
		return
	}
	respond.JSON(w, http.StatusAccepted, map[string]any{
		"handled":                      result.Handled,
		"repo_full_name":               result.RepoFullName,
		"pr_number":                    result.PRNumber,
		"base_branch":                  result.BaseBranch,
		"synced_sha":                   result.SyncedSHA,
		"gitlab_backup_handled":        result.GitLabBackupHandled,
		"gitlab_target_branch":         result.GitLabTargetBranch,
		"gitlab_shadow_closed":         result.GitLabShadowClosed,
		"gitlab_shadow_mr_url":         result.GitLabShadowMRURL,
		"github_backup_handled":        result.GitHubBackupHandled,
		"github_target_branch":         result.GitHubTargetBranch,
		"github_shadow_closed":         result.GitHubShadowClosed,
		"github_shadow_pr_url":         result.GitHubShadowPRURL,
		"ags_pr_number":                result.AGSPRNumber,
		"branch_deleted":               result.BranchDeleted,
		"deleted_branch":               result.DeletedBranch,
		"ags_source_branch_deleted":    result.AGSSourceBranchDeleted,
		"gitlab_source_branch_deleted": result.GitLabSourceBranchDeleted,
		"github_source_branch_deleted": result.GitHubSourceBranchDeleted,
		"workflow_action":              result.WorkflowAction,
		"workflow_label":               result.WorkflowLabel,
		"workflow_status":              result.WorkflowStatus,
	})
}
