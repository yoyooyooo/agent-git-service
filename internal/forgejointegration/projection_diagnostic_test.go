package forgejointegration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ngaut/agent-git-service/internal/attribution"
	"github.com/ngaut/agent-git-service/internal/gittransport"
)

func TestProjectionDiagnosticTypedHTTPNoBodyAndOldAPI(t *testing.T) {
	const secret = "synthetic-private-sentinel"
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
			t.Error("unexpected write")
		}
		w.WriteHeader(403)
		fmt.Fprint(w, secret+" Authorization: secret header /private/secret-path \u202e\x00")
	}))
	defer server.Close()
	c := NewHTTPClient(server.URL, "fixture-token")
	_, err := c.ListPullRequests(context.Background(), "fixture", "repo", "all")
	var typed *ProviderHTTPError
	if !errors.As(err, &typed) || typed.Status != 403 || typed.Method != "GET" || typed.RouteKind != "pull_request" {
		t.Fatalf("lost HTTP evidence: %v", err)
	}
	pe := ClassifyProjectionError("refs/heads/topic", strings.Repeat("a", 40), fmt.Errorf("caller: %w", err))
	if pe.Type != ProjectionFailurePermissionDenied || pe.Diagnostic == nil || pe.Diagnostic.HTTPStatus == nil || *pe.Diagnostic.HTTPStatus != 403 {
		t.Fatalf("wrong classification: %+v", pe)
	}
	b, _ := json.Marshal(pe)
	if strings.Contains(string(b)+err.Error(), secret) || writes.Load() != 0 {
		t.Fatal("unsafe output or effect")
	}
	// Git stderr 403 is a pattern, never a fabricated measured HTTP status.
	legacy := ClassifyProjectionError("refs/heads/topic", "", errors.New("updates were rejected: 403 forbidden "+secret))
	if legacy.Type != ProjectionFailurePermissionDenied || legacy.Diagnostic.HTTPStatus != nil || strings.Contains(legacy.Error(), secret) {
		t.Fatal("legacy compatibility unsafe")
	}
}

