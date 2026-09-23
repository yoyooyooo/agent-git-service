package multicaprojection

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestExtractIssueKeyPrefersExplicitMulticaMarker(t *testing.T) {
	got := ExtractIssueKey("title mentions HUM-1", "body\nMultica: hum-34\nother HUM-99")
	if got != "HUM-34" {
		t.Fatalf("ExtractIssueKey() = %q, want HUM-34", got)
	}
}

func TestIssueURLUsesWorkspaceSlugAndIssueKey(t *testing.T) {
	got := IssueURL("https://multica.ai/", "workspace-alpha", "hum-60")
	want := "https://multica.ai/workspace-alpha/issues/HUM-60"
	if got != want {
		t.Fatalf("IssueURL() = %q, want %q", got, want)
	}
}

func TestResolveIssueRefPrefersExplicitWorkspaceMarker(t *testing.T) {
	projection := New(Config{
		Enabled: true,
		Workspaces: map[string]WorkspaceConfig{
			"workspace-beta": {Workspace: "workspace-beta", WorkspaceID: "ws-workspace-beta"},
		},
		Workspace:   "workspace-alpha",
		WorkspaceID: "ws-humanity",
	})

	got := projection.ResolveIssueRef("example-owner/demo", "body\nMultica: workspace-beta/hak-12")
	want := IssueRef{Workspace: "workspace-beta", WorkspaceID: "ws-workspace-beta", IssueKey: "HAK-12", URL: "https://multica.ai/workspace-beta/issues/HAK-12"}
	if got != want {
		t.Fatalf("ResolveIssueRef() = %#v, want %#v", got, want)
	}
}

func TestResolveIssueRefExtractsWorkspaceFromIssueURL(t *testing.T) {
	projection := New(Config{Enabled: true})

	got := projection.ResolveIssueRef("example-owner/demo", "see https://multica.ai/workspace-alpha/issues/hum-66 for context")
	want := IssueRef{Workspace: "workspace-alpha", IssueKey: "HUM-66", URL: "https://multica.ai/workspace-alpha/issues/HUM-66"}
	if got != want {
		t.Fatalf("ResolveIssueRef() = %#v, want %#v", got, want)
	}
}

func TestResolveIssueRefUsesRepoWorkspaceForLegacyMarker(t *testing.T) {
	projection := New(Config{
		Enabled: true,
		Repos: map[string]WorkspaceConfig{
			"example-owner/personal": {Workspace: "workspace-beta", WorkspaceID: "ws-workspace-beta"},
		},
		Workspace:   "workspace-alpha",
		WorkspaceID: "ws-humanity",
	})

	got := projection.ResolveIssueRef("example-owner/personal", "Multica: HAK-12")
	want := IssueRef{Workspace: "workspace-beta", WorkspaceID: "ws-workspace-beta", IssueKey: "HAK-12", URL: "https://multica.ai/workspace-beta/issues/HAK-12"}
	if got != want {
		t.Fatalf("ResolveIssueRef() = %#v, want %#v", got, want)
	}
}

func TestEnrichPullRequestBodyAppendsMulticaIssueURL(t *testing.T) {
	projection := New(Config{Enabled: true, Workspace: "workspace-alpha"})
	got := projection.EnrichPullRequestBody("Multica: HUM-60\n\nSmoke body.")
	want := "Multica: HUM-60\n\nSmoke body.\n\nMultica issue: https://multica.ai/workspace-alpha/issues/HUM-60"
	if got != want {
		t.Fatalf("EnrichPullRequestBody() = %q, want %q", got, want)
	}
	if again := projection.EnrichPullRequestBody(got); again != got {
		t.Fatalf("EnrichPullRequestBody() duplicated issue URL: %q", again)
	}
}

func TestResolveIssueRefIgnoresPlainIssueLikeText(t *testing.T) {
	projection := New(Config{Enabled: true, Workspace: "workspace-alpha"})

	got := projection.ResolveIssueRef(
		"example-team/shipping-fixture",
		"Smoke body.",
		"feat(shipment): data-driven Step-3 ordering + framing",
		"sync/upstream-latest",
	)
	if got != (IssueRef{}) {
		t.Fatalf("ResolveIssueRef() = %#v, want empty ref", got)
	}
}

