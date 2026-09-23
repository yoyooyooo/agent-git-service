package sessionauthority_test

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

func TestCanonicalTeamAuthorityUsesTeamClassNotSubjectAndIntersectsOperations(t *testing.T) {
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "multica", Issuer: "https://multica.test", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "trust-v1"}},
		PolicyClasses:  []sessionauthority.PolicyClass{{ID: "multica.workspace.default.v1", Status: "active", PolicyRevision: "class-v1", Operations: []string{"ci.read", "git.push", "git.read", "pr.create", "pr.rebase", "pr.read", "repo.read", "review.read"}}},
		TeamBindings:   []sessionauthority.TeamBinding{{ID: "team", IssuerInstanceID: "multica", WorkspaceID: "workspace-1", TeamIdentityID: "team-immutable", PolicyClass: "multica.workspace.default.v1", PrincipalID: 7, Status: "active", BindingRevision: "team-v1", EpochFloor: 3}},
		Resources:      []sessionauthority.ResourcePolicy{{ID: "repo", Target: "primary-a", Service: "ags", Repository: "operator/agent-git-service", Status: "active", MaxSessionTTL: "30m", PolicyRevision: "resource-v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := sessionauthority.Request{Issuer: "https://multica.test", IssuerInstanceID: "multica", AssertionKeyID: "key", Target: "primary-a", Service: "ags", Repository: "operator/agent-git-service", WorkspaceID: "workspace-1", TeamIdentityID: "team-immutable", PolicyClass: "multica.workspace.default.v1", MembershipEpoch: 3}
	for _, subject := range []string{"previously-unknown-agent", "new-squad-member"} {
		request := base
		request.Subject, request.Operation = subject, "pr.rebase"
		resolved, err := set.Resolve(request)
		if err != nil || resolved.PrincipalID != 7 || resolved.TeamBinding.ID != "team" || resolved.Binding.ID != "" {
			t.Fatalf("subject %q resolved=%#v err=%v", subject, resolved, err)
		}
	}
	for _, operation := range []string{"pr.merge", "review.submit", "repo.admin", "repo.create"} {
		request := base
		request.Operation = operation
		if _, err := set.Resolve(request); !errors.Is(err, sessionauthority.ErrOperationNotSupported) {
			t.Fatalf("%s err=%v", operation, err)
		}
	}
	stale := base
	stale.Operation, stale.MembershipEpoch = "repo.read", 2
	if _, err := set.Resolve(stale); !errors.Is(err, sessionauthority.ErrTeamBindingInactive) {
		t.Fatalf("stale epoch err=%v", err)
	}
}

func TestDefaultDynamicPolicyClassRejectsOperationExpansionAtConfigBoundary(t *testing.T) {
	base := sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "multica", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "v1"}},
		TeamBindings:   []sessionauthority.TeamBinding{{ID: "team", IssuerInstanceID: "issuer", WorkspaceID: "workspace-1", TeamIdentityID: "team", PolicyClass: sessionauthority.DefaultDynamicPolicyClass, PrincipalID: 1, Status: "active", BindingRevision: "v1"}},
		Resources:      []sessionauthority.ResourcePolicy{{ID: "repo", Target: "primary-a", Service: "ags", Repository: "operator/agent-git-service", Status: "active", MaxSessionTTL: "1m", PolicyRevision: "v1"}},
	}
	allowed := []string{"ci.read", "git.push", "git.read", "pr.create", "pr.rebase", "pr.read", "repo.read", "review.read"}
	for _, operation := range []string{"pr.merge", "review.submit", "repo.admin", "repo.create"} {
		t.Run(operation, func(t *testing.T) {
			config := base
			config.PolicyClasses = []sessionauthority.PolicyClass{{ID: sessionauthority.DefaultDynamicPolicyClass, Status: "active", PolicyRevision: "class-v1", Operations: append(append([]string(nil), allowed...), operation)}}
			if _, err := sessionauthority.New(config); err == nil {
				t.Fatalf("default dynamic class accepted operation expansion %q", operation)
			}
		})
	}

	t.Run("required read surface cannot be omitted", func(t *testing.T) {
		config := base
		config.PolicyClasses = []sessionauthority.PolicyClass{{ID: sessionauthority.DefaultDynamicPolicyClass, Status: "active", PolicyRevision: "class-v1", Operations: allowed[:len(allowed)-1]}}
		if _, err := sessionauthority.New(config); err == nil {
			t.Fatal("default dynamic class accepted an incomplete fixed operation set")
		}
	})
}

