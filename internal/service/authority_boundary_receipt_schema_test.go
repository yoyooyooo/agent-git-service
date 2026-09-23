package service

import (
	"encoding/json"
	"testing"

	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

func TestAuthorityBoundaryReceiptPayloadClosedJSONFixtures(t *testing.T) {
	canonicalSet, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "urn:issuer", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "trust-v1"}},
		TeamBindings:   []sessionauthority.TeamBinding{{ID: "team", IssuerInstanceID: "issuer", WorkspaceID: "workspace", TeamIdentityID: "team-1", PolicyClass: "contributor", PrincipalID: 7, Status: "active", BindingRevision: "team-v1", EpochFloor: 1}},
		PolicyClasses:  []sessionauthority.PolicyClass{{ID: "contributor", Status: "active", PolicyRevision: "class-v1", Operations: []string{"repo.read"}}},
		Resources:      []sessionauthority.ResourcePolicy{{ID: "repo", Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Status: "active", MaxSessionTTL: "30m", PolicyRevision: "repo-v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defaultSet, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "urn:issuer", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "trust-v1"}},
		TeamBindings:   []sessionauthority.TeamBinding{{ID: "team", IssuerInstanceID: "issuer", WorkspaceID: "workspace", TeamIdentityID: "team-1", PolicyClass: "contributor", PrincipalID: 7, Status: "active", BindingRevision: "team-v1", EpochFloor: 1}},
		PolicyClasses:  []sessionauthority.PolicyClass{{ID: "contributor", Status: "active", PolicyRevision: "class-v1", Operations: []string{"repo.read"}}},
		ResourceDefaults: []sessionauthority.ResourceDefault{{
			ID: "mini-default", Target: "primary-a", Service: "ags", Status: "active", MaxSessionTTL: "30m", PolicyRevision: "default-v1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	legacySet, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion - 1, ContractRevision: sessionauthority.LegacyContractRevision,
		LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers:          []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "urn:issuer", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "trust-v1"}},
		Bindings: []sessionauthority.PrincipalBinding{
			{ID: "binding-active", IssuerInstanceID: "issuer", Subject: "agent-1", PrincipalID: 7, Status: "active", BindingRevision: "binding-v1", ExpiresAt: "2030-01-02T03:04:05+02:00"},
			{ID: "binding-revoked", IssuerInstanceID: "issuer", Subject: "agent-2", PrincipalID: 8, Status: "revoked", BindingRevision: "binding-v2", RevokedAt: "2029-02-03T04:05:06+02:00"},
		},
		Resources: []sessionauthority.ResourcePolicy{{ID: "repo", Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Status: "active", MaxSessionTTL: "30m", PolicyRevision: "repo-v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		kind  string
		value any
		want  string
	}{
		{
			name:  "canonical team producer",
			kind:  AuthorityBoundaryReceiptLegacyCaptureKind,
			value: legacyAuthorityCapturePayloadForConfig(canonicalSet.Config(), []authorityPrincipalReadback{{ID: 7, Login: "agent", Status: "active", SiteAdmin: false}}),
			want:  `{"schema":"ags.authority-boundary.legacy-capture.v2","version":2,"contract_revision":"2026-07-24.team-authority-v4","trusted_issuers":[{"id":"issuer","issuer":"urn:issuer","status":"active","trust_revision":"trust-v1"}],"bindings":null,"team_bindings":[{"id":"team","issuer_instance_id":"issuer","workspace_id":"workspace","team_identity_id":"team-1","policy_class":"contributor","principal_id":7,"status":"active","binding_revision":"team-v1","epoch_floor":1}],"policy_classes":[{"id":"contributor","status":"active","policy_revision":"class-v1","operations":["repo.read"]}],"resource_defaults":[],"resources":[{"id":"repo","target":"primary-a","service":"ags","repository":"operator/project-kit","status":"active","max_session_ttl":"30m","policy_revision":"repo-v1"}],"principals":[{"id":7,"login":"agent","status":"active","site_admin":false}]}`,
		},
		{
			name:  "canonical team producer with target resource default",
			kind:  AuthorityBoundaryReceiptLegacyCaptureKind,
			value: legacyAuthorityCapturePayloadForConfig(defaultSet.Config(), []authorityPrincipalReadback{{ID: 7, Login: "agent", Status: "active", SiteAdmin: false}}),
			want:  `{"schema":"ags.authority-boundary.legacy-capture.v2","version":2,"contract_revision":"2026-07-24.team-authority-v4","trusted_issuers":[{"id":"issuer","issuer":"urn:issuer","status":"active","trust_revision":"trust-v1"}],"bindings":null,"team_bindings":[{"id":"team","issuer_instance_id":"issuer","workspace_id":"workspace","team_identity_id":"team-1","policy_class":"contributor","principal_id":7,"status":"active","binding_revision":"team-v1","epoch_floor":1}],"policy_classes":[{"id":"contributor","status":"active","policy_revision":"class-v1","operations":["repo.read"]}],"resource_defaults":[{"id":"mini-default","target":"primary-a","service":"ags","status":"active","max_session_ttl":"30m","policy_revision":"default-v1"}],"resources":[],"principals":[{"id":7,"login":"agent","status":"active","site_admin":false}]}`,
		},
		{
			name:  "legacy subject producer with optional lifecycle fields",
			kind:  AuthorityBoundaryReceiptLegacyCaptureKind,
			value: legacyAuthorityCapturePayloadForConfig(legacySet.Config(), []authorityPrincipalReadback{{ID: 7, Login: "agent-1", Status: "active"}, {ID: 8, Login: "agent-2", Status: "active"}}),
			want:  `{"schema":"ags.authority-boundary.legacy-capture.v2","version":2,"contract_revision":"2026-07-24.team-authority-v4","legacy_compatibility_mode":"legacy-subject-v2","trusted_issuers":[{"id":"issuer","issuer":"urn:issuer","status":"active","trust_revision":"trust-v1"}],"bindings":[{"id":"binding-active","issuer_instance_id":"issuer","subject":"agent-1","principal_id":7,"status":"active","binding_revision":"binding-v1","expires_at":"2030-01-02T01:04:05Z"},{"id":"binding-revoked","issuer_instance_id":"issuer","subject":"agent-2","principal_id":8,"status":"revoked","binding_revision":"binding-v2","revoked_at":"2029-02-03T02:05:06Z"}],"team_bindings":null,"policy_classes":[],"resource_defaults":[],"resources":[{"id":"repo","target":"primary-a","service":"ags","repository":"operator/project-kit","status":"active","max_session_ttl":"30m","policy_revision":"repo-v1"}],"principals":[{"id":7,"login":"agent-1","status":"active","site_admin":false},{"id":8,"login":"agent-2","status":"active","site_admin":false}]}`,
		},
		{
			name: "historical closed v1 legacy capture remains readable",
			kind: AuthorityBoundaryReceiptLegacyCaptureV1Kind,
			value: legacyAuthorityCapturePayloadV1{
				Schema: legacyAuthorityCaptureV1Schema, Version: canonicalSet.Config().Version, ContractRevision: canonicalSet.Config().ContractRevision,
				TrustedIssuers: []authorityIssuerProjection{{ID: "issuer", Issuer: "urn:issuer", Status: "active", TrustRevision: "trust-v1"}},
				Bindings:       canonicalSet.Config().Bindings, TeamBindings: canonicalSet.Config().TeamBindings, PolicyClasses: canonicalSet.Config().PolicyClasses,
				Resources: canonicalSet.Config().Resources, Principals: []authorityPrincipalReadback{{ID: 7, Login: "agent", Status: "active", SiteAdmin: false}},
			},
			want: `{"schema":"ags.authority-boundary.legacy-capture.v1","version":2,"contract_revision":"2026-07-24.team-authority-v4","trusted_issuers":[{"id":"issuer","issuer":"urn:issuer","status":"active","trust_revision":"trust-v1"}],"bindings":null,"team_bindings":[{"id":"team","issuer_instance_id":"issuer","workspace_id":"workspace","team_identity_id":"team-1","policy_class":"contributor","principal_id":7,"status":"active","binding_revision":"team-v1","epoch_floor":1}],"policy_classes":[{"id":"contributor","status":"active","policy_revision":"class-v1","operations":["repo.read"]}],"resources":[{"id":"repo","target":"primary-a","service":"ags","repository":"operator/project-kit","status":"active","max_session_ttl":"30m","policy_revision":"repo-v1"}],"principals":[{"id":7,"login":"agent","status":"active","site_admin":false}]}`,
		},
		{
			name:  "historical delegated producer",
			kind:  AuthorityBoundaryReceiptDelegatedEffectKind,
			value: delegatedEffectPayload{Schema: "ags.authority-boundary.delegated-effect.v1", SessionID: "session", PrincipalID: 7, RepositoryID: 9, Repository: "operator/project-kit", Operation: "git.push", Capabilities: nil, PolicySnapshotHash: "snapshot", NativeGrantRevision: "grant-v1"},
			want:  `{"schema":"ags.authority-boundary.delegated-effect.v1","session_id":"session","principal_id":7,"repository_id":9,"repository":"operator/project-kit","operation":"git.push","capabilities":null,"policy_snapshot_hash":"snapshot","native_grant_revision":"grant-v1"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tc.want {
				t.Fatalf("encoded=%s\nwant=%s", encoded, tc.want)
			}
			canonical, err := canonicalReceiptJSON(tc.kind, encoded)
			if err != nil || string(canonical) != tc.want {
				t.Fatalf("canonical=%s err=%v want=%s", canonical, err, tc.want)
			}
		})
	}
}
