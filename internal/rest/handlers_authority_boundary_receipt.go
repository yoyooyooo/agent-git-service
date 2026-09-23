package rest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

// CaptureLegacyAuthorityBoundary accepts only an expected epoch. The server
// owns every payload field and digest.
func (d *Deps) CaptureLegacyAuthorityBoundary(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExpectedAuthorityEpoch string `json:"expected_authority_epoch"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.ExpectedAuthorityEpoch) == "" {
		respond.ValidationFailed(w, "expected_authority_epoch is required and no other fields are accepted")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		respond.ValidationFailed(w, "expected_authority_epoch is required and no other fields are accepted")
		return
	}
	receipt, err := d.Svc.CaptureLegacyAuthorityBoundary(r.Context(), body.ExpectedAuthorityEpoch)
	if err != nil {
		respondAuthorityBoundaryReceiptError(w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, receipt)
}

func (d *Deps) GetAuthorityBoundaryReceipt(w http.ResponseWriter, r *http.Request) {
	receipt, err := d.Svc.GetAuthorityBoundaryReceipt(r.Context(), pathParam(r, "id"))
	if err != nil {
		respondAuthorityBoundaryReceiptError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, receipt)
}

func (d *Deps) GetCurrentDelegatedEffectBoundaryReceipt(w http.ResponseWriter, r *http.Request) {
	receipt, err := d.Svc.GetCurrentDelegatedEffectBoundaryReceipt(r.Context(), pathParam(r, "id"))
	if err != nil {
		respondAuthorityBoundaryReceiptError(w, err)
		return
	}
	respond.JSON(w, http.StatusOK, receipt)
}

func respondAuthorityBoundaryReceiptError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrAuthorityBoundaryReceiptDenied):
		respond.Error(w, http.StatusForbidden, "Authority boundary receipt access denied")
	case errors.Is(err, service.ErrAuthorityBoundaryReceiptEpoch), errors.Is(err, service.ErrAuthorityBoundaryReceiptSource):
		respond.Error(w, http.StatusConflict, "Authority boundary receipt precondition failed")
	case errors.Is(err, service.ErrAuthorityBoundaryReceiptCorrupt), errors.Is(err, service.ErrAuthorityBoundaryReceiptSecret):
		respond.Error(w, http.StatusInternalServerError, "Authority boundary receipt integrity check failed")
	case errors.Is(err, service.ErrNotFound):
		respond.NotFound(w)
	default:
		respond.Error(w, http.StatusInternalServerError, "Authority boundary receipt operation failed")
	}
}