func TestAuthorityConfigRejectsSecretShapedUnknownFieldWithoutLeakingIt(t *testing.T) {
	unknown := strings.Join([]string{"mat", "_secret_token"}, "")
	var config sessionauthority.Config
	err := yaml.Unmarshal([]byte("version: 2\ncontract_revision: 2026-07-24.team-authority-v4\n"+unknown+": must-not-leak\n"), &config)
	if err == nil {
		t.Fatal("expected unknown field denial")
	}
	if strings.Contains(err.Error(), unknown) || strings.Contains(err.Error(), "must-not-leak") {
		t.Fatalf("config rejection leaked secret-shaped input: %v", err)
	}
}

func TestAuthorityConfigRejectsSecretShapedValuesWithoutLeakingThem(t *testing.T) {
	secrets := []string{
		strings.Join([]string{"mat", "_config_secret"}, ""),
		strings.Join([]string{"ags", "_sess_config_secret"}, ""),
		strings.Join([]string{"ey", "JhbGciOiJIUzI1NiJ9", ".eyJzdWIiOiJ4In0", ".signature"}, ""),
		strings.Join([]string{"-----BE", "GIN PRIVATE KEY-----"}, ""),
	}
	for _, secret := range secrets {
		t.Run(secret[:3], func(t *testing.T) {
			_, err := sessionauthority.New(sessionauthority.Config{
				Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
				TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "multica", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "v1"}},
				PolicyClasses:  []sessionauthority.PolicyClass{{ID: "class", Status: "active", PolicyRevision: "v1", Operations: []string{secret}}},
				TeamBindings:   []sessionauthority.TeamBinding{{ID: "team", IssuerInstanceID: "issuer", WorkspaceID: "workspace-1", TeamIdentityID: "team", PolicyClass: "class", PrincipalID: 1, Status: "active", BindingRevision: "v1"}},
				Resources:      []sessionauthority.ResourcePolicy{{ID: "repo", Target: "primary-a", Service: "ags", Repository: "operator/agent-git-service", Status: "active", MaxSessionTTL: "1m", PolicyRevision: "v1"}},
			})
			if err == nil {
				t.Fatal("expected secret-shaped authority value denial")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("authority error leaked secret-shaped value: %v", err)
			}
		})
	}
}

func TestCanonicalTeamAuthorityConfigIsDeeplyImmutable(t *testing.T) {
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "multica", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "v1"}},
		PolicyClasses:  []sessionauthority.PolicyClass{{ID: "class", Status: "active", PolicyRevision: "v1", Operations: []string{"repo.read"}}},
		TeamBindings:   []sessionauthority.TeamBinding{{ID: "team", IssuerInstanceID: "issuer", WorkspaceID: "workspace-1", TeamIdentityID: "team", PolicyClass: "class", PrincipalID: 1, Status: "active", BindingRevision: "v1"}},
		Resources:      []sessionauthority.ResourcePolicy{{ID: "repo", Target: "primary-a", Service: "ags", Repository: "operator/agent-git-service", Status: "active", MaxSessionTTL: "1m", PolicyRevision: "v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := set.Config()
	config.PolicyClasses[0].Operations[0] = "repo.admin"
	_, err = set.Resolve(sessionauthority.Request{Issuer: "multica", IssuerInstanceID: "issuer", AssertionKeyID: "key", WorkspaceID: "workspace-1", TeamIdentityID: "team", PolicyClass: "class", MembershipEpoch: 1, Target: "primary-a", Service: "ags", Repository: "operator/agent-git-service", Operation: "repo.admin"})
	if !errors.Is(err, sessionauthority.ErrOperationNotSupported) {
		t.Fatalf("mutated config altered authority: %v", err)
	}
}

func TestResolveReturnsDeepImmutableAuthoritySnapshot(t *testing.T) {
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "multica", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "v1"}},
		PolicyClasses:  []sessionauthority.PolicyClass{{ID: "class", Status: "active", PolicyRevision: "v1", Operations: []string{"repo.read"}}},
		TeamBindings:   []sessionauthority.TeamBinding{{ID: "team", IssuerInstanceID: "issuer", WorkspaceID: "workspace-1", TeamIdentityID: "team", PolicyClass: "class", PrincipalID: 1, Status: "active", BindingRevision: "v1"}},
		Resources:      []sessionauthority.ResourcePolicy{{ID: "repo", Target: "primary-a", Service: "ags", Repository: "operator/agent-git-service", Status: "active", MaxSessionTTL: "1m", PolicyRevision: "v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := sessionauthority.Request{Issuer: "multica", IssuerInstanceID: "issuer", AssertionKeyID: "key", WorkspaceID: "workspace-1", TeamIdentityID: "team", PolicyClass: "class", MembershipEpoch: 1, Target: "primary-a", Service: "ags", Repository: "operator/agent-git-service", Operation: "repo.read"}
	resolved, err := set.Resolve(request)
	if err != nil {
		t.Fatal(err)
	}
	resolved.Issuer.KeyIDs[0] = "attacker-key"
	resolved.PolicyClass.Operations[0] = "repo.admin"
	resolved.Operation.Capabilities[0] = "repo:write"
	resolved, err = set.Resolve(request)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Issuer.KeyIDs[0] != "key" || resolved.PolicyClass.Operations[0] != "repo.read" || resolved.Operation.Capabilities[0] != "repo:read" {
		t.Fatalf("Resolve leaked mutable authority state: %#v", resolved)
	}
}

func TestCanonicalTeamAuthorityCannotMixLegacyBindings(t *testing.T) {
	_, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "multica", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "v1"}},
		Bindings:       []sessionauthority.PrincipalBinding{{ID: "legacy", IssuerInstanceID: "issuer", Subject: "agent", PrincipalID: 1, Status: "active", BindingRevision: "v1"}},
		PolicyClasses:  []sessionauthority.PolicyClass{{ID: "class", Status: "active", PolicyRevision: "v1", Operations: []string{"repo.read"}}},
		TeamBindings:   []sessionauthority.TeamBinding{{ID: "team", IssuerInstanceID: "issuer", WorkspaceID: "workspace-1", TeamIdentityID: "team", PolicyClass: "class", PrincipalID: 1, Status: "active", BindingRevision: "v1"}},
		Resources:      []sessionauthority.ResourcePolicy{{ID: "repo", Target: "primary-a", Service: "ags", Repository: "operator/agent-git-service", Status: "active", MaxSessionTTL: "1m", PolicyRevision: "v1"}},
	})
	if err == nil {
		t.Fatal("mixed legacy and canonical authority should fail")
	}
}

