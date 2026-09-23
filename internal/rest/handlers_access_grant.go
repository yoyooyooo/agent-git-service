package rest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ngaut/agent-git-service/internal/executioncontext"
	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

const accessGrantRequestBodyLimit int64 = 32 << 10

type accessGrantExecutionContextRequest struct {
	SourceInstanceID    string                   `json:"source_instance_id,omitempty"`
	RuntimeEndpointHint string                   `json:"runtime_endpoint_hint,omitempty"`
	Locator             executioncontext.Locator `json:"locator"`
	SourceToken         string                   `json:"source_token"`
}

type accessGrantIssueRequest struct {
	SourceInstanceID    string                   `json:"source_instance_id,omitempty"`
	RuntimeEndpointHint string                   `json:"runtime_endpoint_hint,omitempty"`
	Locator             executioncontext.Locator `json:"locator"`
	SourceToken         string                   `json:"source_token"`
	Repository          string                   `json:"repository"`
	AgentID             string                   `json:"agent_id,omitempty"`
	PolicyClass         string                   `json:"policy_class,omitempty"`
	AccessRole          string                   `json:"access_role,omitempty"`
	Operations          []string                 `json:"operations,omitempty"`
}

// IssueAccessGrant bootstraps one task/repository-scoped AGS authority grant
// from a fresh source observation. No durable AGS profile or provider token is
// accepted by this route.
func (d *Deps) IssueAccessGrant(w http.ResponseWriter, r *http.Request) {
	var body accessGrantIssueRequest
	if err := decodeAccessGrantJSON(w, r, &body, false); err != nil {
		accessGrantError(w, err)
		return
	}
	defer func() { body.SourceToken = "" }()
	if strings.TrimSpace(body.SourceToken) == "" {
		accessGrantError(w, service.ErrValidation)
		return
	}
	result, err := d.Svc.IssueAccessGrant(r.Context(), service.AccessGrantIssueInput{
		ExecutionContext: service.ExecutionContextIntakeInput{
			SourceInstanceID: body.SourceInstanceID, RuntimeEndpointHint: body.RuntimeEndpointHint,
			Locator: body.Locator, SourceToken: body.SourceToken,
		},
		Repository: body.Repository, AgentSelector: body.AgentID,
		PolicyClass: body.PolicyClass, AccessRole: body.AccessRole, AdditionalOperations: body.Operations,
	})
	body.SourceToken = ""
	if err != nil {
		accessGrantError(w, err)
		return
	}
	accessGrantJSON(w, http.StatusCreated, result)
}

func (d *Deps) RenewAccessGrant(w http.ResponseWriter, r *http.Request) {
	token, err := accessGrantBearer(r)
	if err != nil {
		accessGrantError(w, err)
		return
	}
	defer func() { token = "" }()
	var body accessGrantExecutionContextRequest
	if err := decodeAccessGrantJSON(w, r, &body, false); err != nil {
		accessGrantError(w, err)
		return
	}
	defer func() { body.SourceToken = "" }()
	if strings.TrimSpace(body.SourceToken) == "" {
		accessGrantError(w, service.ErrValidation)
		return
	}
	result, err := d.Svc.RenewAccessGrant(r.Context(), token, service.AccessGrantRenewInput{
		ExecutionContext: service.ExecutionContextIntakeInput{
			SourceInstanceID: body.SourceInstanceID, RuntimeEndpointHint: body.RuntimeEndpointHint,
			Locator: body.Locator, SourceToken: body.SourceToken,
		},
	})
	body.SourceToken = ""
	if err != nil {
		accessGrantError(w, err)
		return
	}
	accessGrantJSON(w, http.StatusCreated, result)
}

func (d *Deps) GetCurrentAccessGrant(w http.ResponseWriter, r *http.Request) {
	token, err := accessGrantBearer(r)
	if err != nil {
		accessGrantError(w, err)
		return
	}
	defer func() { token = "" }()
	result, err := d.Svc.GetAccessGrant(r.Context(), token)
	if err != nil {
		accessGrantError(w, err)
		return
	}
	accessGrantJSON(w, http.StatusOK, result)
}

func (d *Deps) RevokeAccessGrant(w http.ResponseWriter, r *http.Request) {
	token, err := accessGrantBearer(r)
	if err != nil {
		accessGrantError(w, err)
		return
	}
	defer func() { token = "" }()
	var body struct {
		Reason string `json:"reason,omitempty"`
	}
	if err := decodeAccessGrantJSON(w, r, &body, true); err != nil {
		accessGrantError(w, err)
		return
	}
	result, err := d.Svc.RevokeAccessGrant(r.Context(), token, body.Reason)
	if err != nil {
		accessGrantError(w, err)
		return
	}
	accessGrantJSON(w, http.StatusOK, result)
}

func (d *Deps) AuthorizeAccessGrantOperation(w http.ResponseWriter, r *http.Request) {
	token, err := accessGrantBearer(r)
	if err != nil {
		accessGrantError(w, err)
		return
	}
	defer func() { token = "" }()
	var body struct {
		Operation   string         `json:"operation"`
		Constraints map[string]any `json:"constraints"`
	}
	if err := decodeAccessGrantJSON(w, r, &body, false); err != nil {
		accessGrantError(w, err)
		return
	}
	if body.Constraints == nil {
		body.Constraints = map[string]any{}
	}
	result, err := d.Svc.AuthorizeAccessGrantOperation(r.Context(), token, service.AccessGrantOperationInput{
		Operation: body.Operation, Constraints: body.Constraints,
	})
	if err != nil {
		accessGrantError(w, err)
		return
	}
	accessGrantJSON(w, http.StatusOK, result)
}

