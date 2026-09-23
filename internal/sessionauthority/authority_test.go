package sessionauthority_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

func TestResolveUsesExplicitIssuerSubjectBindingsAndAllowsNToOne(t *testing.T) {
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	set, err := sessionauthority.New(sessionauthority.Config{
		Version:                 sessionauthority.CurrentVersion,
		ContractRevision:        sessionauthority.ContractRevision,
		LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers: []sessionauthority.TrustedIssuer{
			{ID: "multica-mini", Issuer: "multica", KeyIDs: []string{"mini-key"}, Status: "active", TrustRevision: "trust-mini-v1"},
			{ID: "multica-region-b", Issuer: "https://multica.region-b.example", KeyIDs: []string{"region-b-key"}, Status: "active", TrustRevision: "trust-region-b-v1"},
		},
		Bindings: []sessionauthority.PrincipalBinding{
			{ID: "binding-mini", IssuerInstanceID: "multica-mini", Subject: "agent-mini-1", PrincipalID: 42, Status: "active", BindingRevision: "binding-mini-v1"},
			{ID: "binding-region-b", IssuerInstanceID: "multica-region-b", Subject: "agent-region-b-9", PrincipalID: 42, Status: "active", BindingRevision: "binding-region-b-v1"},
		},
		Resources: []sessionauthority.ResourcePolicy{
			{ID: "repo-agent-kit", Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Status: "active", MaxSessionTTL: "30m", PolicyRevision: "repo-policy-v1"},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range []struct {
		name             string
		issuer           string
		issuerInstanceID string
		subject          string
		keyID            string
		wantBinding      string
	}{
		{name: "primary-a", issuer: "multica", issuerInstanceID: "multica-mini", subject: "agent-mini-1", keyID: "mini-key", wantBinding: "binding-mini"},
		{name: "region-b", issuer: "https://multica.region-b.example", issuerInstanceID: "multica-region-b", subject: "agent-region-b-9", keyID: "region-b-key", wantBinding: "binding-region-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := set.Resolve(sessionauthority.Request{
				Issuer: tc.issuer, IssuerInstanceID: tc.issuerInstanceID, AssertionKeyID: tc.keyID, Subject: tc.subject,
				Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Operation: "pr.create", Now: now,
			})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if resolved.PrincipalID != 42 || resolved.Binding.ID != tc.wantBinding {
				t.Fatalf("resolved = %#v", resolved)
			}
			if resolved.Operation.Name != "pr.create" || resolved.Operation.RequiredPermission != sessionauthority.PermissionWrite {
				t.Fatalf("operation = %#v", resolved.Operation)
			}
		})
	}
}

func TestResolveBindsIssuerInstanceToVerifiedAssertionKey(t *testing.T) {
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision, LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers: []sessionauthority.TrustedIssuer{
			{ID: "multica-mini", Issuer: "multica", KeyIDs: []string{"mini-key"}, Status: "active", TrustRevision: "trust-mini-v1"},
			{ID: "multica-region-b", Issuer: "multica", KeyIDs: []string{"region-b-key"}, Status: "active", TrustRevision: "trust-region-b-v1"},
		},
		Bindings: []sessionauthority.PrincipalBinding{
			{ID: "binding-mini", IssuerInstanceID: "multica-mini", Subject: "agent-1", PrincipalID: 42, Status: "active", BindingRevision: "binding-mini-v1"},
			{ID: "binding-region-b", IssuerInstanceID: "multica-region-b", Subject: "agent-1", PrincipalID: 42, Status: "active", BindingRevision: "binding-region-b-v1"},
		},
		Resources: []sessionauthority.ResourcePolicy{{ID: "repo", Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "repo-v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := sessionauthority.Request{Issuer: "multica", Subject: "agent-1", Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Operation: "repo.read"}
	base.IssuerInstanceID, base.AssertionKeyID = "multica-mini", "region-b-key"
	if _, err := set.Resolve(base); !errors.Is(err, sessionauthority.ErrIssuerNotFound) {
		t.Fatalf("cross-instance key error = %v", err)
	}
	base.IssuerInstanceID, base.AssertionKeyID = "", "mini-key"
	resolved, err := set.Resolve(base)
	if err != nil || resolved.Issuer.ID != "multica-mini" {
		t.Fatalf("key-bound inference = %#v, %v", resolved, err)
	}
}

func TestResolveResourceOperationSharesRegistryAndUniquePolicy(t *testing.T) {
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision, LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "multica", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "trust-v1"}},
		Bindings:       []sessionauthority.PrincipalBinding{{ID: "binding", IssuerInstanceID: "issuer", Subject: "subject", PrincipalID: 42, Status: "active", BindingRevision: "binding-v1"}},
		Resources:      []sessionauthority.ResourcePolicy{{ID: "repo", Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "repo-policy-v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := set.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
		Service: "ags", Repository: "operator/project-kit", Operation: "review.submit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Resource.PolicyRevision != "repo-policy-v1" || resolved.Operation.RequiredPermission != sessionauthority.PermissionWrite {
		t.Fatalf("resolved = %#v", resolved)
	}
	if _, err := set.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
		Service: "ags", Repository: "operator/project-kit", Operation: "unknown.operation",
	}); !errors.Is(err, sessionauthority.ErrOperationNotSupported) {
		t.Fatalf("unknown operation error = %v", err)
	}
}

