package service

import (
	"context"
	"errors"
	"sort"

	"gorm.io/gorm"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/delegationpolicy"
)

const DelegationPolicyClaimLimit = "legacy delegation inventory verified; it cannot authorize principal/session-v2 exchange"

type DelegationPolicyQuery struct {
	Issuer      string `json:"issuer"`
	WorkspaceID string `json:"workspace_id"`
	AgentID     string `json:"agent_id,omitempty"`
	TaskID      string `json:"task_id,omitempty"`
	Target      string `json:"target"`
	Repository  string `json:"repository"`
}

type DelegationPolicyCheck struct {
	ID      string         `json:"id"`
	Status  string         `json:"status"`
	Summary string         `json:"summary"`
	Details map[string]any `json:"details,omitempty"`
}

type DelegationPrincipalReport struct {
	ID                  uint   `json:"id,omitempty"`
	Login               string `json:"login"`
	UserKind            string `json:"user_kind,omitempty"`
	Status              string `json:"status,omitempty"`
	Exists              bool   `json:"exists"`
	EffectivePermission string `json:"effective_permission"`
}

type DelegationPolicyReport struct {
	OK                 bool                      `json:"ok"`
	SchemaVersion      int                       `json:"schema_version,omitempty"`
	PolicyID           string                    `json:"policy_id,omitempty"`
	Issuer             string                    `json:"issuer"`
	WorkspaceID        string                    `json:"workspace_id"`
	AgentID            string                    `json:"agent_id,omitempty"`
	Role               string                    `json:"role,omitempty"`
	TaskIDs            []string                  `json:"task_ids,omitempty"`
	Target             string                    `json:"target"`
	Repository         string                    `json:"repository"`
	Principal          DelegationPrincipalReport `json:"principal"`
	MaxCapabilities    []string                  `json:"max_capabilities"`
	RequiredPermission string                    `json:"required_permission"`
	MaxSessionTTL      string                    `json:"max_session_ttl,omitempty"`
	AllowMerge         bool                      `json:"allow_merge"`
	Status             string                    `json:"policy_status,omitempty"`
	PolicyVersion      string                    `json:"policy_version,omitempty"`
	Checks             []DelegationPolicyCheck   `json:"checks"`
	ClaimLimit         string                    `json:"claim_limit"`
}

func check(id string, ok bool, passSummary, failSummary string, details map[string]any) DelegationPolicyCheck {
	status, summary := "fail", failSummary
	if ok {
		status, summary = "pass", passSummary
	}
	return DelegationPolicyCheck{ID: id, Status: status, Summary: summary, Details: details}
}

