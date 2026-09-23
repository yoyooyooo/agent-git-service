package edgeprotocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func manifestFixture(t *testing.T) Manifest {
	t.Helper()
	m, err := NewManifest(RepositoryIdentity{AuthorityID: "primary", StoreID: "store-1", RepositoryID: 7, Kind: "repo"},
		"sha1", Head{SymbolicRef: "refs/heads/main", OID: strings.Repeat("a", 40)}, "policy-1",
		[]Ref{{Name: "refs/tags/v1", OID: strings.Repeat("b", 40)}, {Name: "refs/heads/main", OID: strings.Repeat("a", 40)}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestManifestCanonicalBindings(t *testing.T) {
	m := manifestFixture(t)
	if m.Refs[0].Name != "refs/heads/main" {
		t.Fatal("refs not canonicalized")
	}
	reordered := []Ref{m.Refs[1], m.Refs[0]}
	same, err := NewManifest(m.Snapshot.Identity, m.Snapshot.ObjectFormat, m.Snapshot.HEAD, m.Snapshot.ExportPolicyRevision, reordered)
	if err != nil || same.Snapshot != m.Snapshot {
		t.Fatalf("order changed binding: %v", err)
	}
	reordered[0].OID = strings.Repeat("c", 40)
	if same.Refs[1].OID != strings.Repeat("b", 40) {
		t.Fatal("constructor retained caller slice")
	}
	for name, change := range map[string]func(*Manifest){
		"delete ref":        func(m *Manifest) { m.Refs = m.Refs[:1] },
		"force ref":         func(m *Manifest) { m.Refs[1].OID = strings.Repeat("c", 40) },
		"store incarnation": func(m *Manifest) { m.Snapshot.Identity.StoreID = "new-store" },
		"authority":         func(m *Manifest) { m.Snapshot.Identity.AuthorityID = "another-primary" },
		"kind":              func(m *Manifest) { m.Snapshot.Identity.Kind = "wiki" },
		"policy":            func(m *Manifest) { m.Snapshot.ExportPolicyRevision = "policy-2" },
		"HEAD":              func(m *Manifest) { m.Snapshot.HEAD = Head{OID: strings.Repeat("b", 40)} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := m.Clone()
			change(&changed)
			if changed.Validate() == nil {
				t.Fatal("tampering accepted")
			}
			rebuilt, err := NewManifest(changed.Snapshot.Identity, changed.Snapshot.ObjectFormat, changed.Snapshot.HEAD, changed.Snapshot.ExportPolicyRevision, changed.Refs)
			if err != nil {
				t.Fatal(err)
			}
			if rebuilt.Snapshot.SnapshotID == m.Snapshot.SnapshotID {
				t.Fatal("semantic change did not change snapshot identity")
			}
		})
	}
}

func TestManifestRejectsInconsistentRefsAndHead(t *testing.T) {
	base := manifestFixture(t)
	for name, change := range map[string]func(*Manifest){
		"duplicate": func(m *Manifest) { m.Refs = append(m.Refs, m.Refs[0]) },
		"parent conflict": func(m *Manifest) {
			m.Refs = append(m.Refs, Ref{Name: "refs/heads/main/child", OID: strings.Repeat("a", 40)})
		},
		"invalid ref":        func(m *Manifest) { m.Refs[1].Name = "refs/heads/../secret" },
		"null OID":           func(m *Manifest) { m.Refs[1].OID = strings.Repeat("0", 40) },
		"HEAD mismatch":      func(m *Manifest) { m.Snapshot.HEAD.OID = strings.Repeat("c", 40) },
		"unexported HEAD":    func(m *Manifest) { m.Snapshot.HEAD.SymbolicRef = "refs/internal/secret" },
		"unborn but present": func(m *Manifest) { m.Snapshot.HEAD = Head{SymbolicRef: "refs/heads/main", Unborn: true} },
	} {
		t.Run(name, func(t *testing.T) {
			m := base.Clone()
			change(&m)
			if _, err := NewManifest(m.Snapshot.Identity, m.Snapshot.ObjectFormat, m.Snapshot.HEAD, m.Snapshot.ExportPolicyRevision, m.Refs); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	m, err := NewManifest(base.Snapshot.Identity, "sha1", Head{SymbolicRef: "refs/heads/main", Unborn: true}, "policy-1", nil)
	if err != nil || len(m.Roots()) != 0 {
		t.Fatalf("empty unborn manifest: %v", err)
	}
	m, err = NewManifest(base.Snapshot.Identity, "sha256", Head{OID: strings.Repeat("e", 64)}, "policy-1", nil)
	if err != nil || len(m.Roots()) != 1 {
		t.Fatalf("detached sha256: %v", err)
	}
}

func TestManifestWireIsBoundedClosedAndCanonical(t *testing.T) {
	m := manifestFixture(t)
	wire, err := EncodeManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeManifest(bytes.NewReader(wire))
	if err != nil || got.Snapshot != m.Snapshot {
		t.Fatalf("roundtrip: %v", err)
	}
	for name, input := range map[string][]byte{
		"second JSON":     append(append([]byte{}, wire...), wire...),
		"unknown field":   bytes.Replace(wire, []byte(`{"version"`), []byte(`{"credential":"secret","version"`), 1),
		"duplicate field": bytes.Replace(wire, []byte(`{"version"`), []byte(`{"version":"wrong","version"`), 1),
		"truncated":       wire[:len(wire)-1],
		"oversized":       bytes.Repeat([]byte(" "), MaxManifestBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeManifest(bytes.NewReader(input)); err == nil {
				t.Fatal("bad wire accepted")
			}
		})
	}
	var decoded map[string]any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "credential") || strings.Contains(string(wire), "path") {
		t.Fatal("manifest leaked runtime fields")
	}
}
