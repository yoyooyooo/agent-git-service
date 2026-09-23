package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/snapshotstore"
	"github.com/ngaut/agent-git-service/internal/tenant"
)

// PrimaryReadAuthority coordinates real primary Git with original-user
// authorization. HTTP/token resolution and peer authentication are adapters;
// neither a descriptor nor a peer's copy permission authorizes the user.
type PrimaryReadAuthority struct {
	svc       *Service
	retained  *snapshotstore.Store
	authority string
	policy    string
	prefixes  []string
	ttl       time.Duration
}

func NewPrimaryReadAuthority(svc *Service, retained *snapshotstore.Store, authority, policy string, prefixes []string, ttl time.Duration) (*PrimaryReadAuthority, error) {
	identity := edgeprotocol.RepositoryIdentity{AuthorityID: authority, StoreID: policy, RepositoryID: 1, Kind: "repo"}
	if svc == nil || svc.DB == nil || svc.Git == nil || retained == nil || identity.Validate() != nil || ttl < time.Second || ttl > 2*time.Minute || len(prefixes) == 0 {
		return nil, errors.New("invalid primary read authority configuration")
	}
	for _, prefix := range prefixes {
		if !strings.HasPrefix(prefix, "refs/") || !strings.HasSuffix(prefix, "/") || strings.ContainsAny(prefix, "\\\x00\r\n ~^:?*[") || strings.Contains(prefix, "..") {
			return nil, errors.New("invalid explicit export prefix")
		}
	}
	// Bind the actual prefix policy, not only an operator-supplied label.
	// A config change must invalidate prior plans even if that label is reused.
	prefixes = append([]string{}, prefixes...)
	sort.Strings(prefixes)
	policyBytes, _ := json.Marshal(struct {
		Label    string
		Prefixes []string
	}{policy, prefixes})
	digest := sha256.Sum256(policyBytes)
	return &PrimaryReadAuthority{svc: svc, retained: retained, authority: authority, policy: "policy-" + hex.EncodeToString(digest[:]), prefixes: prefixes, ttl: ttl}, nil
}

// authorize always uses fresh repository/permission caches. Replication's first
// supported mode is the single business DB. It cannot fall back to the root DB
// when a tenant context is present. The transport adapter resolves tokens again
// after a long capture and for every separate revalidation request.
func (a *PrimaryReadAuthority) authorize(ctx context.Context, locator string, expected edgeprotocol.RepositoryIdentity) (db.Repository, string, error) {
	if expected.AuthorityID != a.authority || expected.Kind != "repo" { // wiki live routing is not admitted yet
		return db.Repository{}, "", edgeprotocol.ErrReadDenied
	}
	if _, ok := tenant.FromContext(ctx); ok {
		return db.Repository{}, "", edgeprotocol.ErrReadDenied
	}
	if tenantDB, ok := DBFromContext(ctx); ok && tenantDB != nil && tenantDB != a.svc.DB {
		return db.Repository{}, "", edgeprotocol.ErrReadDenied
	}
	ctx = ContextWithRepoCache(ctx)
	repo, err := a.svc.AuthorizeGitTransport(ctx, locator, "git-upload-pack")
	if err != nil || repo.Disabled {
		return db.Repository{}, "", edgeprotocol.ErrReadDenied
	}
	identity, err := replicationIdentity(repo, a.authority, expected.Kind)
	if err != nil || identity != expected {
		return db.Repository{}, "", edgeprotocol.ErrReadDenied
	}
	path, err := a.svc.Git.GetRepoPath(ctx, repo.FullName)
	if err != nil || !a.svc.Git.Exists(ctx, repo.FullName) {
		return db.Repository{}, "", edgeprotocol.ErrReadUnavailable
	}
	return repo, path, nil
}

func (a *PrimaryReadAuthority) PrepareRead(ctx context.Context, edgeID string, request edgeprotocol.PrepareRead) (edgeprotocol.ReadPlan, error) {
	if request.Validate() != nil {
		return edgeprotocol.ReadPlan{}, edgeprotocol.ErrReadDenied
	}
	// Cheap denial BEFORE acquiring the exclusive capture barrier.
	if _, _, err := a.authorize(ctx, request.Repository, request.Identity); err != nil {
		return edgeprotocol.ReadPlan{}, err
	}
	descriptor, err := a.prepareSnapshot(ctx, request)
	if err != nil {
		if errors.Is(err, edgeprotocol.ErrReadDenied) {
			return edgeprotocol.ReadPlan{}, err
		}
		return edgeprotocol.ReadPlan{}, edgeprotocol.ErrReadUnavailable
	}
	now := time.Now().UTC()
	plan := edgeprotocol.ReadPlan{Version: edgeprotocol.ReadPlanVersion, RequestID: request.RequestID, EdgeID: edgeID, Operation: "git.read", Phase: request.Phase, Snapshot: descriptor, AuthorizedAt: now, ExpiresAt: now.Add(a.ttl)}
	if err := plan.ValidateFor(edgeID, now); err != nil {
		return edgeprotocol.ReadPlan{}, edgeprotocol.ErrReadDenied
	}
	return plan, nil
}

