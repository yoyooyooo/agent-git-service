package edge_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/replication"
	"github.com/ngaut/agent-git-service/internal/service"
)

type afterControlPrepare struct {
	replication.ReadAuthority
	after func() error
}

func (a afterControlPrepare) PrepareRead(ctx context.Context, edge string, req edgeprotocol.PrepareRead) (edgeprotocol.ReadPlan, error) {
	plan, err := a.ReadAuthority.PrepareRead(ctx, edge, req)
	if err != nil {
		return plan, err
	}
	return plan, a.after()
}

func TestPrimaryControlReauthenticatesAfterCapture(t *testing.T) {
	f := newControlFixture(t, func(a replication.ReadAuthority, svc *service.Service) replication.ReadAuthority {
		return afterControlPrepare{a, func() error {
			return svc.DB.Where("value = ?", "control-original-user-test-only").Delete(&db.Token{}).Error
		}}
	})
	_, err := f.peer.PrepareRead(context.Background(), "edge-fixture-test", "Bearer "+f.token, f.request())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("revocation after capture was ignored: %v", err)
	}
}

func TestPrimaryControlFreshNativePermissionAndDisabledRepository(t *testing.T) {
	f := newControlFixture(t)
	reader := db.User{Login: "edge-reader", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := f.svc.DB.Create(&reader).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DB.Create(&db.Token{UserID: reader.ID, Value: "reader-token-test"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AddCollaborator(f.ctx, f.repo.ID, reader.ID, "read"); err != nil {
		t.Fatal(err)
	}
	plan, err := f.peer.PrepareRead(context.Background(), "edge-fixture-test", "Bearer reader-token-test", f.request())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RemoveCollaborator(f.ctx, f.repo.ID, reader.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.peer.RevalidateRead(context.Background(), "edge-fixture-test", "Bearer reader-token-test", plan); err == nil {
		t.Fatal("removed native grant still accepted")
	}
	ownerPlan := f.prepare(t)
	if err := f.svc.DB.Model(&db.Repository{}).Where("id = ?", f.repo.ID).Update("disabled", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.peer.RevalidateRead(context.Background(), "edge-fixture-test", "Bearer "+f.token, ownerPlan); err == nil {
		t.Fatal("disabled repository still accepted")
	}
}

func TestPrimaryControlHonorsEffectiveGlobalGitReadPolicy(t *testing.T) {
	for _, content := range []string{"[uploadpack]\n\thideRefs = refs/heads/secret\n", "[http]\n\tuploadpack = false\n"} {
		t.Run(strings.Split(content, "\n")[0], func(t *testing.T) {
			f := newControlFixture(t)
			plan := f.prepare(t)
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			if err := f.peer.RevalidateRead(context.Background(), "edge-fixture-test", "Bearer "+f.token, plan); err == nil {
				t.Fatal("effective global policy was bypassed")
			}
			if _, err := f.peer.PrepareRead(context.Background(), "edge-fixture-test", "Bearer "+f.token, f.request()); err == nil {
				t.Fatal("new discovery bypassed effective global policy")
			}
		})
	}
}