func TestEnrichPullRequestBodyIgnoresPlainIssueLikeTitle(t *testing.T) {
	projection := New(Config{Enabled: true, Workspace: "workspace-alpha"})
	got := projection.EnrichPullRequestBody("Smoke body.", "HUM-60 smoke")
	want := "Smoke body."
	if got != want {
		t.Fatalf("EnrichPullRequestBody() = %q, want %q", got, want)
	}
}

func TestVerifyPRLinkTokenUsesExternalPRAudience(t *testing.T) {
	secret := "test-secret"
	projection := New(Config{Enabled: true, LinkTokenSecret: secret})
	tokenString := signTestPRLinkToken(t, secret, "external-pr-link")

	claims, err := projection.VerifyPRLinkToken(tokenString)
	if err != nil {
		t.Fatalf("VerifyPRLinkToken() error = %v", err)
	}
	if claims.WorkspaceID != "workspace-id" || claims.IssueID != "issue-id" || claims.IssueKey != "HUM-42" {
		t.Fatalf("VerifyPRLinkToken() claims = %#v", claims)
	}

	agsAudienceToken := signTestPRLinkToken(t, secret, "ags")
	if _, err := projection.VerifyPRLinkToken(agsAudienceToken); err == nil {
		t.Fatalf("VerifyPRLinkToken() accepted legacy AGS audience")
	}
}

func TestVerifyPRLinkTokenRejectsRetiredWorkloadAssertionShape(t *testing.T) {
	secret := "test-secret"
	projection := New(Config{Enabled: true, LinkTokenSecret: secret})
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"aud": "external-pr-link", "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
		"purpose": "external_pr_link", "source": "task_token",
	})
	token.Header["kid"] = "multica-workload-assertion-v1"
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projection.VerifyPRLinkToken(signed); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("retired workload assertion err=%v", err)
	}
}

func TestRegisterPullRequestLinkPostsProviderNeutralEndpoint(t *testing.T) {
	var gotPath string
	var gotAuth string
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	projection := New(Config{Enabled: true, ServerURL: server.URL, ServiceToken: "service-secret"})
	if err := projection.RegisterPullRequestLink(context.Background(), ExternalPRLinkRequest{
		IssueID:                 "issue-id",
		WorkspaceID:             "workspace-id",
		Workspace:               "workspace-alpha",
		IssueKey:                "HUM-42",
		ExternalRepo:            "operator/demo",
		ExternalNumber:          7,
		ExternalURL:             "http://ags.local/operator/demo/pull/7",
		MergeProvider:           "forgejo",
		MergeRepo:               "mirror/demo",
		MergeNumber:             9,
		TargetInstance:          "primary-b",
		CanonicalRepositoryID:   "sha256:" + strings.Repeat("a", 64),
		CanonicalRepository:     "operator/demo",
		ProviderBindingID:       "sha256:" + strings.Repeat("b", 64),
		ProviderBindingRevision: "sha256:" + strings.Repeat("c", 64),
		ProviderRepository:      "mirror/demo",
		ExpectedHeadSHA:         strings.Repeat("1", 40),
		ExpectedBaseSHA:         strings.Repeat("2", 40),
		BaseRef:                 "main",
		DelegatedMergeMethod:    "rebase",
		ProjectionFactsRevision: "sha256:" + strings.Repeat("d", 64),
		CompletionIntent:        true,
		LinkConfidence:          "authoritative",
		State:                   "open",
	}); err != nil {
		t.Fatalf("RegisterPullRequestLink() error = %v", err)
	}
	if gotPath != "/api/integrations/external-pr/links" {
		t.Fatalf("path = %q, want provider-neutral endpoint", gotPath)
	}
	if gotAuth != "Bearer service-secret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if got["provider"] != "ags" || got["external_repo"] != "operator/demo" || got["external_number"].(float64) != 7 ||
		got["target_instance"] != "primary-b" || got["canonical_repository_id"] != "sha256:"+strings.Repeat("a", 64) ||
		got["provider_binding_revision"] != "sha256:"+strings.Repeat("c", 64) || got["delegated_merge_method"] != "rebase" {
		t.Fatalf("request body = %#v", got)
	}
}

