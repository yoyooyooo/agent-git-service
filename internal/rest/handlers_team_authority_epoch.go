package rest

import (
	"net/http"
	"strings"

	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

// AdvanceTeamAuthorityEpochFloor advances the target-local canonical team
// authority revoke floor. It is intentionally separate from config loading so
// an operator can invalidate older sessions without changing issuer claims.
func (d *Deps) AdvanceTeamAuthorityEpochFloor(w http.ResponseWriter, r *http.Request) {
	viewer, ok := service.UserFromContext(r.Context())
	if !ok {
		respond.Unauthorized(w, "Bad credentials")
		return
	}
	if !viewer.SiteAdmin {
		respond.Forbidden(w, "site admin access required")
		return
	}
	var body struct {
		IssuerInstanceID string `json:"issuer_instance_id"`
		TeamIdentityID   string `json:"team_identity_id"`
		PolicyClass      string `json:"policy_class"`
		MembershipEpoch  int64  `json:"membership_epoch"`
	}
	if err := decodeBodyStrict(r, &body); err != nil {
		respond.ValidationFailed(w, "invalid team authority epoch request")
		return
	}
	if err := d.Svc.AdvanceTeamAuthorityEpochFloor(
		r.Context(), strings.TrimSpace(body.IssuerInstanceID), strings.TrimSpace(body.TeamIdentityID),
		strings.TrimSpace(body.PolicyClass), body.MembershipEpoch,
	); err != nil {
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]any{"status": "advanced"})
}
