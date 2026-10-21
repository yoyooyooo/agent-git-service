package githubintegration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Reproduces GitHub auto-closing a PR when its head disappears: an open-only
// lookup loses the terminal fact and AGS can never mark its projection closed.
func TestClosePullRequestForBranchObservesAlreadyClosed(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
			t.Errorf("unexpected write: %s", r.Method)
			return
		}
		if r.URL.Query().Get("state") != "all" {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprint(w, `[{"number":8,"state":"closed","merged":true}]`)
	}))
	defer server.Close()
	pr, handled, err := NewHTTPClient(server.URL, "synthetic").ClosePullRequestForBranch(context.Background(), "fixture/repo", "work", "main", "closed")
	if err != nil || !handled || pr.Number != 8 || writes != 0 {
		t.Fatalf("pr=%+v handled=%v writes=%d err=%v", pr, handled, writes, err)
	}
}

func TestObservePullRequestUsesRecordedNumberAnd404IsMissing(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/repos/fixture/repo/pulls/8" || r.URL.RawQuery != "" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		if requests == 1 {
			fmt.Fprint(w, `{"number":8,"state":"closed","merged":true}`)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewHTTPClient(server.URL, "synthetic")
	pr, found, err := client.ObservePullRequest(context.Background(), "fixture/repo", 8, "work", "main")
	if err != nil || !found || !pr.Merged {
		t.Fatalf("pr=%+v found=%v err=%v", pr, found, err)
	}
	_, found, err = client.ObservePullRequest(context.Background(), "fixture/repo", 8, "work", "main")
	if err != nil || found || requests != 2 {
		t.Fatalf("found=%v requests=%d err=%v", found, requests, err)
	}
}

func TestCloseShadowUnknownWriteOutcomeUsesReadbackWithoutReplay(t *testing.T) {
	closed := false
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/fixture/repo/pulls/8" {
			t.Errorf("unexpected path: %s", r.URL)
			w.WriteHeader(500)
			return
		}
		if r.Method == http.MethodPatch {
			closed = true
			writes++
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		state := "open"
		if closed {
			state = "closed"
		}
		fmt.Fprintf(w, `{"number":8,"state":%q}`, state)
	}))
	defer server.Close()
	integration := NewWithClient(Config{Enabled: true, Token: "synthetic", BaseURL: server.URL, Repos: map[string]RepoMapping{"fixture/ags": {Owner: "fixture", Repo: "repo"}}}, func(context.Context, string, string, string, string) error { t.Fatal("close pushed Git"); return nil }, NewHTTPClient(server.URL, "synthetic"))
	req := CloseShadowPullRequestRequest{RepoFullName: "fixture/ags", HeadBranch: "work", BaseBranch: "main", ExternalNumber: 8}
	if _, _, err := integration.CloseShadowPullRequest(context.Background(), req); err == nil {
		t.Fatal("expected lost PATCH response")
	}
	result, handled, err := integration.CloseShadowPullRequest(context.Background(), req)
	if err != nil || !handled || result.Number != 8 || writes != 1 {
		t.Fatalf("result=%+v handled=%v writes=%d err=%v", result, handled, writes, err)
	}
}

func TestEnsureObservesTerminalShadowWithoutPushOrCreate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("state") != "all" {
			t.Errorf("unexpected mutation/lookup: %s %s", r.Method, r.URL)
			w.WriteHeader(500)
			return
		}
		fmt.Fprint(w, `[{"number":8,"state":"closed"}]`)
	}))
	defer server.Close()
	integration := NewWithClient(Config{Enabled: true, Token: "synthetic", Repos: map[string]RepoMapping{"fixture/ags": {Owner: "fixture", Repo: "repo"}}}, func(context.Context, string, string, string, string) error {
		t.Fatal("terminal shadow pushed")
		return nil
	}, NewHTTPClient(server.URL, "synthetic"))
	result, handled, err := integration.EnsureShadowPullRequest(context.Background(), ShadowPullRequestRequest{RepoFullName: "fixture/ags", RepoPath: "/fixture.git", HeadBranch: "work", BaseBranch: "main", HeadSHA: "aaa"})
	if err != nil || !handled || result.Number != 8 || result.State != "closed" {
		t.Fatalf("result=%+v handled=%v err=%v", result, handled, err)
	}
}

func TestEnsurePinsObservedIdentityAcrossTerminalTransitionAndBranchReuse(t *testing.T) {
	for _, transition := range []bool{false, true} {
		t.Run(fmt.Sprint(transition), func(t *testing.T) {
			gets, patches, pushes := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/fixture/repo/pulls/8":
					gets++
					state := "open"
					if transition && gets > 1 {
						state = "closed"
					}
					fmt.Fprintf(w, `{"number":8,"state":%q,"title":"old"}`, state)
				case r.Method == http.MethodPatch && r.URL.Path == "/repos/fixture/repo/pulls/8":
					patches++
					fmt.Fprint(w, `{"number":8,"state":"open","title":"new"}`)
				default:
					// An unpinned branch lookup would pick competing PR #9,
					// or an open-only absence could authorize a replacement.
					t.Errorf("identity escaped: %s %s", r.Method, r.URL)
					fmt.Fprint(w, `[{"number":9,"state":"open"}]`)
				}
			}))
			defer server.Close()
			integration := NewWithClient(Config{Enabled: true, Token: "synthetic", Repos: map[string]RepoMapping{"fixture/ags": {Owner: "fixture", Repo: "repo"}}}, func(context.Context, string, string, string, string) error { pushes++; return nil }, NewHTTPClient(server.URL, "synthetic"))
			res, handled, err := integration.EnsureShadowPullRequest(context.Background(), ShadowPullRequestRequest{RepoFullName: "fixture/ags", RepoPath: "/fixture.git", HeadBranch: "work", BaseBranch: "main", HeadSHA: "aaa", Title: "new", ExternalNumber: 8})
			wantState, wantPatches := "open", 1
			if transition {
				wantState, wantPatches = "closed", 0
			}
			if err != nil || !handled || res.Number != 8 || res.State != wantState || patches != wantPatches || gets != 2 || pushes != 1 {
				t.Fatalf("res=%+v handled=%v err=%v gets=%d patches=%d pushes=%d", res, handled, err, gets, patches, pushes)
			}
		})
	}
}

func TestEnsureAllStateReadbackBeforeCreation(t *testing.T) {
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("state") != "all" {
			t.Errorf("unexpected creation/lookup: %s %s", r.Method, r.URL)
			w.WriteHeader(500)
			return
		}
		reads++
		if reads == 1 {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprint(w, `[{"number":8,"state":"closed"}]`)
	}))
	defer server.Close()
	integration := NewWithClient(Config{Enabled: true, Token: "synthetic", Repos: map[string]RepoMapping{"fixture/ags": {Owner: "fixture", Repo: "repo"}}}, func(context.Context, string, string, string, string) error { return nil }, NewHTTPClient(server.URL, "synthetic"))
	res, handled, err := integration.EnsureShadowPullRequest(context.Background(), ShadowPullRequestRequest{RepoFullName: "fixture/ags", RepoPath: "/fixture.git", HeadBranch: "work", BaseBranch: "main", HeadSHA: "aaa"})
	if err != nil || !handled || res.State != "closed" || res.Number != 8 || reads != 2 {
		t.Fatalf("res=%+v handled=%v err=%v reads=%d", res, handled, err, reads)
	}
}