func signTestPRLinkToken(t *testing.T, secret, audience string) string {
	t.Helper()
	now := time.Now()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"aud":          audience,
		"iat":          now.Unix(),
		"exp":          now.Add(time.Minute).Unix(),
		"source":       "task_token",
		"workspace":    "workspace-alpha",
		"workspace_id": "workspace-id",
		"issue_id":     "issue-id",
		"issue_key":    "HUM-42",
		"issue_url":    "https://multica.ai/workspace-alpha/issues/HUM-42",
		"task_id":      "task-id",
		"agent_id":     "agent-id",
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return token
}

func TestProjectWritesMetadataOnlyEvenWhenLegacyCommentEnabled(t *testing.T) {
	capture, command := fakeMulticaCommand(t)
	projection := New(Config{
		Enabled:          true,
		Command:          command,
		Profile:          "test-profile",
		Workspace:        "workspace-alpha",
		Comment:          true,
		SetStatusOnMerge: true,
	})

	err := projection.Project(context.Background(), Request{
		IssueKey:                 "HUM-34",
		RepoFullName:             "example-owner/forgejo-actions-demo",
		AGSPRNumber:              11,
		AGSPRURL:                 "http://primary-b:6666/example-owner/forgejo-actions-demo/pull/11",
		HeadBranch:               "agent/hum-34-team-e2e",
		HeadSHA:                  "e57dc23d477801974fd0083e38c279523ad76b19",
		ForgejoNumber:            14,
		ForgejoURL:               "http://10.0.0.3:5555/example-owner/forgejo-actions-demo/pulls/14",
		GitLabProject:            "example-human/forgejo-actions-demo",
		GitLabMRNumber:           7,
		GitLabMRURL:              "http://gitlab.example.test/example-human/forgejo-actions-demo/-/merge_requests/7",
		GitHubRepo:               "example-org/project-kit",
		GitHubPRNumber:           8,
		GitHubPRURL:              "https://github.com/example-org/project-kit/pull/8",
		CIState:                  "passed",
		CIRunURL:                 "http://forgejo.local/actions/runs/27",
		MergeState:               "merged",
		MergedSHA:                "6aa09f4d51dc832544f1553ec139402b19b2a06e",
		MergedAt:                 "2026-06-07T23:16:43+08:00",
		ExternalPRProvider:       "ags",
		ExternalPRLinkConfidence: "authoritative",
		ExternalPRLinkSource:     "task_token",
		AllowStatusSet:           true,
	})
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}

	log := readCapture(t, capture)
	mustContain(t, log, "--profile test-profile workspace switch workspace-alpha")
	mustContain(t, log, "issue metadata set HUM-34 --key ags_pr_number --value 11 --type string --output json")
	mustContain(t, log, "issue metadata set HUM-34 --key forgejo_pr_number --value 14 --type string --output json")
	mustContain(t, log, "issue metadata set HUM-34 --key external_pr_link --value ags:authoritative:task_token --type string --output json")
	if strings.Contains(log, "--key external_pr_completion_policy") {
		t.Fatalf("Project() overwrote Issue-owned external PR completion policy; log:\n%s", log)
	}
	if strings.Contains(log, "--key external_pr_number") || strings.Contains(log, "--key external_pr_merge_url") {
		t.Fatalf("Project() wrote expanded external_pr metadata instead of compact keys; log:\n%s", log)
	}
	mustContain(t, log, "issue metadata set HUM-34 --key gitlab_mr_number --value 7 --type string --output json")
	mustContain(t, log, "issue metadata set HUM-34 --key gitlab_mr_url --value http://gitlab.example.test/example-human/forgejo-actions-demo/-/merge_requests/7 --type string --output json")
	mustContain(t, log, "issue metadata set HUM-34 --key github_pr_number --value 8 --type string --output json")
	mustContain(t, log, "issue metadata set HUM-34 --key github_pr_url --value https://github.com/example-org/project-kit/pull/8 --type string --output json")
	mustContain(t, log, "issue metadata set HUM-34 --key ci_state --value passed --type string --output json")
	mustContain(t, log, "issue metadata set HUM-34 --key ci_run_url --value http://forgejo.local/actions/runs/27 --type string --output json")
	mustContain(t, log, "issue metadata set HUM-34 --key multica_issue_url --value https://multica.ai/workspace-alpha/issues/HUM-34 --type string --output json")
	if strings.Contains(log, "issue status HUM-34 done --output json") {
		t.Fatalf("Project() used deprecated direct status completion; log:\n%s", log)
	}
	if strings.Contains(log, "issue comment add") {
		t.Fatalf("Project() added a Multica issue comment from projection bookkeeping; log:\n%s", log)
	}
}

