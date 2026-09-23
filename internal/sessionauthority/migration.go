package sessionauthority

import (
	"sort"

	"github.com/ngaut/agent-git-service/internal/delegationpolicy"
)

// LegacyMigrationPlan is a secret-free inventory of legacy delegation inputs.
// It never produces executable bindings or native grants. Operators must make
// explicit issuer-instance, immutable-principal-ID, and resource-policy choices.
type LegacyMigrationPlan struct {
	SourceSchemaVersion int                        `json:"source_schema_version,omitempty"`
	Candidates          []LegacyMigrationCandidate `json:"candidates"`
	ClaimLimit          string                     `json:"claim_limit"`
}

type LegacyMigrationCandidate struct {
	SourceSchemaVersion        int      `json:"source_schema_version"`
	SourceID                   string   `json:"source_id"`
	Issuer                     string   `json:"issuer"`
	WorkspaceID                string   `json:"workspace_id,omitempty"`
	Subject                    string   `json:"subject,omitempty"`
	PrincipalLogin             string   `json:"principal_login"`
	Target                     string   `json:"target"`
	Repository                 string   `json:"repository"`
	Status                     string   `json:"status"`
	Executable                 bool     `json:"executable"`
	IgnoredAuthorizationFields []string `json:"ignored_authorization_fields"`
	Blockers                   []string `json:"blockers"`
}

const LegacyMigrationClaimLimit = "legacy delegation is inventory-only; it cannot authorize principal/session-v2 exchange"

func PlanLegacyDelegation(config delegationpolicy.Config) LegacyMigrationPlan {
	plan := LegacyMigrationPlan{SourceSchemaVersion: config.Version, ClaimLimit: LegacyMigrationClaimLimit}
	switch config.Version {
	case delegationpolicy.CurrentVersion:
		for _, mapping := range config.Mappings {
			plan.Candidates = append(plan.Candidates, LegacyMigrationCandidate{
				SourceSchemaVersion:        config.Version,
				SourceID:                   mapping.ID,
				Issuer:                     mapping.Issuer,
				WorkspaceID:                mapping.WorkspaceID,
				Subject:                    mapping.AgentID,
				PrincipalLogin:             mapping.Principal,
				Target:                     mapping.Target,
				Repository:                 mapping.Repository,
				Status:                     mapping.Status,
				Executable:                 false,
				IgnoredAuthorizationFields: []string{"max_capabilities", "max_session_ttl", "role", "task_ids"},
				Blockers:                   []string{"trusted_issuer_instance_id_required", "immutable_principal_id_required", "native_grant_review_required", "resource_policy_revision_required"},
			})
		}
	case delegationpolicy.LegacyVersion:
		for _, policy := range config.Policies {
			repositories := make([]string, 0, len(policy.Repositories))
			for repository := range policy.Repositories {
				repositories = append(repositories, repository)
			}
			sort.Strings(repositories)
			for _, repository := range repositories {
				plan.Candidates = append(plan.Candidates, LegacyMigrationCandidate{
					SourceSchemaVersion:        config.Version,
					SourceID:                   policy.ID,
					Issuer:                     policy.Issuer,
					WorkspaceID:                policy.WorkspaceID,
					PrincipalLogin:             policy.Principal,
					Target:                     policy.Target,
					Repository:                 repository,
					Status:                     policy.Status,
					Executable:                 false,
					IgnoredAuthorizationFields: []string{"max_capabilities", "max_session_ttl", "workspace_selector"},
					Blockers:                   []string{"immutable_subject_required", "trusted_issuer_instance_id_required", "immutable_principal_id_required", "native_grant_review_required", "resource_policy_revision_required"},
				})
			}
		}
	}
	sort.Slice(plan.Candidates, func(i, j int) bool {
		if plan.Candidates[i].SourceID != plan.Candidates[j].SourceID {
			return plan.Candidates[i].SourceID < plan.Candidates[j].SourceID
		}
		return plan.Candidates[i].Repository < plan.Candidates[j].Repository
	})
	return plan
}
