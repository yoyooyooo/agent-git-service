package forgejointegration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type retirementAuthorityFixture struct {
	force       bool
	fingerprint string
}

func (f *retirementAuthorityFixture) EnsureRepository(context.Context, string, string, bool) error {
	return nil
}
func (f *retirementAuthorityFixture) EnsurePullRequest(context.Context, PullRequestRequest) (PullRequestResult, error) {
	return PullRequestResult{}, nil
}
func (f *retirementAuthorityFixture) UpdatePullRequestState(context.Context, string, string, int, string) (PullRequestResult, error) {
	return PullRequestResult{}, nil
}
func (f *retirementAuthorityFixture) InspectRepositoryAuthority(context.Context, string, string, string, string, string) (RepositoryAuthorityState, error) {
	return RepositoryAuthorityState{
		BaseBranchProtected: true, ForcePushBlocked: !f.force,
		IntegrationBotCollaborator: true, IntegrationBotAuthorized: true,
		IntegrationBotMergeAuthorized: true,
	}, nil
}
func (f *retirementAuthorityFixture) ApplyRepositoryAuthority(context.Context, string, string, string, string, string, string, RepositoryAuthorityState) error {
	return nil
}
func (f *retirementAuthorityFixture) InspectArtifactRetirementProtection(context.Context, string, string, string) (ArtifactRetirementProtectionState, error) {
	return ArtifactRetirementProtectionState{Exists: true, ForcePushEnabled: f.force, PolicyFingerprint: f.fingerprint}, nil
}
func (f *retirementAuthorityFixture) SetArtifactRetirementForce(_ context.Context, _, _, _, fingerprint string, enabled bool) error {
	if fingerprint != f.fingerprint {
		return context.Canceled
	}
	f.force = enabled
	return nil
}

func TestArtifactRetirementForcePolicyIsRestrictedAndRestored(t *testing.T) {
	fixture := &retirementAuthorityFixture{fingerprint: strings.Repeat("a", 64)}
	enabled := true
	integration := NewWithGitCapabilities(Config{
		Enabled: true, IntegrationBot: "ags-bot",
		RepoMap: map[string]RepoMapping{
			"owner/repo": {Owner: "mirror", Repo: "repo", Enabled: &enabled, BaseBranch: "main"},
		},
	}, fixture, func(context.Context, PushRequest) error { return nil }, func(context.Context, string, string, string) (string, error) {
		return strings.Repeat("1", 40), nil
	})
	state, required, err := integration.ArtifactRetirementForcePolicy(context.Background(), "owner/repo", "main")
	if err != nil || !required || state.PolicyFingerprint != fixture.fingerprint || state.ForcePushEnabled {
		t.Fatalf("policy: %+v required=%v err=%v", state, required, err)
	}
	if err := integration.SetArtifactRetirementForce(context.Background(), "owner/repo", "main", state.PolicyFingerprint, true); err != nil || !fixture.force {
		t.Fatalf("enable: force=%v err=%v", fixture.force, err)
	}
	if err := integration.SetArtifactRetirementForce(context.Background(), "owner/repo", "main", state.PolicyFingerprint, false); err != nil || fixture.force {
		t.Fatalf("restore: force=%v err=%v", fixture.force, err)
	}
	if _, required, err := integration.ArtifactRetirementForcePolicy(context.Background(), "owner/repo", "feature"); err != nil || required {
		t.Fatalf("non-base policy window: required=%v err=%v", required, err)
	}
}

func TestHTTPArtifactRetirementForcePatchChangesOnlyForceBit(t *testing.T) {
	force := false
	requiredApprovals := 2
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/mirror/repo/branch_protections/main" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"branch_name": "main", "rule_name": "main",
				"enable_push": true, "enable_force_push": force,
				"enable_push_whitelist": false, "push_whitelist_usernames": []string{},
				"enable_merge_whitelist": false, "merge_whitelist_usernames": []string{},
				"enable_status_check": true, "status_check_contexts": []string{"CI Success"},
				"required_approvals": requiredApprovals, "apply_to_admins": false,
				"block_on_outdated_branch": true,
			})
		case http.MethodPatch:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body) != 1 {
				t.Fatalf("force window mutated unrelated policy: %#v", body)
			}
			value, ok := body["enable_force_push"].(bool)
			if !ok {
				t.Fatalf("force patch missing boolean: %#v", body)
			}
			force = value
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "operator-fixture")
	before, err := client.InspectArtifactRetirementProtection(context.Background(), "mirror", "repo", "main")
	if err != nil || !before.Exists || before.ForcePushEnabled {
		t.Fatalf("before: %+v %v", before, err)
	}
	if err := client.SetArtifactRetirementForce(context.Background(), "mirror", "repo", "main", before.PolicyFingerprint, true); err != nil {
		t.Fatal(err)
	}
	opened, err := client.InspectArtifactRetirementProtection(context.Background(), "mirror", "repo", "main")
	if err != nil || !opened.ForcePushEnabled || opened.PolicyFingerprint != before.PolicyFingerprint {
		t.Fatalf("opened: %+v %v", opened, err)
	}
	if err := client.SetArtifactRetirementForce(context.Background(), "mirror", "repo", "main", before.PolicyFingerprint, false); err != nil {
		t.Fatal(err)
	}
	restored, err := client.InspectArtifactRetirementProtection(context.Background(), "mirror", "repo", "main")
	if err != nil || restored.ForcePushEnabled || restored.PolicyFingerprint != before.PolicyFingerprint || requiredApprovals != 2 {
		t.Fatalf("restored: %+v approvals=%d err=%v", restored, requiredApprovals, err)
	}
}
