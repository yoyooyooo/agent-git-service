package service

import (
	"context"
	"errors"
	"strings"

	"gorm.io/gorm"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

const PrincipalSessionAuthorityClaimLimit = "authority and current native grant verified; Access Grant issuance, transport derivation, and operation execution are not claimed"

type PrincipalSessionAuthorityQuery struct {
	Issuer           string `json:"issuer"`
	IssuerInstanceID string `json:"issuer_instance_id"`
	AssertionKeyID   string `json:"key_id"`
	Subject          string `json:"subject"`
	WorkspaceID      string `json:"workspace_id,omitempty"`
	TeamIdentityID   string `json:"team_identity_id,omitempty"`
	PolicyClass      string `json:"policy_class,omitempty"`
	MembershipEpoch  int64  `json:"membership_epoch,omitempty"`
	Target           string `json:"target"`
	Service          string `json:"service"`
	Repository       string `json:"repository"`
	Operation        string `json:"operation"`
}

type PrincipalSessionAuthorityReport struct {
	OK                     bool                      `json:"ok"`
	ContractRevision       string                    `json:"contract_revision"`
	IssuerInstanceID       string                    `json:"issuer_instance_id,omitempty"`
	TrustRevision          string                    `json:"trust_revision,omitempty"`
	AssertionKeyID         string                    `json:"key_id,omitempty"`
	Subject                string                    `json:"subject,omitempty"`
	WorkspaceID            string                    `json:"workspace_id,omitempty"`
	BindingID              string                    `json:"binding_id,omitempty"`
	BindingRevision        string                    `json:"binding_revision,omitempty"`
	TeamIdentityID         string                    `json:"team_identity_id,omitempty"`
	PolicyClass            string                    `json:"policy_class,omitempty"`
	MembershipEpoch        int64                     `json:"membership_epoch,omitempty"`
	Principal              DelegationPrincipalReport `json:"principal"`
	Target                 string                    `json:"target"`
	Service                string                    `json:"service"`
	Repository             string                    `json:"repository"`
	ResourcePolicyRevision string                    `json:"resource_policy_revision,omitempty"`
	Operation              string                    `json:"operation"`
	RequiredPermission     string                    `json:"required_permission,omitempty"`
	NativeGrant            string                    `json:"native_grant"`
	NativeGrantRevision    string                    `json:"native_grant_revision,omitempty"`
	Checks                 []DelegationPolicyCheck   `json:"checks"`
	ClaimLimit             string                    `json:"claim_limit"`
}

func (s *Service) ExplainPrincipalSessionAuthority(ctx context.Context, query PrincipalSessionAuthorityQuery) (PrincipalSessionAuthorityReport, error) {
	// A report is a read boundary, not an echo service. Fill request-derived
	// fields only after Resolve has matched them to immutable authority state.
	report := PrincipalSessionAuthorityReport{
		ContractRevision: sessionauthority.ContractRevision,
		ClaimLimit:       PrincipalSessionAuthorityClaimLimit,
	}
	if s.PrincipalSessions == nil {
		report.Checks = append(report.Checks, check("authority_configured", false, "principal session authority is configured", "principal session authority is not configured", nil))
		return report, nil
	}
	resolved, err := s.PrincipalSessions.Resolve(sessionauthority.Request{
		Issuer: query.Issuer, IssuerInstanceID: query.IssuerInstanceID, AssertionKeyID: query.AssertionKeyID, Subject: query.Subject,
		Target: query.Target, Service: query.Service, Repository: query.Repository, Operation: query.Operation,
		WorkspaceID: query.WorkspaceID, TeamIdentityID: query.TeamIdentityID, PolicyClass: query.PolicyClass, MembershipEpoch: query.MembershipEpoch,
	})
	if err != nil {
		report.Checks = append(report.Checks, check("authority_resolution", false, "principal binding and resource resolved", "principal binding or resource did not resolve", nil))
		return report, nil
	}
	report.IssuerInstanceID = resolved.Issuer.ID
	report.TrustRevision = resolved.Issuer.TrustRevision
	report.AssertionKeyID = strings.TrimSpace(query.AssertionKeyID)
	report.Subject = resolved.Binding.Subject
	report.BindingID = resolved.Binding.ID
	report.BindingRevision = resolved.Binding.BindingRevision
	report.WorkspaceID = resolved.TeamBinding.WorkspaceID
	report.TeamIdentityID = resolved.TeamBinding.TeamIdentityID
	report.PolicyClass = resolved.TeamBinding.PolicyClass
	report.MembershipEpoch = query.MembershipEpoch
	report.Target = resolved.Resource.Target
	report.Service = resolved.Resource.Service
	report.Repository = resolved.Resource.Repository
	report.ResourcePolicyRevision = resolved.Resource.PolicyRevision
	report.Operation = resolved.Operation.Name
	report.RequiredPermission = string(resolved.Operation.RequiredPermission)
	report.Checks = append(report.Checks, check("authority_resolution", true, "principal binding and resource resolved", "principal binding or resource did not resolve", nil))

	database := s.DBForCtx(ctx)
	var principal db.User
	if err := database.First(&principal, "id = ? AND type = ?", resolved.PrincipalID, db.TypeUser).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return report, err
		}
		report.Checks = append(report.Checks, check("principal_active", false, "bound principal is active", "bound principal is unavailable", nil))
		return report, nil
	}
	report.Principal = DelegationPrincipalReport{ID: principal.ID, Login: principal.Login, UserKind: principal.UserKind, Status: principal.Status, Exists: true}
	principalActive := isUserStatusActive(principal.Status)
	report.Checks = append(report.Checks, check("principal_active", principalActive, "bound principal is active", "bound principal is inactive", nil))

	var repository db.Repository
	if err := database.First(&repository, "full_name = ?", resolved.Resource.Repository).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return report, err
		}
		report.Checks = append(report.Checks, check("resource_available", false, "resource is available", "resource is unavailable", nil))
		return report, nil
	}
	resourceAvailable := !repository.Disabled
	report.Checks = append(report.Checks, check("resource_available", resourceAvailable, "resource is available", "resource is disabled", nil))
	permission, err := s.HasRepoAccess(ctx, repository.ID, principal.ID)
	if err != nil {
		return report, err
	}
	required := repoPermissionForSessionAuthority(resolved.Operation.RequiredPermission)
	report.NativeGrant = permission.Effective().String()
	report.Principal.EffectivePermission = report.NativeGrant
	report.NativeGrantRevision = nativeGrantRevision(principal.ID, repository.ID, permission)
	nativeAllowed := permission.AtLeast(required)
	report.Checks = append(report.Checks, check("native_grant", nativeAllowed, "principal native grant authorizes the operation", "principal native grant does not authorize the operation", map[string]any{
		"required_permission": required.String(), "effective_permission": permission.Effective().String(),
	}))
	report.OK = principalActive && resourceAvailable && nativeAllowed
	return report, nil
}
