package edge_test

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/edge"
	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

type shortDiscovery struct {
	inner   edge.ReadAuthority
	fetches atomic.Int32
}

func (a *shortDiscovery) PrepareRead(ctx context.Context, id, auth string, r edgeprotocol.PrepareRead) (edgeprotocol.ReadPlan, error) {
	p, err := a.inner.PrepareRead(ctx, id, auth, r)
	if err == nil && r.Phase == "discover" {
		p.ExpiresAt = time.Now().UTC().Add(time.Second)
	}
	if r.Phase == "fetch" {
		a.fetches.Add(1)
	}
	return p, err
}
func (a *shortDiscovery) RevalidateRead(ctx context.Context, id, auth string, p edgeprotocol.ReadPlan) error {
	return a.inner.RevalidateRead(ctx, id, auth, p)
}

type delayedMaterialization struct {
	inner edge.SnapshotReader
	after func()
}

func (s delayedMaterialization) EnsureSnapshot(ctx context.Context, p edgeprotocol.RepositorySnapshot) (edge.SnapshotView, error) {
	v, err := s.inner.EnsureSnapshot(ctx, p)
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		v.Release()
		return nil, ctx.Err()
	case <-time.After(1200 * time.Millisecond):
	}
	if s.after != nil {
		s.after()
	}
	return v, nil
}

func TestSlowColdSyncObtainsNewExactAdmissionWithoutExtendingOldPlan(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		name := "allowed"
		if revoke {
			name = "revoked_during_wait"
		}
		t.Run(name, func(t *testing.T) {
			f := newControlFixture(t)
			mirror := newReplicaMirror(t, f.peer)
			authority := &shortDiscovery{inner: f.peer}
			reader := delayedMaterialization{inner: mirror}
			if revoke {
				reader.after = func() {
					if err := f.svc.DB.Where("value = ?", f.token).Delete(&db.Token{}).Error; err != nil {
						t.Error(err)
					}
				}
			}
			runtime, err := edge.NewReadRuntime(edge.ReadRuntimeConfig{EdgeID: "edge-fixture-test", Bindings: []edge.ReadBinding{{Repository: f.repo.FullName, Identity: f.identity}}, Concurrency: 2, RecentViews: 4, RecentWindow: time.Minute}, authority, reader)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("GET", "http://mini/"+f.repo.FullName+".git/info/refs?service=git-upload-pack", nil)
			request.Header.Set("Authorization", "Bearer "+f.token)
			response := httptest.NewRecorder()
			runtime.ServeHTTP(response, request)
			if !revoke && (response.Code != 200 || authority.fetches.Load() != 1) {
				t.Fatalf("read=%d exact readmission=%d", response.Code, authority.fetches.Load())
			}
			if revoke && response.Code == 200 {
				t.Fatal("renewed an expired plan after revocation")
			}
		})
	}
}
