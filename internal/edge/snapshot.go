package edge

import (
	"context"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

// SnapshotReader is implemented by Mirror using authenticated export transfer,
// verified local publication and pinned views. ReadRuntime separately supplies
// per-request original-user authority, multi-RPC selection and revalidation.
// A materialized cache entry is never permission to serve a caller.
type SnapshotReader interface {
	// EnsureSnapshot returns only a fully materialized, verified, published
	// view matching required, never an empty initialization or stale mirror.
	// Callers for the same store may share synchronization; cancellation of
	// one waiter must not cancel work still needed by other waiters.
	EnsureSnapshot(ctx context.Context, required edgeprotocol.RepositorySnapshot) (SnapshotView, error)
}

// SnapshotView is an in-process pinned read lease, NOT a Git client session.
// ProjectRoot + Repository identify the trusted local path passed to
// gitbackend. Repository need not be the public owner/name: the cache is
// keyed by immutable authority/store identity. The files/refs/objects must
// remain readable and immutable until Release (which must be idempotent).
// No credentials, write capability, or caller-supplied path belongs here.
type SnapshotView interface {
	Snapshot() edgeprotocol.RepositorySnapshot
	Manifest() edgeprotocol.Manifest
	// ContainsObjects only checks this verified, self-contained local view.
	// Native Git still enforces its own want/reachability rules.
	ContainsObjects(context.Context, []string) (bool, error)
	ProjectRoot() string
	Repository() string
	Release()
}