func TestProjectDoesNotSetDoneForOpenMergeState(t *testing.T) {
	capture, command := fakeMulticaCommand(t)
	projection := New(Config{
		Enabled:          true,
		Command:          command,
		Profile:          "test-profile",
		Comment:          false,
		SetStatusOnMerge: true,
	})

	err := projection.Project(context.Background(), Request{
		IssueKey:       "HUM-36",
		RepoFullName:   "example-owner/forgejo-actions-demo",
		AGSPRNumber:    12,
		ForgejoNumber:  15,
		CIState:        "pending",
		MergeState:     "open",
		AllowStatusSet: true,
	})
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}

	log := readCapture(t, capture)
	mustContain(t, log, "issue metadata set HUM-36 --key merge_state --value open --type string --output json")
	if strings.Contains(log, "issue status HUM-36 done") {
		t.Fatalf("Project() set done for open merge state; log:\n%s", log)
	}
}

func TestProjectOmitsProfileWhenConfiguredAsDash(t *testing.T) {
	capture, command := fakeMulticaCommand(t)
	projection := New(Config{Enabled: true, Command: command, Profile: "-", WorkspaceID: "ws-default"})

	err := projection.Project(context.Background(), Request{IssueKey: "HAK-12", RepoFullName: "example-owner/demo", AGSPRNumber: 1})
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}

	log := readCapture(t, capture)
	if strings.Contains(log, "--profile") {
		t.Fatalf("Project() passed --profile despite Profile=-; log:\n%s", log)
	}
	mustContain(t, log, "--workspace-id ws-default issue metadata set HAK-12 --key ags_pr_number --value 1 --type string --output json")
}

func TestProjectUsesWorkspaceIDWithoutSwitchingProfileWorkspace(t *testing.T) {
	capture, command := fakeMulticaCommand(t)
	projection := New(Config{
		Enabled:   true,
		Command:   command,
		Profile:   "test-profile",
		Workspace: "workspace-alpha",
		Repos: map[string]WorkspaceConfig{
			"example-owner/personal": {Workspace: "workspace-beta", WorkspaceID: "ws-workspace-beta"},
		},
	})

	err := projection.Project(context.Background(), Request{
		IssueKey:     "HAK-12",
		RepoFullName: "example-owner/personal",
		AGSPRNumber:  1,
		MergeState:   "open",
	})
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}

	log := readCapture(t, capture)
	if strings.Contains(log, "workspace switch") {
		t.Fatalf("Project() switched workspace despite workspace_id; log:\n%s", log)
	}
	mustContain(t, log, "--profile test-profile --workspace-id ws-workspace-beta issue metadata set HAK-12 --key ags_pr_number --value 1 --type string --output json")
	mustContain(t, log, "--profile test-profile --workspace-id ws-workspace-beta issue metadata set HAK-12 --key multica_workspace --value workspace-beta --type string --output json")
	mustContain(t, log, "--profile test-profile --workspace-id ws-workspace-beta issue metadata set HAK-12 --key multica_workspace_id --value ws-workspace-beta --type string --output json")
	mustContain(t, log, "--profile test-profile --workspace-id ws-workspace-beta issue metadata set HAK-12 --key multica_issue_ref --value workspace-beta/HAK-12 --type string --output json")
	mustContain(t, log, "--profile test-profile --workspace-id ws-workspace-beta issue metadata set HAK-12 --key multica_issue_url --value https://multica.ai/workspace-beta/issues/HAK-12 --type string --output json")
}