func TestResolveResourceOperationDerivesRepositoryFromTargetDefault(t *testing.T) {
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision, LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "multica", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "trust-v1"}},
		Bindings:       []sessionauthority.PrincipalBinding{{ID: "binding", IssuerInstanceID: "issuer", Subject: "subject", PrincipalID: 42, Status: "active", BindingRevision: "binding-v1"}},
		ResourceDefaults: []sessionauthority.ResourceDefault{{
			ID: "mini-default", Target: "primary-a", Service: "ags", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "mini-default-v1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := set.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
		Target: "primary-a", Service: "ags", Repository: "operator/new-project", Operation: "pr.create",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Resource.ID != "mini-default" || resolved.Resource.Repository != "operator/new-project" ||
		resolved.Resource.PolicyRevision != "mini-default-v1" || resolved.Resource.MaxSessionDuration() != 15*time.Minute {
		t.Fatalf("derived resource = %#v", resolved.Resource)
	}
}

func TestResolveResourceOperationRejectsAmbiguousTargetlessDefaults(t *testing.T) {
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision, LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "multica", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "trust-v1"}},
		Bindings:       []sessionauthority.PrincipalBinding{{ID: "binding", IssuerInstanceID: "issuer", Subject: "subject", PrincipalID: 42, Status: "active", BindingRevision: "binding-v1"}},
		ResourceDefaults: []sessionauthority.ResourceDefault{
			{ID: "mini-default", Target: "primary-a", Service: "ags", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "mini-default-v1"},
			{ID: "region-b-default", Target: "region-b", Service: "ags", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "region-b-default-v1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
		Service: "ags", Repository: "operator/new-project", Operation: "repo.read",
	}); !errors.Is(err, sessionauthority.ErrResourceAmbiguous) {
		t.Fatalf("ambiguous targetless defaults error = %v", err)
	}
	resolved, err := set.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
		Target: "primary-a", Service: "ags", Repository: "operator/new-project", Operation: "repo.read",
	})
	if err != nil || resolved.Resource.PolicyRevision != "mini-default-v1" {
		t.Fatalf("targeted default resolve = %#v, %v", resolved, err)
	}
}