func TestResolveRuntimePolicyAuthorityUsesTrustedSourceAndRejectsAmbiguity(t *testing.T) {
	config := sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "multica-mini", Issuer: "multica", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "trust-v1"}},
		PolicyClasses:  []sessionauthority.PolicyClass{{ID: sessionauthority.DefaultDynamicPolicyClass, Status: "active", PolicyRevision: "class-v1", Operations: []string{"ci.read", "git.push", "git.read", "pr.create", "pr.rebase", "pr.read", "repo.read", "review.read"}}},
		TeamBindings:   []sessionauthority.TeamBinding{{ID: "team-a", IssuerInstanceID: "multica-mini", WorkspaceID: "workspace-1", TeamIdentityID: "team-a", PolicyClass: sessionauthority.DefaultDynamicPolicyClass, PrincipalID: 7, Status: "active", BindingRevision: "team-a-v1"}},
		Resources:      []sessionauthority.ResourcePolicy{{ID: "repo", Target: "primary-a", Service: "ags", Repository: "example-owner/demo", Status: "active", MaxSessionTTL: "30m", PolicyRevision: "repo-v1"}},
	}
	set, err := sessionauthority.New(config)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := set.ResolveRuntimePolicyAuthority("multica-mini", "workspace-1", sessionauthority.DefaultDynamicPolicyClass)
	if err != nil || resolved.TeamBinding.PrincipalID != 7 || resolved.TeamBinding.TeamIdentityID != "team-a" {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	if _, err := set.ResolveRuntimePolicyAuthority("unknown", "workspace-1", sessionauthority.DefaultDynamicPolicyClass); !errors.Is(err, sessionauthority.ErrIssuerNotFound) {
		t.Fatalf("unknown issuer err=%v", err)
	}

	config.TeamBindings = append(config.TeamBindings, sessionauthority.TeamBinding{
		ID: "team-b", IssuerInstanceID: "multica-mini", WorkspaceID: "workspace-1", TeamIdentityID: "team-b",
		PolicyClass: sessionauthority.DefaultDynamicPolicyClass, PrincipalID: 8, Status: "active", BindingRevision: "team-b-v1",
	})
	ambiguous, err := sessionauthority.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ambiguous.ResolveRuntimePolicyAuthority("multica-mini", "workspace-1", sessionauthority.DefaultDynamicPolicyClass); !errors.Is(err, sessionauthority.ErrTeamBindingAmbiguous) {
		t.Fatalf("ambiguous authority err=%v", err)
	}
}
