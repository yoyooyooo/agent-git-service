package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/db"
)

func TestForkExtensionAliasesShareAuthorizationWithoutRedirects(t *testing.T) {
	for _, canonicalOnly := range []bool{false, true} {
		name := "compatible"
		if canonicalOnly {
			name = "canonical-only"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			srv, err := New(config.Config{DBdsn: "file:" + filepath.Join(root, "test.db"), GitRepoDir: filepath.Join(root, "repos"), BaseURL: "http://ags.test", ListenMode: "production", Environment: "production", DisableLegacyExtensionAliases: canonicalOnly})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := srv.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			})
			user := db.User{Login: "compat-human", Type: db.TypeUser, UserKind: db.UserKindHuman, Status: db.UserStatusActive}
			if err := srv.deps.DB.Create(&user).Error; err != nil {
				t.Fatal(err)
			}
			if err := srv.deps.DB.Create(&db.Token{UserID: user.ID, Value: "compat-test-only"}).Error; err != nil {
				t.Fatal(err)
			}
			for _, prefix := range []string{"/api/ext/v1", "/api/v3"} {
				for _, authorized := range []bool{false, true} {
					req := httptest.NewRequest(http.MethodPost, "http://ags.test"+prefix+"/user/tokens", strings.NewReader(`{"name":"compat-created"}`))
					req.Header.Set("Content-Type", "application/json")
					if authorized {
						req.Header.Set("Authorization", "Bearer compat-test-only")
					}
					rr := httptest.NewRecorder()
					srv.Handler().ServeHTTP(rr, req)
					want := http.StatusUnauthorized
					if authorized {
						want = http.StatusCreated
					}
					if canonicalOnly && prefix == "/api/v3" {
						want = http.StatusNotFound
					}
					if rr.Code != want || rr.Header().Get("Location") != "" {
						t.Fatalf("%s auth=%v status=%d want=%d body=%s", prefix, authorized, rr.Code, want, rr.Body.String())
					}
				}
			}
			var count int64
			if err := srv.deps.DB.Model(&db.Token{}).Where("name = ?", "compat-created").Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			wantCount := int64(2)
			if canonicalOnly {
				wantCount = 1
			}
			if count != wantCount {
				t.Fatalf("effects=%d want=%d; alias must not replay", count, wantCount)
			}
		})
	}
}

func TestRetiredControlPlaneFailsBeforeDatabaseBootstrap(t *testing.T) {
	root := t.TempDir()
	if _, err := New(config.Config{DBdsn: "file:" + filepath.Join(root, "unused.db"), GitRepoDir: filepath.Join(root, "repos"), ControlPlaneDSN: "legacy-setting"}); err == nil || !strings.Contains(err.Error(), "refusing root database fallback") {
		t.Fatalf("retired setting not refused: %v", err)
	}
}