func TestResolveResourceOperationTargetlessEvaluatesEffectiveCandidatePerTarget(t *testing.T) {
	base := func() sessionauthority.Config {
		return sessionauthority.Config{
			Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision, LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
			TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "multica", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "trust-v1"}},
			Bindings:       []sessionauthority.PrincipalBinding{{ID: "binding", IssuerInstanceID: "issuer", Subject: "subject", PrincipalID: 42, Status: "active", BindingRevision: "binding-v1"}},
		}
	}
	t.Run("active exact and another target default are ambiguous", func(t *testing.T) {
		config := base()
		config.Resources = []sessionauthority.ResourcePolicy{{ID: "mini-exact", Target: "primary-a", Service: "ags", Repository: "operator/project", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "mini-exact-v1"}}
		config.ResourceDefaults = []sessionauthority.ResourceDefault{{ID: "region-b-default", Target: "region-b", Service: "ags", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "region-b-default-v1"}}
		set, err := sessionauthority.New(config)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := set.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{Service: "ags", Repository: "operator/project", Operation: "repo.read"}); !errors.Is(err, sessionauthority.ErrResourceAmbiguous) {
			t.Fatalf("cross-target exact/default error = %v", err)
		}
	})
	t.Run("same target exact overrides its default", func(t *testing.T) {
		config := base()
		config.Resources = []sessionauthority.ResourcePolicy{{ID: "mini-exact", Target: "primary-a", Service: "ags", Repository: "operator/project", Status: "active", MaxSessionTTL: "5m", PolicyRevision: "mini-exact-v1"}}
		config.ResourceDefaults = []sessionauthority.ResourceDefault{{ID: "mini-default", Target: "primary-a", Service: "ags", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "mini-default-v1"}}
		set, err := sessionauthority.New(config)
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := set.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{Service: "ags", Repository: "operator/project", Operation: "repo.read"})
		if err != nil || resolved.Resource.ID != "mini-exact" || resolved.Resource.PolicyRevision != "mini-exact-v1" {
			t.Fatalf("same-target override = %#v, %v", resolved, err)
		}
	})
	t.Run("disabled exact screens only its own target default", func(t *testing.T) {
		config := base()
		config.Resources = []sessionauthority.ResourcePolicy{{ID: "mini-deny", Target: "primary-a", Service: "ags", Repository: "operator/project", Status: "disabled", MaxSessionTTL: "5m", PolicyRevision: "mini-deny-v1"}}
		config.ResourceDefaults = []sessionauthority.ResourceDefault{
			{ID: "mini-default", Target: "primary-a", Service: "ags", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "mini-default-v1"},
			{ID: "region-b-default", Target: "region-b", Service: "ags", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "region-b-default-v1"},
		}
		set, err := sessionauthority.New(config)
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := set.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{Service: "ags", Repository: "operator/project", Operation: "repo.read"})
		if err != nil || resolved.Resource.ID != "region-b-default" || resolved.Resource.Target != "region-b" {
			t.Fatalf("cross-target candidate after exact deny = %#v, %v", resolved, err)
		}
	})
}

func TestResolveResourceOperationExactDisabledOverrideBlocksDefault(t *testing.T) {
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision, LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "multica", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "trust-v1"}},
		Bindings:       []sessionauthority.PrincipalBinding{{ID: "binding", IssuerInstanceID: "issuer", Subject: "subject", PrincipalID: 42, Status: "active", BindingRevision: "binding-v1"}},
		ResourceDefaults: []sessionauthority.ResourceDefault{{
			ID: "mini-default", Target: "primary-a", Service: "ags", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "mini-default-v1",
		}},
		Resources: []sessionauthority.ResourcePolicy{{
			ID: "sensitive-repo", Target: "primary-a", Service: "ags", Repository: "operator/sensitive", Status: "disabled", MaxSessionTTL: "5m", PolicyRevision: "sensitive-deny-v1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
		Target: "primary-a", Service: "ags", Repository: "operator/sensitive", Operation: "repo.read",
	}); !errors.Is(err, sessionauthority.ErrResourceInactive) {
		t.Fatalf("disabled exact override error = %v", err)
	}
	resolved, err := set.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
		Target: "primary-a", Service: "ags", Repository: "operator/ordinary", Operation: "repo.read",
	})
	if err != nil || resolved.Resource.PolicyRevision != "mini-default-v1" {
		t.Fatalf("ordinary derived resource = %#v, %v", resolved, err)
	}
}

