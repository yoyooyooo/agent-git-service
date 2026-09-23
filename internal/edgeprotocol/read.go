// Package edgeprotocol contains credential-free descriptors shared by the
// primary and Edge. The mTLS export data plane accepts canonical snapshot
// descriptors; a user-authorized ReadPlan HTTP surface is not wired yet.
// Validation establishes shape/binding ONLY, never user or peer authority,
// object completeness, snapshot retention, or freshness of primary facts.
package edgeprotocol

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ReadPlanVersion = "ags.edge.read-plan.v1"
	SnapshotVersion = "ags.edge.snapshot.v1"
)

// RepositoryIdentity survives a rename. StoreID must never be reused after
// delete/recreate; a wiki is a distinct store under the same RepositoryID.
// AuthorityID identifies the owning AGS, not a hostname or an Edge node.
type RepositoryIdentity struct {
	AuthorityID  string `json:"authority_id"`
	StoreID      string `json:"store_id"`
	RepositoryID uint64 `json:"repository_id"`
	Kind         string `json:"kind"` // repo or wiki
}

func (identity RepositoryIdentity) Validate() error {
	if !identifier(identity.AuthorityID) || !identifier(identity.StoreID) || identity.RepositoryID == 0 || (identity.Kind != "repo" && identity.Kind != "wiki") {
		return fmt.Errorf("invalid repository identity")
	}
	return nil
}

// Head preserves symbolic, detached and unborn HEAD without inventing a SHA.
type Head struct {
	SymbolicRef string `json:"symbolic_ref,omitempty"`
	OID         string `json:"oid,omitempty"`
	Unborn      bool   `json:"unborn"`
}

// RepositorySnapshot is an immutable export descriptor, not the manifest or
// a local filesystem path. RefsDigest binds the complete export-visible refs
// AND HEAD; SnapshotID identifies the primary-retained export view. Manifest
// hashing, verified storage and transport exist; live coherent capture and
// user-authorized view selection are not integrated into the server yet.
// Neither a timestamp nor a change-stream cursor is a substitute for it.
type RepositorySnapshot struct {
	Version              string             `json:"version"`
	Identity             RepositoryIdentity `json:"identity"`
	SnapshotID           string             `json:"snapshot_id"`
	RefsDigest           string             `json:"refs_digest"`
	ObjectFormat         string             `json:"object_format"` // sha1 or sha256
	HEAD                 Head               `json:"head"`
	ExportPolicyRevision string             `json:"export_policy_revision"`
}

// ReadPlan is a request-scoped primary decision, received over an authenticated
// peer connection after checking the ORIGINAL user/Agent credential. It is not
// a bearer token and must not be replayed as permission for another HTTP RPC.
// Discover and fetch are separate stateless Git phases; fetch must honor its
// requested OIDs, not silently substitute a newer discovered head.
type ReadPlan struct {
	Version      string             `json:"version"`
	RequestID    string             `json:"request_id"`
	EdgeID       string             `json:"edge_id"`
	Operation    string             `json:"operation"` // git.read only
	Phase        string             `json:"phase"`     // discover or fetch
	Snapshot     RepositorySnapshot `json:"snapshot"`
	AuthorizedAt time.Time          `json:"authorized_at"`
	ExpiresAt    time.Time          `json:"expires_at"`
}

func (s RepositorySnapshot) Validate() error {
	if s.Version != SnapshotVersion {
		return fmt.Errorf("unsupported snapshot version")
	}
	if err := s.Identity.Validate(); err != nil {
		return err
	}
	if !identifier(s.SnapshotID) || !identifier(s.ExportPolicyRevision) || !hexOID(s.RefsDigest, 64) {
		return fmt.Errorf("invalid snapshot binding")
	}
	length := 0
	switch s.ObjectFormat {
	case "sha1":
		length = 40
	case "sha256":
		length = 64
	default:
		return fmt.Errorf("unsupported object format")
	}
	if s.HEAD.SymbolicRef != "" && !fullRef(s.HEAD.SymbolicRef) {
		return fmt.Errorf("invalid symbolic HEAD")
	}
	if s.HEAD.Unborn {
		if s.HEAD.SymbolicRef == "" || s.HEAD.OID != "" {
			return fmt.Errorf("unborn HEAD requires a symbolic ref and no OID")
		}
	} else if !hexOID(s.HEAD.OID, length) {
		return fmt.Errorf("invalid HEAD object ID")
	}
	return nil
}

func (p ReadPlan) ValidateFor(edgeID string, now time.Time) error {
	if p.Version != ReadPlanVersion || !identifier(p.RequestID) || !identifier(p.EdgeID) || p.EdgeID != edgeID {
		return fmt.Errorf("invalid read plan binding")
	}
	if p.Operation != "git.read" || (p.Phase != "discover" && p.Phase != "fetch") {
		return fmt.Errorf("unsupported read operation or phase")
	}
	if p.AuthorizedAt.IsZero() || !p.ExpiresAt.After(p.AuthorizedAt) || p.AuthorizedAt.After(now) || !now.Before(p.ExpiresAt) {
		return fmt.Errorf("read plan is not currently valid")
	}
	return p.Snapshot.Validate()
}

func identifier(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func hexOID(value string, length int) bool {
	if len(value) != length || strings.Trim(value, "0") == "" {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func fullRef(value string) bool {
	if !utf8.ValidString(value) || len(value) > 1024 {
		return false
	}
	if !strings.HasPrefix(value, "refs/") || strings.Contains(value, "..") || strings.Contains(value, "@{") || strings.ContainsAny(value, " ~^:?*[\\") || strings.HasSuffix(value, ".") {
		return false
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return false
		}
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
