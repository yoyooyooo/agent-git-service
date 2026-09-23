package service

import (
	"context"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/snapshotstore"
	"github.com/ngaut/agent-git-service/internal/tenant"
)

// WarmSnapshot is called only behind exact mTLS peer-copy authorization. It
// creates no user identity, grant or read plan. Foreground reads still resolve
// the original user at every RPC and revalidate after any synchronization wait.
func (a *PrimaryReadAuthority) WarmSnapshot(ctx context.Context, identity edgeprotocol.RepositoryIdentity) (edgeprotocol.RepositorySnapshot, error) {
	if identity.Validate() != nil || identity.AuthorityID != a.authority || identity.Kind != "repo" {
		return edgeprotocol.RepositorySnapshot{}, edgeprotocol.ErrReadDenied
	}
	if _, ok := tenant.FromContext(ctx); ok {
		return edgeprotocol.RepositorySnapshot{}, edgeprotocol.ErrReadDenied
	}
	observe := func(ctx context.Context, _ string, expected edgeprotocol.RepositoryIdentity) (string, edgeprotocol.Manifest, error) {
		source, err := a.svc.ReplicationSourcePath(ctx, expected)
		if err != nil {
			return "", edgeprotocol.Manifest{}, edgeprotocol.ErrReadDenied
		}
		if err := checkPrimaryExportPolicy(ctx, source); err != nil {
			return "", edgeprotocol.Manifest{}, edgeprotocol.ErrReadUnavailable
		}
		manifest, err := snapshotstore.ObserveManifest(ctx, source, expected, a.policy, a.prefixes)
		return source, manifest, err
	}
	snapshot, err := a.prepareObservedSnapshot(ctx, edgeprotocol.PrepareRead{Identity: identity, Phase: "discover"}, observe)
	if err != nil {
		return edgeprotocol.RepositorySnapshot{}, err
	}
	// A source may have been deleted while packing outside the barrier. The
	// retained copy is not permission to keep advertising a nonexistent source.
	err = a.svc.Git.WithSnapshotCapture(ctx, func(ctx context.Context) error {
		_, _, err := observe(ctx, "", identity)
		return err
	})
	return snapshot, err
}
