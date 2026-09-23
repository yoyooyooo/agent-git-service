package edgeprotocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

const (
	ManifestVersion  = "ags.edge.manifest.v1"
	MaxManifestBytes = 16 << 20
	MaxManifestRefs  = 100000
)

// Ref is an export-visible direct reference. Symbolic aliases other than HEAD
// are deliberately not flattened: the first capture adapter must reject them.
// No pack, source path, credential, hidden retention ref or fetch URL is here.
type Ref struct {
	Name string `json:"name"`
	OID  string `json:"oid"`
}

// Manifest binds the exact exported refs, HEAD, format, policy and immutable
// repository identity. Its hashes establish integrity, NOT authority. The
// caller must already have a current primary-authorized export policy.
type Manifest struct {
	Version  string             `json:"version"`
	Snapshot RepositorySnapshot `json:"snapshot"`
	Refs     []Ref              `json:"refs"`
}

// NewManifest owns a canonical copy of refs. SnapshotID is content-addressed,
// including authority/store identity, so same-name recreated repositories and
// different export policies cannot alias an old published directory.
func NewManifest(identity RepositoryIdentity, format string, head Head, policy string, refs []Ref) (Manifest, error) {
	m := Manifest{Version: ManifestVersion, Refs: append([]Ref{}, refs...)}
	sort.Slice(m.Refs, func(i, j int) bool { return m.Refs[i].Name < m.Refs[j].Name })
	m.Snapshot = RepositorySnapshot{Version: SnapshotVersion, Identity: identity,
		ObjectFormat: format, HEAD: head, ExportPolicyRevision: policy}
	m.Snapshot.RefsDigest = m.refsDigest()
	m.Snapshot.SnapshotID = m.snapshotID()
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func digestJSON(value any) string {
	data, _ := json.Marshal(value) // These closed value structs cannot fail.
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (m Manifest) refsDigest() string {
	return digestJSON(struct {
		Version string `json:"version"`
		Format  string `json:"object_format"`
		HEAD    Head   `json:"head"`
		Refs    []Ref  `json:"refs"`
	}{ManifestVersion, m.Snapshot.ObjectFormat, m.Snapshot.HEAD, m.Refs})
}

func (m Manifest) snapshotID() string {
	return "snap-" + digestJSON(struct {
		Version  string             `json:"version"`
		Identity RepositoryIdentity `json:"identity"`
		Digest   string             `json:"refs_digest"`
		Policy   string             `json:"policy"`
	}{SnapshotVersion, m.Snapshot.Identity, m.Snapshot.RefsDigest, m.Snapshot.ExportPolicyRevision})
}

func (m Manifest) Validate() error {
	if m.Version != ManifestVersion || m.Refs == nil || len(m.Refs) > MaxManifestRefs {
		return errors.New("invalid manifest version or ref collection")
	}
	if err := m.Snapshot.Validate(); err != nil {
		return err
	}
	oidLength := 40
	if m.Snapshot.ObjectFormat == "sha256" {
		oidLength = 64
	}
	refs := make(map[string]string, len(m.Refs))
	previous := ""
	for _, ref := range m.Refs {
		if len(ref.Name) > 1024 || !fullRef(ref.Name) || !hexOID(ref.OID, oidLength) || ref.Name <= previous {
			return errors.New("invalid, duplicate or unsorted manifest ref")
		}
		refs[ref.Name] = ref.OID
		previous = ref.Name
	}
	// Detect file/directory collisions even if another lexicographic name
	// occurs between refs/heads/a and refs/heads/a/b.
	for name := range refs {
		for i := len(name) - 1; i > 0; i-- {
			if name[i] == '/' {
				if _, ok := refs[name[:i]]; ok {
					return errors.New("conflicting manifest refs")
				}
			}
		}
	}
	head := m.Snapshot.HEAD
	if head.SymbolicRef != "" {
		oid, exists := refs[head.SymbolicRef]
		if head.Unborn && exists || !head.Unborn && (!exists || oid != head.OID) {
			return errors.New("HEAD does not match exported references")
		}
	}
	if m.Snapshot.RefsDigest != m.refsDigest() || m.Snapshot.SnapshotID != m.snapshotID() {
		return errors.New("manifest content binding mismatch")
	}
	return nil
}

// Roots returns unique exact OIDs; these are the only pack traversal roots.
// HEAD can contribute a detached object. Arbitrary revision expressions and
// --all are never accepted from the peer or inferred from a source directory.
func (m Manifest) Roots() []string {
	seen := make(map[string]bool, len(m.Refs)+1)
	for _, ref := range m.Refs {
		seen[ref.OID] = true
	}
	if m.Snapshot.HEAD.OID != "" {
		seen[m.Snapshot.HEAD.OID] = true
	}
	roots := make([]string, 0, len(seen))
	for oid := range seen {
		roots = append(roots, oid)
	}
	sort.Strings(roots)
	return roots
}

func (m Manifest) Clone() Manifest {
	m.Refs = append([]Ref{}, m.Refs...)
	return m
}

// Key is a filesystem-safe, fixed-length binding to the ENTIRE descriptor.
// It does not rely on case-sensitive filenames or concatenate peer identifiers.
func SnapshotKey(snapshot RepositorySnapshot) (string, error) {
	if err := snapshot.Validate(); err != nil {
		return "", err
	}
	return digestJSON(snapshot), nil
}

func EncodeManifest(m Manifest) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxManifestBytes {
		return nil, errors.New("manifest too large")
	}
	return data, nil
}

func DecodeManifest(r io.Reader) (Manifest, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxManifestBytes+1))
	if err != nil {
		return Manifest{}, err
	}
	if len(data) > MaxManifestBytes {
		return Manifest{}, errors.New("manifest too large")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if dec.Decode(new(any)) != io.EOF {
		return Manifest{}, errors.New("trailing manifest data")
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	// The wire encoding is deliberately canonical. This also rejects duplicate
	// JSON keys rather than allowing last-key-wins to cross trust boundaries.
	canonical, err := EncodeManifest(m)
	if err != nil {
		return Manifest{}, err
	}
	if !bytes.Equal(bytes.TrimSpace(data), canonical) {
		return Manifest{}, errors.New("noncanonical manifest encoding")
	}
	return m, nil
}
