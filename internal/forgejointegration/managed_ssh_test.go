package forgejointegration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ngaut/agent-git-service/internal/attribution"
)

func TestManagedSSHConcurrentReuseRecoveryAndRevocation(t *testing.T) {
	var mu sync.Mutex
	var keys []providerKey
	registrations, grants, pushes := 0, 0, 0
	permission := "none"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Sudo") != "" {
			t.Error("admin key lifecycle must not use Sudo")
		}
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/keys"):
			if keys == nil {
				w.Write([]byte("[]"))
			} else {
				json.NewEncoder(w).Encode(keys)
			}
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/keys"):
			var b struct{ Key, Title string }
			json.NewDecoder(r.Body).Decode(&b)
			registrations++
			keys = append(keys, providerKey{ID: int64(registrations), Key: b.Key, Title: b.Title})
			w.WriteHeader(201)
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/keys/"):
			keys = nil
			w.WriteHeader(204)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/permission"):
			fmt.Fprintf(w, `{"permission":%q}`, permission)
		case r.Method == "PUT" && strings.Contains(r.URL.Path, "/collaborators/"):
			grants++
			permission = "write"
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	cfg := Config{BaseURL: server.URL, Token: "synthetic", TokenFile: filepath.Join(t.TempDir(), "operator-token.txt"), ManagedSSH: ManagedSSHConfig{URL: "ssh://git@forgejo.example.test:2222", KnownHostsFile: "/fixture/known_hosts"}, RepoMap: map[string]RepoMapping{"ags/repo": {Owner: "team", Repo: "repo"}}, Attribution: attribution.Config{Instance: "test", Authorize: allowProjectionTestOperation, Principals: map[string]attribution.Principal{"alice": {Forgejo: "alice.remote"}}}}
	ctx := attribution.WithActor(context.Background(), attribution.Actor{UserID: 7, Login: "alice", Repo: "ags/repo"})
	var keyPath string
	push := func(_ context.Context, req PushRequest) error {
		mu.Lock()
		defer mu.Unlock()
		pushes++
		if req.SSH == nil || req.RemoteURL != "ssh://git@forgejo.example.test:2222/team/repo.git" || req.Refspec != "refs/heads/topic:refs/heads/topic" || req.ForceWithLeaseSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
			t.Error("credential selection changed exact push")
		}
		if keyPath != "" && keyPath != req.SSH.IdentityFile {
			t.Error("key rotated")
		}
		keyPath = req.SSH.IdentityFile
		info, err := os.Stat(keyPath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Error("private key permissions")
		}
		return nil
	}
	i := New(cfg, nil, push)
	req := PushRequest{Repo: TargetRepo{Owner: "team", Repo: "repo"}, Refspec: "refs/heads/topic:refs/heads/topic", ForceWithLeaseRef: "refs/heads/topic", ForceWithLeaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	var wg sync.WaitGroup
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := i.push(ctx, req); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if registrations != 1 || grants != 1 || pushes != 12 {
		t.Fatalf("registrations=%d grants=%d pushes=%d", registrations, grants, pushes)
	}
	original, _ := os.ReadFile(keyPath)
	keys = nil
	if err := i.push(ctx, req); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(keyPath)
	if string(current) != string(original) || registrations != 2 || len(keys) != 1 {
		t.Fatal("deleted public key did not recover idempotently")
	}

	sshFailure := errors.New("SSH transport failed")
	attempts := 0
	err := i.attributedPush(ctx, req, func(_ context.Context, r PushRequest) error {
		attempts++
		if r.SSH == nil {
			t.Fatal("failed push retried as bot")
		}
		return sshFailure
	})
	if !errors.Is(err, sshFailure) || errors.Is(err, ErrMappedPushNotAttempted) || attempts != 1 {
		t.Fatalf("SSH failure err=%v attempts=%d", err, attempts)
	}
	// Revocation immediately before the actual Git call cannot reuse a cached admission.
	checks := 0
	i.SetAttributionAuthorizer(func(context.Context, attribution.Actor, string, string, int) error {
		checks++
		if checks == 3 {
			return ErrAttributionAuthorizationRequired
		}
		return nil
	})
	err = i.attributedPush(ctx, req, func(context.Context, PushRequest) error { t.Fatal("revoked user reached push"); return nil })
	if !errors.Is(err, ErrAttributionAuthorizationRequired) || !errors.Is(err, ErrMappedPushNotAttempted) {
		t.Fatal("late revocation ignored", err)
	}
	other := cfg
	other.Attribution.Instance = "other-instance"
	other.Attribution.Principals = nil
	if err := ReconcileManagedSSH(ctx, other); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatal("other instance revoked this instance's key")
	}
	cfg.Attribution.Principals = nil
	if err := ReconcileManagedSSH(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatal("mapping removal retained public key")
	}
	if err := ReconcileManagedSSH(ctx, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestManagedSSHDenialAndEmptyMappingHaveNoCredentialEffects(t *testing.T) {
	for _, mode := range []string{"absent", "empty", "unmapped", "denied", "revoked_before_key", "provider_failure"} {
		t.Run(mode, func(t *testing.T) {
			calls, checks, pushes := 0, 0, 0
			denied := errors.New("AGS denied")
			root := t.TempDir()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
			defer server.Close()
			cfg := Config{RepoMap: map[string]RepoMapping{"ags/repo": {Owner: "team", Repo: "repo"}}, BaseURL: server.URL, TokenFile: filepath.Join(root, "operator-token.txt"), Attribution: attribution.Config{Instance: "test"}}
			if mode != "absent" {
				cfg.Attribution.Principals = map[string]attribution.Principal{}
			}
			if mode != "absent" && mode != "empty" {
				cfg.Attribution.Principals["alice"] = attribution.Principal{Forgejo: "alice.remote"}
			}
			cfg.Attribution.Authorize = func(context.Context, attribution.Actor, string, string, int) error {
				checks++
				if mode == "denied" || mode == "revoked_before_key" && checks == 2 {
					return denied
				}
				return nil
			}
			cfg.ManagedSSH = ManagedSSHConfig{URL: "ssh://git@forgejo.example.test:2222", KnownHostsFile: "/fixture/known_hosts"}
			i := New(cfg, nil, func(_ context.Context, req PushRequest) error {
				pushes++
				if req.RemoteURL != "http://bot.example.test/team/repo.git" {
					t.Error("base transport changed")
				}
				return nil
			})
			login := "alice"
			if mode == "unmapped" {
				login = "other"
			}
			ctx := attribution.WithActor(context.Background(), attribution.Actor{UserID: 7, Login: login, Repo: "ags/repo"})
			err := i.push(ctx, PushRequest{RemoteURL: "http://bot.example.test/team/repo.git", Repo: TargetRepo{Owner: "team", Repo: "repo"}})
			if mode == "denied" || mode == "revoked_before_key" {
				if !errors.Is(err, denied) || calls != 0 || pushes != 0 {
					t.Fatalf("err=%v calls=%d pushes=%d", err, calls, pushes)
				}
			} else if mode == "absent" || mode == "empty" || mode == "unmapped" {
				if err != nil || calls != 0 || pushes != 1 {
					t.Fatal("opt-out changed")
				}
			} else {
				if err == nil || pushes != 0 {
					t.Fatal("SSH failure fell back to bot")
				}
			}
		})
	}
}

func TestManagedSSHDenialCannotBecomeExactRefSuccess(t *testing.T) {
	denied := errors.New("revoked AGS admission")
	reads, pushes := 0, 0
	cfg := Config{Attribution: attribution.Config{Instance: "test", Principals: map[string]attribution.Principal{"alice": {Forgejo: "alice.remote"}}, Authorize: func(context.Context, attribution.Actor, string, string, int) error { return denied }}}
	i := NewWithGitCapabilities(cfg, nil, func(context.Context, PushRequest) error { pushes++; return nil }, func(context.Context, string, string, string) (string, error) {
		reads++
		return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
	})
	ctx := attribution.WithActor(context.Background(), attribution.Actor{UserID: 7, Login: "alice", Repo: "ags/repo"})
	err := i.pushAndVerify(ctx, PushRequest{Repo: TargetRepo{Owner: "team", Repo: "repo"}}, "refs/heads/topic", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if !errors.Is(err, denied) || !errors.Is(err, ErrMappedPushNotAttempted) || reads != 0 || pushes != 0 {
		t.Fatalf("err=%v reads=%d pushes=%d", err, reads, pushes)
	}
}
