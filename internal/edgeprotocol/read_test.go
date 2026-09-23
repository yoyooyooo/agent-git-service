package edgeprotocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sampleSnapshot() RepositorySnapshot {
	return RepositorySnapshot{
		Version:    SnapshotVersion,
		Identity:   RepositoryIdentity{AuthorityID: "ags-region-b", StoreID: "store-never-reused", RepositoryID: 7, Kind: "repo"},
		SnapshotID: "export-1", RefsDigest: strings.Repeat("a", 64), ObjectFormat: "sha1",
		HEAD:                 Head{SymbolicRef: "refs/heads/main", OID: strings.Repeat("b", 40)},
		ExportPolicyRevision: "policy-1",
	}
}

func TestRepositorySnapshotDescriptor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*RepositorySnapshot)
		valid  bool
	}{
		{"symbolic", func(s *RepositorySnapshot) {}, true},
		{"unborn", func(s *RepositorySnapshot) { s.HEAD = Head{SymbolicRef: "refs/heads/main", Unborn: true} }, true},
		{"detached", func(s *RepositorySnapshot) { s.HEAD.SymbolicRef = "" }, true},
		{"sha256", func(s *RepositorySnapshot) { s.ObjectFormat = "sha256"; s.HEAD.OID = strings.Repeat("c", 64) }, true},
		{"wiki", func(s *RepositorySnapshot) { s.Identity.Kind = "wiki" }, true},
		{"wrong version", func(s *RepositorySnapshot) { s.Version = "future" }, false},
		{"no authority", func(s *RepositorySnapshot) { s.Identity.AuthorityID = "" }, false},
		{"path as store id", func(s *RepositorySnapshot) { s.Identity.StoreID = "../repo" }, false},
		{"missing repository", func(s *RepositorySnapshot) { s.Identity.RepositoryID = 0 }, false},
		{"unknown store kind", func(s *RepositorySnapshot) { s.Identity.Kind = "database" }, false},
		{"missing policy", func(s *RepositorySnapshot) { s.ExportPolicyRevision = "" }, false},
		{"missing snapshot", func(s *RepositorySnapshot) { s.SnapshotID = "" }, false},
		{"invalid digest", func(s *RepositorySnapshot) { s.RefsDigest = "timestamp-is-not-a-digest" }, false},
		{"uppercase oid", func(s *RepositorySnapshot) { s.HEAD.OID = strings.Repeat("B", 40) }, false},
		{"zero oid", func(s *RepositorySnapshot) { s.HEAD.OID = strings.Repeat("0", 40) }, false},
		{"unknown format", func(s *RepositorySnapshot) { s.ObjectFormat = "other" }, false},
		{"wrong oid length", func(s *RepositorySnapshot) { s.ObjectFormat = "sha256" }, false},
		{"unborn with oid", func(s *RepositorySnapshot) { s.HEAD.Unborn = true }, false},
		{"unborn detached", func(s *RepositorySnapshot) { s.HEAD = Head{Unborn: true} }, false},
		{"invalid symbolic", func(s *RepositorySnapshot) { s.HEAD.SymbolicRef = "refs/heads/main.lock" }, false},
		{"ambiguous symbolic", func(s *RepositorySnapshot) { s.HEAD.SymbolicRef = "refs/heads/a..b" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := sampleSnapshot()
			tc.change(&snapshot)
			if err := snapshot.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestReadPlanBindingsAndTimeBoundary(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	base := ReadPlan{Version: ReadPlanVersion, RequestID: "request-1", EdgeID: "edge-1", Operation: "git.read", Phase: "discover", Snapshot: sampleSnapshot(), AuthorizedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute)}
	for _, tc := range []struct {
		name   string
		change func(*ReadPlan)
		edge   string
		valid  bool
	}{
		{"discover", func(p *ReadPlan) {}, "edge-1", true},
		{"fetch", func(p *ReadPlan) { p.Phase = "fetch" }, "edge-1", true},
		{"different edge", func(p *ReadPlan) {}, "edge-fixture2", false},
		{"no request binding", func(p *ReadPlan) { p.RequestID = "" }, "edge-1", false},
		{"write forbidden", func(p *ReadPlan) { p.Operation = "git.push" }, "edge-1", false},
		{"unknown phase", func(p *ReadPlan) { p.Phase = "any" }, "edge-1", false},
		{"expired exactly", func(p *ReadPlan) { p.ExpiresAt = now }, "edge-1", false},
		{"future decision", func(p *ReadPlan) { p.AuthorizedAt = now.Add(time.Second) }, "edge-1", false},
		{"zero decision", func(p *ReadPlan) { p.AuthorizedAt = time.Time{} }, "edge-1", false},
		{"bad snapshot", func(p *ReadPlan) { p.Snapshot.Identity.StoreID = "" }, "edge-1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := base
			tc.change(&plan)
			if err := plan.ValidateFor(tc.edge, now); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ReadPlan
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != base {
		t.Fatal("descriptor JSON round trip changed bindings")
	}
	// A successful shape check is NOT an authorization test. No network
	// handler accepts these descriptors from clients in this stage.
}