// observeReadSource is called only under the owning Store's capture barrier.
// It takes no snapshot-store mutex and never packs or verifies retained objects.
func (a *PrimaryReadAuthority) observeReadSource(ctx context.Context, locator string, identity edgeprotocol.RepositoryIdentity) (string, edgeprotocol.Manifest, error) {
	_, source, err := a.authorize(ctx, locator, identity)
	if err != nil {
		return "", edgeprotocol.Manifest{}, err
	}
	if err := checkPrimaryExportPolicy(ctx, source); err != nil {
		return "", edgeprotocol.Manifest{}, err
	}
	manifest, err := snapshotstore.ObserveManifest(ctx, source, identity, a.policy, a.prefixes)
	return source, manifest, err
}

func (a *PrimaryReadAuthority) prepareSnapshot(ctx context.Context, request edgeprotocol.PrepareRead) (edgeprotocol.RepositorySnapshot, error) {
	return a.prepareObservedSnapshot(ctx, request, a.observeReadSource)
}

// prepareObservedSnapshot shares materialization only; observers own distinct
// user-read vs registered-node-copy authority. A warm result is never a ReadPlan.
func (a *PrimaryReadAuthority) prepareObservedSnapshot(ctx context.Context, request edgeprotocol.PrepareRead, observe func(context.Context, string, edgeprotocol.RepositoryIdentity) (string, edgeprotocol.Manifest, error)) (result edgeprotocol.RepositorySnapshot, resultErr error) {
	var observed edgeprotocol.Manifest
	err := a.svc.Git.WithSnapshotCapture(ctx, func(ctx context.Context) error {
		_, manifest, err := observe(ctx, request.Repository, request.Identity)
		observed = manifest
		return err
	})
	if err != nil {
		return edgeprotocol.RepositorySnapshot{}, err
	}
	descriptor := observed.Snapshot
	if request.Phase == "fetch" {
		descriptor = *request.Snapshot
		if descriptor.ExportPolicyRevision != a.policy {
			return descriptor, edgeprotocol.ErrReadDenied
		}
	}
	// Acquire can verify persisted content after restart: never perform it
	// while primary writes are paused. A retained fetch must not be substituted.
	if view, err := a.retained.Acquire(ctx, descriptor); err == nil {
		view.Release()
		return descriptor, nil
	} else if request.Phase == "fetch" || !errors.Is(err, snapshotstore.ErrMissing) {
		return descriptor, err
	}
	capture, err := a.retained.NewCapture(ctx)
	if err != nil {
		return descriptor, err
	}
	defer func() {
		if err := capture.Close(); err != nil && resultErr == nil {
			resultErr = err
		}
	}()
	err = a.svc.Git.WithSnapshotCapture(ctx, func(ctx context.Context) error {
		// Git may have changed since the cache lookup. Reobserve and pin in
		// one barrier, rather than linking objects for a stale ref scan.
		source, manifest, err := observe(ctx, request.Repository, request.Identity)
		if err != nil {
			return err
		}
		return capture.PinSource(ctx, source, manifest)
	})
	if err != nil {
		return descriptor, err
	}
	descriptor = capture.Manifest().Snapshot
	// Primary writes, including source deletion, can proceed from here. The
	// private pin owns object links; the HTTP adapter reauthenticates after us.
	if view, err := a.retained.Acquire(ctx, descriptor); err == nil {
		view.Release()
		return descriptor, nil
	} else if !errors.Is(err, snapshotstore.ErrMissing) {
		return descriptor, err
	}
	return descriptor, capture.Retain(ctx)
}

// RevalidateRead is a new use-time check, NOT a bearer exchange. The adapter
// must authenticate the original credential again. Current identity/permission,
// disabled/delete/recreate state, retained objects and policy must still agree.
// It deliberately does not extend the supplied plan's expiration.
func (a *PrimaryReadAuthority) RevalidateRead(ctx context.Context, edgeID string, plan edgeprotocol.ReadPlan) error {
	if _, ok := tenant.FromContext(ctx); ok {
		return edgeprotocol.ErrReadDenied
	}
	if tenantDB, ok := DBFromContext(ctx); ok && tenantDB != nil && tenantDB != a.svc.DB {
		return edgeprotocol.ErrReadDenied
	}
	if plan.ValidateFor(edgeID, time.Now().UTC()) != nil || plan.ExpiresAt.Sub(plan.AuthorizedAt) > a.ttl || plan.Snapshot.ExportPolicyRevision != a.policy {
		return edgeprotocol.ErrReadDenied
	}
	err := a.svc.Git.WithSnapshotCapture(ctx, func(ctx context.Context) error {
		var repo db.Repository
		if err := a.svc.DBForCtx(ctx).WithContext(ctx).Select("id", "full_name").First(&repo, plan.Snapshot.Identity.RepositoryID).Error; err != nil {
			return edgeprotocol.ErrReadDenied
		}
		_, source, err := a.authorize(ctx, repo.FullName, plan.Snapshot.Identity)
		if err != nil {
			return err
		}
		if err := checkPrimaryExportPolicy(ctx, source); err != nil {
			return edgeprotocol.ErrReadUnavailable
		}
		if _, err := snapshotstore.ObserveManifest(ctx, source, plan.Snapshot.Identity, a.policy, a.prefixes); err != nil {
			return edgeprotocol.ErrReadUnavailable
		}
		return nil
	})
	if err != nil {
		return err
	}
	view, err := a.retained.Acquire(ctx, plan.Snapshot)
	if err != nil {
		return edgeprotocol.ErrReadUnavailable
	}
	defer view.Release()
	if plan.ValidateFor(edgeID, time.Now().UTC()) != nil {
		return edgeprotocol.ErrReadDenied
	}
	return nil
}
