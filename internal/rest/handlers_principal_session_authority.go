package rest

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

var principalSessionAuthorityFields = map[string]struct{}{
	"issuer": {}, "issuer_instance_id": {}, "key_id": {}, "subject": {}, "workspace_id": {}, "team_identity_id": {}, "policy_class": {}, "membership_epoch": {}, "target": {},
	"service": {}, "repository": {}, "operation": {},
}

// GetPrincipalSessionAuthority verifies one exact principal binding, native
// grant, resource policy, and operation. Provenance selectors are rejected.
func (d *Deps) GetPrincipalSessionAuthority(w http.ResponseWriter, r *http.Request) {
	viewer, ok := service.UserFromContext(r.Context())
	if !ok {
		respond.Unauthorized(w, "Bad credentials")
		return
	}
	if !viewer.SiteAdmin {
		respond.Forbidden(w, "site admin access required")
		return
	}
	values := r.URL.Query()
	for field, supplied := range values {
		if _, ok := principalSessionAuthorityFields[field]; !ok {
			respond.ValidationFailed(w, "provenance and identity override parameters are not allowed")
			return
		}
		for _, value := range supplied {
			if sessionauthority.IsSecretShapedValue(value) {
				respond.ValidationFailed(w, "authority query contains invalid value")
				return
			}
		}
	}
	query := service.PrincipalSessionAuthorityQuery{
		Issuer: strings.TrimSpace(values.Get("issuer")), IssuerInstanceID: strings.TrimSpace(values.Get("issuer_instance_id")),
		AssertionKeyID: strings.TrimSpace(values.Get("key_id")), Subject: strings.TrimSpace(values.Get("subject")), WorkspaceID: strings.TrimSpace(values.Get("workspace_id")), TeamIdentityID: strings.TrimSpace(values.Get("team_identity_id")), PolicyClass: strings.TrimSpace(values.Get("policy_class")), Target: strings.TrimSpace(values.Get("target")),
		Service: strings.TrimSpace(values.Get("service")), Repository: strings.TrimSpace(values.Get("repository")),
		Operation: strings.TrimSpace(values.Get("operation")),
	}
	for _, value := range []string{query.Issuer, query.IssuerInstanceID, query.AssertionKeyID, query.Target, query.Service, query.Repository, query.Operation} {
		if value == "" {
			respond.ValidationFailed(w, "issuer, issuer_instance_id, key_id, target, service, repository, and operation are required")
			return
		}
	}
	if query.TeamIdentityID == "" && query.PolicyClass == "" {
		if query.Subject == "" {
			respond.ValidationFailed(w, "subject is required for legacy authority explain")
			return
		}
	} else if query.Subject != "" || query.WorkspaceID == "" || query.TeamIdentityID == "" || query.PolicyClass == "" {
		respond.ValidationFailed(w, "canonical authority explain requires workspace_id, team_identity_id, and policy_class without subject")
		return
	} else {
		epoch, err := strconv.ParseInt(strings.TrimSpace(values.Get("membership_epoch")), 10, 64)
		if err != nil || epoch <= 0 {
			respond.ValidationFailed(w, "canonical authority explain requires positive membership_epoch")
			return
		}
		query.MembershipEpoch = epoch
	}
	report, err := d.Svc.ExplainPrincipalSessionAuthority(r.Context(), query)
	if err != nil {
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	status := "fail"
	if report.OK {
		status = "pass"
	}
	respond.JSON(w, http.StatusOK, map[string]any{
		"status": status, "authority": report, "claim_limit": service.PrincipalSessionAuthorityClaimLimit,
	})
}
