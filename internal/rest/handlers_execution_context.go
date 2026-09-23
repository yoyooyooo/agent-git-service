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

const executionContextIntakeBodyLimit int64 = 16 << 10

type executionContextIntakeRequest struct {
	SourceInstanceID    string                   `json:"source_instance_id,omitempty"`
	RuntimeEndpointHint string                   `json:"runtime_endpoint_hint,omitempty"`
	Locator             executioncontext.Locator `json:"locator"`
	SourceToken         string                   `json:"source_token"`
}

// IntakeExecutionContext is deliberately unauthenticated at the AGS token
// layer. The current task-scoped source token authenticates only the read from a
// fixed registered source; this endpoint does not issue AGS authority.
func (d *Deps) IntakeExecutionContext(w http.ResponseWriter, r *http.Request) {
	var body executionContextIntakeRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, executionContextIntakeBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		executionContextIntakeError(w, http.StatusUnprocessableEntity, "invalid_request", "Execution context intake request is invalid")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		executionContextIntakeError(w, http.StatusUnprocessableEntity, "invalid_request", "Execution context intake request is invalid")
		return
	}
	defer func() { body.SourceToken = "" }()
	if strings.TrimSpace(body.SourceToken) == "" {
		executionContextIntakeError(w, http.StatusUnprocessableEntity, "invalid_request", "Execution context intake request is invalid")
		return
	}
	result, err := d.Svc.IntakeExecutionContext(r.Context(), service.ExecutionContextIntakeInput{
		SourceInstanceID: body.SourceInstanceID, RuntimeEndpointHint: body.RuntimeEndpointHint,
		Locator: body.Locator, SourceToken: body.SourceToken,
	})
	body.SourceToken = ""
	if err != nil {
		switch {
		case errors.Is(err, executioncontext.ErrInvalidLocator):
			executionContextIntakeError(w, http.StatusUnprocessableEntity, "invalid_locator", "Execution context locator is invalid")
		case errors.Is(err, executioncontext.ErrConnectorNotFound):
			executionContextIntakeError(w, http.StatusUnprocessableEntity, "connector_not_found", "No registered execution context connector matches the request")
		case errors.Is(err, executioncontext.ErrConnectorAmbiguous):
			executionContextIntakeError(w, http.StatusConflict, "connector_ambiguous", "Execution context connector selection is ambiguous")
		case errors.Is(err, executioncontext.ErrSourceCredentialRejected):
			executionContextIntakeError(w, http.StatusUnauthorized, "source_credential_rejected", "Execution context source credential was rejected")
		case errors.Is(err, executioncontext.ErrSourceRedirectRejected):
			executionContextIntakeError(w, http.StatusBadGateway, "source_redirect_rejected", "Execution context source redirect was rejected")
		case errors.Is(err, executioncontext.ErrSourceContextInvalid):
			executionContextIntakeError(w, http.StatusBadGateway, "source_context_invalid", "Execution context source response was invalid")
		case errors.Is(err, executioncontext.ErrSourceUnavailable):
			executionContextIntakeError(w, http.StatusServiceUnavailable, "source_unavailable", "Execution context source is unavailable")
		case errors.Is(err, executioncontext.ErrDisabled), errors.Is(err, service.ErrExecutionContextIntakeUnavailable):
			executionContextIntakeError(w, http.StatusServiceUnavailable, "intake_unavailable", "Execution context intake is unavailable")
		default:
			executionContextIntakeError(w, http.StatusInternalServerError, "internal_error", "Internal Server Error")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond.JSON(w, http.StatusCreated, result)
}

func executionContextIntakeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Cache-Control", "no-store")
	respond.JSON(w, status, map[string]any{"error": code, "message": message})
}