func TestProjectionDiagnosticActualPrincipalReadOnly(t *testing.T) {
	var writes, reads atomic.Int32
	var user atomic.Value
	user.Store(`{"id":73,"login":"actual-owner"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
			t.Error("diagnostics wrote provider")
		}
		reads.Add(1)
		if r.Header.Get("Authorization") != "token selected-token" || r.Header.Get("Sudo") != "" {
			t.Error("wrong credential or sudo")
		}
		fmt.Fprint(w, user.Load().(string))
	}))
	defer server.Close()
	i := New(Config{BaseURL: server.URL, Token: "selected-token", TokenOwner: "cached-bot", IntegrationBot: "configured-bot"}, nil, nil)
	remote, _ := url.Parse(server.URL + "/fixture/repo.git")
	remote.User = url.UserPassword("x-access-token", "selected-token")
	req := PushRequest{RemoteURL: remote.String(), BranchName: "topic"}
	ctx := attribution.WithActor(context.Background(), attribution.Actor{UserID: 4, Login: "actor", RequestID: "fixture-request"})
	p := i.pushPrincipal(ctx, req)
	if p.Status != "verified" || p.Login == nil || *p.Login != "actual-owner" || p.ID == nil || *p.ID != 73 {
		t.Fatalf("guessed identity: %+v", p)
	}
	d := projectionDiagnostic(ctx, req, p, gittransport.SafeError(errors.New("connection refused synthetic-secret")))
	b, _ := json.Marshal(d)
	for _, bad := range []string{"selected-token", "cached-bot", "configured-bot", "synthetic-secret"} {
		if strings.Contains(string(b), bad) {
			t.Fatal("secret/guessed identity escaped")
		}
	}
	if d.ActorLogin != "actor" || d.EffectStatus != "unknown" {
		t.Fatal("actor/principal or effect confused")
	}
	for _, invalid := range []string{`{`, `{"id":0,"login":"cached-bot"}`, `{"id":1,"login":"evil\u001bcontrol"}`} {
		user.Store(invalid)
		if got := i.pushPrincipal(ctx, req); got.Login != nil || got.ID != nil || got.Status != "unknown" {
			t.Fatal("failed probe guessed identity")
		}
	}
	before := reads.Load()
	// Local, arbitrary SSH, injected pusher, and changed selected token cannot be
	// attested by the HTTP admin identity and must not trigger any fallback.
	req.RemoteURL = "ssh://git@example.test/fixture/repo.git"
	if i.pushPrincipal(ctx, req).Status != "unknown" || reads.Load() != before {
		t.Fatal("SSH confused with HTTP")
	}
	req.RemoteURL = remote.String()
	i.cfg.Token = "rotated-token"
	if i.pushPrincipal(ctx, req).Status != "unknown" || reads.Load() != before {
		t.Fatal("rotation reused old proof")
	}
	if writes.Load() != 0 {
		t.Fatal("provider writes")
	}
}

func TestProjectionDiagnosticSSHBindingDoesNotProvision(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
			t.Error("diagnostic key/ACL write")
			w.WriteHeader(500)
			return
		}
		switch r.URL.Path {
		case "/api/v1/users/ssh-owner/keys":
			fmt.Fprint(w, `[{"id":5,"key":"ssh-ed25519 fixture-public","title":"ags-managed:fixture:actor"}]`)
		case "/api/v1/users/ssh-owner":
			fmt.Fprint(w, `{"id":91,"login":"ssh-owner"}`)
		default:
			t.Error("unexpected HTTP user probe")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := NewHTTPClient(server.URL, "fixture-admin")
	k := managedKey{Instance: "fixture", Identity: "actor", Login: "ssh-owner", Public: "ssh-ed25519 fixture-public"}
	p := selectedSSHPrincipal(context.Background(), c, k)
	if p.Login == nil || *p.Login != "ssh-owner" || p.Source != "selected_key_binding" || p.ID == nil || *p.ID != 91 {
		t.Fatal("not exact selected key owner")
	}
	k.Public = "ssh-ed25519 different-key"
	p = selectedSSHPrincipal(context.Background(), c, k)
	if p.Status != "unknown" || p.Login != nil || writes.Load() != 0 {
		t.Fatal("unknown key was provisioned or attested")
	}
}

func TestProjectionDiagnosticImmediateLogAndNoReplay(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(previous)
	var pushes atomic.Int32
	i := New(Config{TokenOwner: "must-not-be-principal"}, nil, func(context.Context, PushRequest) error {
		pushes.Add(1)
		return gittransport.SafeError(errors.New("could not resolve host sentinel-token Authorization: sentinel-header /private/sentinel-path sentinel-body"))
	})
	ctx := attribution.WithActor(context.Background(), attribution.Actor{Login: "actor", UserID: 9, RequestID: "fixture-native-request"})
	ctx = WithProjectionDiagnosticLink(ctx, DiagnosticLink{JobID: 42, Attempt: 3, Phase: "mirroring_branch"})
	err := i.attributedPush(ctx, PushRequest{RemoteURL: "https://example.test/repo.git", BranchName: "topic"}, func(ctx context.Context, req PushRequest) error {
		pushes.Add(1)
		return gittransport.SafeError(errors.New("connection reset sentinel-token sentinel-header sentinel-path sentinel-body"))
	})
	pe := ClassifyProjectionError("refs/heads/topic", strings.Repeat("a", 40), err)
	b, _ := json.Marshal(pe)
	outputs := buf.String() + string(b) + err.Error()
	for _, secret := range []string{"sentinel-token", "sentinel-header", "sentinel-path", "sentinel-body", "must-not-be-principal"} {
		if strings.Contains(outputs, secret) {
			t.Fatal("secret escaped output")
		}
	}
	if pushes.Load() != 1 || !strings.Contains(buf.String(), `"authenticated_login":null`) || !strings.Contains(buf.String(), `"job_id":42`) || !strings.Contains(buf.String(), `"attempt":3`) {
		t.Fatal("replay/identity/correlation regression", buf.String())
	}
	// JSONL/DTO marshal contain only the same fixed evidence; arbitrary context is rejected.
	d := projectionDiagnostic(context.Background(), PushRequest{BranchName: "bad\x1bref"}, unknownPrincipal(), err)
	if d.Ref != "" || d.RetryAdvice != "inspect_before_new_plan" {
		t.Fatal("unsafe ref or retry authority")
	}
}

func TestProjectionDiagnosticProbeDenialsAndRedirect(t *testing.T) {
	var probes atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { probes.Add(1); t.Error("identity redirect followed") }))
	defer trap.Close()
	for _, tc := range []struct {
		status int
		body   string
	}{
		{401, `secret`}, {403, `secret`}, {404, `secret`}, {200, `invalid json`},
		{200, `{"id":17,"login":"actual-owner"}`}, {302, `redirect`},
	} {
		t.Run(fmt.Sprint(tc.status, tc.body), func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/user" {
					writes.Add(1)
					t.Error("probe is not GET user")
				}
				if tc.status == 302 {
					w.Header().Set("Location", trap.URL+"/user")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			p := NewHTTPClient(server.URL, "fixture-token").readPrincipal(context.Background(), "")
			if tc.status == 200 && strings.Contains(tc.body, "actual-owner") {
				if p.Login == nil || *p.Login != "actual-owner" {
					t.Fatal("valid probe failed")
				}
			} else if p.Status != "unknown" || p.Login != nil || p.ID != nil {
				t.Fatal("denial fabricated identity")
			}
			if writes.Load() != 0 || probes.Load() != 0 {
				t.Fatal("probe side effect")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if NewHTTPClient(server.URL, "fixture-token").readPrincipal(ctx, "").Status != "unknown" {
				t.Fatal("canceled probe attested")
			}
		})
	}
}

func TestProjectionDiagnosticHTTPMethodFailuresStayTyped(t *testing.T) {
	const sentinel = "fixture-http-body-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403); fmt.Fprint(w, sentinel) }))
	defer server.Close()
	c := NewHTTPClient(server.URL, "fixture-token")
	ctx := context.Background()
	for _, run := range []func() error{
		func() error { _, _, e := c.GetRepository(ctx, "fixture", "repo"); return e },
		func() error { return c.EnsureRepository(ctx, "fixture", "repo", true) },
		func() error { _, _, e := c.GetPullRequest(ctx, "fixture", "repo", 1); return e },
		func() error {
			_, e := c.EnsurePullRequest(ctx, PullRequestRequest{Owner: "fixture", Repo: "repo", Head: "topic", Base: "main"})
			return e
		},
		func() error { _, e := c.EditPullRequestBase(ctx, "fixture", "repo", 1, "main"); return e },
		func() error { _, e := c.UpdatePullRequestState(ctx, "fixture", "repo", 1, "closed"); return e },
		func() error { return c.MergePullRequest(ctx, "fixture", "repo", 1, PullRequestMergeRequest{}) },
		func() error { _, e := c.ListIssueLabels(ctx, "fixture", "repo", 1); return e },
		func() error { return c.AddIssueLabels(ctx, "fixture", "repo", 1, []string{"fixture"}) },
		func() error { _, _, e := c.repoLabelID(ctx, "fixture", "repo", "fixture"); return e },
		func() error { _, e := c.CollaboratorPermission(ctx, "fixture", "repo", "fixture"); return e },
		func() error { return c.CreateIssueComment(ctx, "fixture", "repo", 1, "fixture") },
		func() error { _, e := c.ListIssueComments(ctx, "fixture", "repo", 1, 1, 1); return e },
		func() error { _, _, e := c.ListWorkflowRunsPage(ctx, "fixture", "repo", 1, 1); return e },
		func() error { _, e := c.GetWorkflowRunLogs(ctx, "fixture", "repo", 1); return e },
		func() error {
			_, e := c.InspectRepositoryAuthority(ctx, "fixture", "repo", "main", "", "fixture")
			return e
		},
		func() error { return c.authorityRequest(ctx, "PATCH", "/api/v1/repos/fixture/repo", nil) },
	} {
		err := run()
		var typed *ProviderHTTPError
		if !errors.As(err, &typed) || typed.Status != 403 || strings.Contains(err.Error(), sentinel) {
			t.Fatalf("unsafe/untyped HTTP error: %v", err)
		}
	}
}