// ExplainDelegationPolicy resolves only immutable workload selector facts and
// the exact repo. It never accepts a principal/profile/login/display/role
// override.
func (s *Service) ExplainDelegationPolicy(ctx context.Context, query DelegationPolicyQuery) (DelegationPolicyReport, error) {
	report := DelegationPolicyReport{
		Issuer: query.Issuer, WorkspaceID: query.WorkspaceID, AgentID: query.AgentID, Target: query.Target, Repository: query.Repository,
		Principal: DelegationPrincipalReport{EffectivePermission: "none"},
		Checks:    []DelegationPolicyCheck{}, ClaimLimit: DelegationPolicyClaimLimit,
	}
	resolved, err := s.DelegationPolicies.Resolve(delegationpolicy.Selector{
		Issuer: query.Issuer, WorkspaceID: query.WorkspaceID, AgentID: query.AgentID, TaskID: query.TaskID,
		Target: query.Target, Repository: query.Repository,
	})
	if err != nil {
		if errors.Is(err, delegationpolicy.ErrPolicyNotFound) || errors.Is(err, delegationpolicy.ErrRepositoryNotAllowed) {
			report.Checks = append(report.Checks, check("policy_match", false, "delegation policy matched", "no delegation policy matches the issuer/workspace/target/repository", nil))
			return report, nil
		}
		return report, err
	}

	report.SchemaVersion = resolved.SchemaVersion
	report.PolicyID = resolved.ID()
	report.Issuer = resolved.Issuer()
	report.WorkspaceID = resolved.WorkspaceID()
	report.AgentID = resolved.AgentID()
	report.Role = resolved.Role()
	report.TaskIDs = resolved.TaskIDs()
	report.Target = resolved.Target()
	report.Repository = resolved.Repository
	report.Principal.Login = resolved.Principal()
	report.MaxCapabilities = resolved.MaxCapabilities()
	report.RequiredPermission = requiredPermissionForCapabilities(report.MaxCapabilities).String()
	report.MaxSessionTTL = resolved.MaxSessionTTL()
	report.AllowMerge = resolved.AllowMerge()
	report.Status = resolved.Status()
	report.PolicyVersion = resolved.PolicyVersion()
	report.Checks = append(report.Checks,
		check("policy_match", true, "delegation policy matched", "delegation policy did not match", map[string]any{"policy_id": resolved.ID(), "schema_version": resolved.SchemaVersion}),
		check("policy_active", resolved.Status() == "active", "delegation policy is active", "delegation policy is disabled", map[string]any{"status": resolved.Status()}),
	)

	database := s.DBForCtx(ctx)
	var principal db.User
	principalErr := database.Where("login = ? AND type = ?", resolved.Principal(), db.TypeUser).First(&principal).Error
	if principalErr != nil && !errors.Is(principalErr, gorm.ErrRecordNotFound) {
		return report, principalErr
	}
	principalExists := principalErr == nil
	if principalExists {
		report.Principal.ID = principal.ID
		report.Principal.UserKind = principal.UserKind
		report.Principal.Status = principal.Status
		report.Principal.Exists = true
	}
	report.Checks = append(report.Checks,
		check("principal_exists", principalExists, "configured principal exists", "configured principal does not exist", map[string]any{"login": resolved.Principal()}),
		check("principal_active", principalExists && principal.Status == db.UserStatusActive, "configured principal is active", "configured principal is not active", map[string]any{"status": principal.Status}),
	)

	var repository db.Repository
	repoErr := database.Where("full_name = ?", resolved.Repository).First(&repository).Error
	if repoErr != nil && !errors.Is(repoErr, gorm.ErrRecordNotFound) {
		return report, repoErr
	}
	repositoryExists := repoErr == nil && !repository.Disabled
	report.Checks = append(report.Checks, check("repository_exists", repositoryExists, "configured repository exists", "configured repository does not exist or is disabled", map[string]any{"repository": resolved.Repository}))

	required := requiredPermissionForCapabilities(report.MaxCapabilities)
	permission := RepoPermissionNone
	if principalExists && repositoryExists {
		permission, err = s.HasRepoAccess(ctx, repository.ID, principal.ID)
		if err != nil {
			return report, err
		}
	}
	report.Principal.EffectivePermission = permission.String()
	permissionOK := principalExists && repositoryExists && permission.AtLeast(required)
	report.Checks = append(report.Checks, check("principal_repo_permission", permissionOK, "current principal permission covers the historical migration record", "current principal permission is below the historical migration record", map[string]any{"required": required.String(), "effective": permission.String()}))

	report.OK = true
	for _, item := range report.Checks {
		if item.Status != "pass" {
			report.OK = false
			break
		}
	}
	return report, nil
}

// VerifyDelegationPolicies returns one inventory report per legacy repository
// or Agent mapping. Historical Task selectors are read only to explain the
// source snapshot; they never authorize a new Session.
func (s *Service) VerifyDelegationPolicies(ctx context.Context) ([]DelegationPolicyReport, error) {
	reports := make([]DelegationPolicyReport, 0)
	for _, policy := range s.DelegationPolicies.Policies() {
		repositories := make([]string, 0, len(policy.Repositories))
		for repository := range policy.Repositories {
			repositories = append(repositories, repository)
		}
		sort.Strings(repositories)
		for _, repository := range repositories {
			report, err := s.ExplainDelegationPolicy(ctx, DelegationPolicyQuery{
				Issuer: policy.Issuer, WorkspaceID: policy.WorkspaceID, Target: policy.Target, Repository: repository,
			})
			if err != nil {
				return nil, err
			}
			reports = append(reports, report)
		}
	}
	for _, mapping := range s.DelegationPolicies.Mappings() {
		taskID := ""
		if len(mapping.TaskIDs) != 0 {
			taskID = mapping.TaskIDs[0]
		}
		report, err := s.ExplainDelegationPolicy(ctx, DelegationPolicyQuery{
			Issuer: mapping.Issuer, WorkspaceID: mapping.WorkspaceID, AgentID: mapping.AgentID, TaskID: taskID,
			Target: mapping.Target, Repository: mapping.Repository,
		})
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, nil
}

func requiredPermissionForCapabilities(capabilities []string) RepoPermission {
	for _, capability := range capabilities {
		switch capability {
		case "repo:write", "pr:create", "pr:update", "pr:comment", "pr:review":
			return RepoPermissionWrite
		}
	}
	return RepoPermissionRead
}