func TestProjectMarksMissingCIAsProjectionVerifiedWhenPolicyIsNotRequired(t *testing.T) {
	capture, command := fakeMulticaCommand(t)
	projection := New(Config{Enabled: true, Command: command, Profile: "test-profile", WorkspaceID: "ws-default"})

	err := projection.Project(context.Background(), Request{
		IssueKey:      "MINI-120",
		RepoFullName:  "operator/agent-om",
		AGSPRNumber:   1,
		ForgejoNumber: 1,
		CIState:       "not_configured",
		MergeState:    "open",
	})
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}

	log := readCapture(t, capture)
	mustContain(t, log, "issue metadata set MINI-120 --key ci_state --value not_configured --type string --output json")
	mustContain(t, log, "issue metadata set MINI-120 --key ci_policy --value absent_allowed --type string --output json")
	mustContain(t, log, "issue metadata set MINI-120 --key pipeline_status --value projection_verified --type string --output json")
	if strings.Contains(log, "--key pipeline_status --value blocked") || strings.Contains(log, "--key pipeline_status --value ci_blocked") {
		t.Fatalf("Project() marked missing optional CI as blocked; log:\n%s", log)
	}
}

func TestProjectBlocksMissingCIOnlyWhenPolicyRequiresCI(t *testing.T) {
	capture, command := fakeMulticaCommand(t)
	projection := New(Config{Enabled: true, Command: command, Profile: "test-profile", WorkspaceID: "ws-default"})

	err := projection.Project(context.Background(), Request{
		IssueKey:      "MINI-121",
		RepoFullName:  "operator/strict-ci",
		AGSPRNumber:   2,
		ForgejoNumber: 2,
		CIState:       "no_run",
		CIPolicy:      "required",
		MergeState:    "open",
	})
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}

	log := readCapture(t, capture)
	mustContain(t, log, "issue metadata set MINI-121 --key ci_state --value no_run --type string --output json")
	mustContain(t, log, "issue metadata set MINI-121 --key ci_policy --value required --type string --output json")
	mustContain(t, log, "issue metadata set MINI-121 --key pipeline_status --value ci_blocked --type string --output json")
}

func TestProjectMarksFailedCIAsBlocked(t *testing.T) {
	capture, command := fakeMulticaCommand(t)
	projection := New(Config{Enabled: true, Command: command, Profile: "test-profile", WorkspaceID: "ws-default"})

	err := projection.Project(context.Background(), Request{
		IssueKey:      "MINI-122",
		RepoFullName:  "operator/failing-ci",
		AGSPRNumber:   3,
		ForgejoNumber: 3,
		CIState:       "failed",
		MergeState:    "open",
	})
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}

	log := readCapture(t, capture)
	mustContain(t, log, "issue metadata set MINI-122 --key ci_policy --value optional --type string --output json")
	mustContain(t, log, "issue metadata set MINI-122 --key pipeline_status --value ci_blocked --type string --output json")
}

func fakeMulticaCommand(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	capture := filepath.Join(dir, "calls.log")
	command := filepath.Join(dir, "multica")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$MULTICA_CAPTURE\"\n"
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake multica command: %v", err)
	}
	t.Setenv("MULTICA_CAPTURE", capture)
	return capture, command
}

func readCapture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	return string(data)
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("capture missing %q; log:\n%s", needle, haystack)
	}
}