func (d *Deps) IssueAccessGrantTransportSession(w http.ResponseWriter, r *http.Request) {
	token, err := accessGrantBearer(r)
	if err != nil {
		accessGrantError(w, err)
		return
	}
	defer func() { token = "" }()
	var body struct {
		Operation   string         `json:"operation"`
		Constraints map[string]any `json:"constraints"`
	}
	if err := decodeAccessGrantJSON(w, r, &body, false); err != nil {
		accessGrantError(w, err)
		return
	}
	if body.Constraints == nil {
		body.Constraints = map[string]any{}
	}
	result, err := d.Svc.IssueAccessGrantTransportSession(r.Context(), token, service.AccessGrantOperationInput{
		Operation: body.Operation, Constraints: body.Constraints,
	})
	if err != nil {
		accessGrantError(w, err)
		return
	}
	accessGrantJSON(w, http.StatusCreated, result)
}

func (d *Deps) RequestAccessGrantPRMerge(w http.ResponseWriter, r *http.Request) {
	token, err := accessGrantBearer(r)
	if err != nil {
		accessGrantError(w, err)
		return
	}
	defer func() { token = "" }()
	var body struct {
		InvocationID     string `json:"invocation_id"`
		AGSPRNumber      int    `json:"ags_pr_number"`
		ProviderPRNumber int    `json:"provider_pr_number"`
		ExpectedHeadSHA  string `json:"expected_head_sha"`
		MergeMethod      string `json:"merge_method"`
	}
	if err := decodeAccessGrantJSON(w, r, &body, false); err != nil {
		accessGrantError(w, err)
		return
	}
	result, err := d.Svc.ExecuteAccessGrantPRMerge(r.Context(), token, service.AccessGrantPRMergeInput{
		InvocationID: body.InvocationID, AGSPRNumber: body.AGSPRNumber, ProviderPRNumber: body.ProviderPRNumber,
		ExpectedHeadSHA: body.ExpectedHeadSHA, MergeMethod: body.MergeMethod,
	})
	if err != nil {
		accessGrantError(w, err)
		return
	}
	accessGrantJSON(w, http.StatusOK, result)
}

func (d *Deps) GetAccessGrantInvocation(w http.ResponseWriter, r *http.Request) {
	token, err := accessGrantBearer(r)
	if err != nil {
		accessGrantError(w, err)
		return
	}
	defer func() { token = "" }()
	result, err := d.Svc.GetAccessGrantInvocation(r.Context(), token, pathParam(r, "invocation_id"))
	if err != nil {
		accessGrantError(w, err)
		return
	}
	accessGrantJSON(w, http.StatusOK, result)
}

func decodeAccessGrantJSON(w http.ResponseWriter, r *http.Request, target any, allowEmpty bool) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, accessGrantRequestBodyLimit))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return nil
		}
		return service.ErrValidation
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return service.ErrValidation
	}
	return nil
}

func accessGrantBearer(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", service.ErrAccessGrantCredentialInvalid
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", service.ErrAccessGrantCredentialInvalid
	}
	return parts[1], nil
}

func accessGrantJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Cache-Control", "no-store")
	respond.JSON(w, status, body)
}

func accessGrantError(w http.ResponseWriter, err error) {
	status, code, message := http.StatusInternalServerError, "internal_error", "Internal Server Error"
	switch {
	case errors.Is(err, service.ErrValidation), errors.Is(err, executioncontext.ErrInvalidLocator):
		status, code, message = http.StatusUnprocessableEntity, "invalid_request", "Access grant request is invalid"
	case errors.Is(err, service.ErrAccessGrantCredentialInvalid):
		status, code, message = http.StatusUnauthorized, "invalid_grant", "Access grant credential is invalid"
	case errors.Is(err, executioncontext.ErrSourceCredentialRejected):
		status, code, message = http.StatusUnauthorized, "source_credential_rejected", "Execution context source credential was rejected"
	case errors.Is(err, service.ErrAccessGrantDenied):
		status, code, message = http.StatusForbidden, "grant_denied", "Access grant authority denied the request"
	case errors.Is(err, service.ErrAccessGrantConflict), errors.Is(err, executioncontext.ErrConnectorAmbiguous):
		status, code, message = http.StatusConflict, "grant_conflict", "Access grant authority or exact operation facts changed"
	case errors.Is(err, executioncontext.ErrConnectorNotFound):
		status, code, message = http.StatusUnprocessableEntity, "connector_not_found", "No registered execution context connector matches the request"
	case errors.Is(err, executioncontext.ErrSourceRedirectRejected), errors.Is(err, executioncontext.ErrSourceContextInvalid):
		status, code, message = http.StatusBadGateway, "source_context_rejected", "Execution context source response was rejected"
	case errors.Is(err, executioncontext.ErrSourceUnavailable), errors.Is(err, executioncontext.ErrDisabled),
		errors.Is(err, service.ErrExecutionContextIntakeUnavailable), errors.Is(err, service.ErrAccessGrantUnavailable):
		status, code, message = http.StatusServiceUnavailable, "grant_unavailable", "Access grant authority is unavailable"
	}
	w.Header().Set("Cache-Control", "no-store")
	respond.JSON(w, status, map[string]any{"error": code, "message": message})
}