func TestResolveResourceOperationSelectsUniqueActivePolicyWithDisabledSiblings(t *testing.T) {
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision, LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "multica", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "trust-v1"}},
		Bindings:       []sessionauthority.PrincipalBinding{{ID: "binding", IssuerInstanceID: "issuer", Subject: "subject", PrincipalID: 42, Status: "active", BindingRevision: "binding-v1"}},
		Resources: []sessionauthority.ResourcePolicy{
			{ID: "old-mini", Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Status: "disabled", MaxSessionTTL: "15m", PolicyRevision: "mini-disabled-v1"},
			{ID: "active-region-b", Target: "region-b", Service: "ags", Repository: "operator/project-kit", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "region-b-active-v2"},
			{ID: "old-legacy", Target: "legacy", Service: "ags", Repository: "operator/project-kit", Status: "disabled", MaxSessionTTL: "15m", PolicyRevision: "legacy-disabled-v1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := set.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
		Service: "ags", Repository: "operator/project-kit", Operation: "repo.read",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Resource.ID != "active-region-b" || resolved.Resource.PolicyRevision != "region-b-active-v2" {
		t.Fatalf("resolved resource = %#v", resolved.Resource)
	}
}

func TestResolveResourceOperationRejectsNoActiveTargetlessPolicy(t *testing.T) {
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision, LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "multica", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "trust-v1"}},
		Bindings:       []sessionauthority.PrincipalBinding{{ID: "binding", IssuerInstanceID: "issuer", Subject: "subject", PrincipalID: 42, Status: "active", BindingRevision: "binding-v1"}},
		Resources: []sessionauthority.ResourcePolicy{
			{ID: "old-mini", Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Status: "disabled", MaxSessionTTL: "15m", PolicyRevision: "mini-disabled-v1"},
			{ID: "old-region-b", Target: "region-b", Service: "ags", Repository: "operator/project-kit", Status: "disabled", MaxSessionTTL: "15m", PolicyRevision: "region-b-disabled-v1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
		Service: "ags", Repository: "operator/project-kit", Operation: "repo.read",
	}); !errors.Is(err, sessionauthority.ErrResourceNotFound) {
		t.Fatalf("no-active resource error = %v", err)
	}
}

func TestResolveResourceOperationRejectsAmbiguousTargetlessPolicy(t *testing.T) {
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision, LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "issuer", Issuer: "multica", KeyIDs: []string{"key"}, Status: "active", TrustRevision: "trust-v1"}},
		Bindings:       []sessionauthority.PrincipalBinding{{ID: "binding", IssuerInstanceID: "issuer", Subject: "subject", PrincipalID: 42, Status: "active", BindingRevision: "binding-v1"}},
		Resources: []sessionauthority.ResourcePolicy{
			{ID: "primary-a", Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "mini-v1"},
			{ID: "region-b", Target: "region-b", Service: "ags", Repository: "operator/project-kit", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "region-b-v1"},
			{ID: "legacy", Target: "legacy", Service: "ags", Repository: "operator/project-kit", Status: "disabled", MaxSessionTTL: "15m", PolicyRevision: "legacy-v1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
		Service: "ags", Repository: "operator/project-kit", Operation: "repo.read",
	}); !errors.Is(err, sessionauthority.ErrResourceAmbiguous) {
		t.Fatalf("ambiguous resource error = %v", err)
	}
	resolved, err := set.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
		Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Operation: "repo.read",
	})
	if err != nil || resolved.Resource.PolicyRevision != "mini-v1" {
		t.Fatalf("targeted resolve = %#v, %v", resolved, err)
	}
}

func TestResolveRevokesOneBindingWithoutAffectingAnother(t *testing.T) {
	cfg := sessionauthority.Config{
		Version:                 sessionauthority.CurrentVersion,
		ContractRevision:        sessionauthority.ContractRevision,
		LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers: []sessionauthority.TrustedIssuer{
			{ID: "multica-mini", Issuer: "multica", KeyIDs: []string{"session-key"}, Status: "active", TrustRevision: "trust-v1"},
		},
		Bindings: []sessionauthority.PrincipalBinding{
			{ID: "binding-a", IssuerInstanceID: "multica-mini", Subject: "subject-a", PrincipalID: 42, Status: "revoked", BindingRevision: "binding-a-v2", RevokedAt: "2026-07-18T00:00:00Z"},
			{ID: "binding-b", IssuerInstanceID: "multica-mini", Subject: "subject-b", PrincipalID: 42, Status: "active", BindingRevision: "binding-b-v1"},
		},
		Resources: []sessionauthority.ResourcePolicy{
			{ID: "repo", Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "repo-v1"},
		},
	}
	set, err := sessionauthority.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	base := sessionauthority.Request{Issuer: "multica", IssuerInstanceID: "multica-mini", AssertionKeyID: "session-key", Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Operation: "repo.read", Now: time.Now().UTC()}
	base.Subject = "subject-a"
	if _, err := set.Resolve(base); !errors.Is(err, sessionauthority.ErrBindingInactive) {
		t.Fatalf("revoked binding error = %v", err)
	}
	base.Subject = "subject-b"
	if resolved, err := set.Resolve(base); err != nil || resolved.Binding.ID != "binding-b" {
		t.Fatalf("active sibling resolve = %#v, %v", resolved, err)
	}
}
