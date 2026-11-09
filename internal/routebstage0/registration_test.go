package routebstage0

import (
	"encoding/json"
	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureRegistration() Registration {
	id := "fixture-native"
	sha := strings.Repeat("a", 64)
	file := func(path string) interpreterauthority.File {
		return interpreterauthority.File{Path: path, SHA256: sha, Size: 1}
	}
	return Registration{Schema: Schema, ID: id, Scope: "isolated-native",
		Binding:     interpreterauthority.Binding{HostInstance: "fixture-host", HostProfileSHA256: sha, ToolRevision: strings.Repeat("b", 40), ToolTree: strings.Repeat("c", 40), ToolManifestSHA256: sha, Platform: "linux", Architecture: "amd64"},
		NativeImage: file(filepath.Join(rootFor(id), "stage0/ags-routeb-stage0")), NativeRecoveryImage: file(filepath.Join(rootFor(id), "stage0/recovery/ags-routeb-stage0")),
		HostProfile: file(filepath.Join(rootFor(id), "host-profile.json")), Interpreter: Reference{ID: "publisher-one", SHA256: sha},
		Invocations: []Invocation{{ID: "probe-one", Operation: "probe", Phase: "probe", AttemptID: "attempt-one", AuthorityEpoch: 1, AuthorizationID: "authorization-one", Unit: unitName(id, "probe"),
			Definition: file(filepath.Join("/etc/systemd/system", unitName(id, "probe"))), DefinitionRecovery: file(filepath.Join(rootFor(id), "unit-recovery", unitName(id, "probe")))}}}
}

func TestRegistrationBindsRootImagesUnitsAndIndependentRecovery(t *testing.T) {
	r := fixtureRegistration()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Registration){
		"production":               func(r *Registration) { r.Scope = "production" },
		"other-image":              func(r *Registration) { r.NativeImage.Path = "/tmp/stage0" },
		"same-recovery-path":       func(r *Registration) { r.NativeRecoveryImage.Path = r.NativeImage.Path },
		"different-recovery-bytes": func(r *Registration) { r.NativeRecoveryImage.SHA256 = strings.Repeat("d", 64) },
		"other-unit":               func(r *Registration) { r.Invocations[0].Unit = "agent-git-service.service" },
		"unregistered-recovery":    func(r *Registration) { r.Invocations[0].DefinitionRecovery = interpreterauthority.File{} },
		"epoch-replay":             func(r *Registration) { r.Invocations[0].AuthorityEpoch = 2 },
		"phase-substitution":       func(r *Registration) { r.Invocations[0].Phase = "serve-old" },
		"future-approval-required": func(r *Registration) { r.Invocations[0].Operation = "restore"; r.Invocations[0].Phase = "restore" },
		"duplicate-invocation":     func(r *Registration) { r.Invocations = append(r.Invocations, r.Invocations[0]) },
		"caller-grant-path":        func(r *Registration) { f := r.HostProfile; f.Path = "/tmp/grant.json"; r.ScopeGrant = &f },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			v := fixtureRegistration()
			mutate(&v)
			if err := v.Validate(); err == nil {
				t.Fatal("accepted widened or incomplete authority")
			}
		})
	}
}

func TestRegistrationJSONRejectsAliasAndMissingRecoveryField(t *testing.T) {
	raw, err := json.Marshal(fixtureRegistration())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{strings.Replace(string(raw), `"schema":`, `"Schema":`, 1), strings.Replace(string(raw), `"schema":`, `"Schema":"untrusted","schema":`, 1), strings.Replace(string(raw), `"definition_recovery":`, `"recovery_copy":`, 1)} {
		var r Registration
		if err = decodeExact([]byte(value), &r); err == nil {
			t.Fatal("noncanonical registration keys accepted")
		}
	}
}

func TestFixedChildArgumentsCannotChooseScriptsOrManagerTargets(t *testing.T) {
	r := fixtureRegistration()
	h := HostProfile{Root: "/home/example/.local/state/ags-routeb-native/fixture-native/ags", ToolBundle: rootFor(r.ID) + "/bundle", LogicalHost: "imile-win"}
	f := interpreterauthority.File{Path: filepath.Dir(h.Root) + "/package/package.json", SHA256: strings.Repeat("a", 64), Size: 1}
	a := f
	a.Path = filepath.Dir(h.Root) + "/package/approval.json"
	r.Inputs.Package = &f
	r.Inputs.Approval = &a
	args, err := childArguments(r, h, "/publisher/python", "old-start")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(args[:5], " ") != "/publisher/python -I -B -S "+h.ToolBundle+"/routeb_native_recovery.py" || args[5] != "old-start" {
		t.Fatalf("wrong fixed entry: %q", args)
	}
	for _, op := range []string{"/tmp/code.py", "-c", "stop", "restore --unsafe"} {
		if _, err := childArguments(r, h, "/publisher/python", op); err == nil {
			t.Fatalf("accepted %q", op)
		}
	}
	for _, args := range [][]string{{"serve"}, {"serve", "--registration", "fixture", "--operation", "probe", "--skip-closure"}, {"serve", "--registration", "fixture", "--operation", "/tmp/code.py"}} {
		if Execute(args) == nil {
			t.Fatal("caller extended native command")
		}
	}
}

func TestRecoveryReviewBindsDistinctPurposeAndSnapshot(t *testing.T) {
	r := fixtureRegistration()
	f := r.HostProfile
	f.Path = rootFor(r.ID) + "/scope-grant.json"
	r.ScopeGrant = &f
	l := loaded{registration: r}
	a := RecoveryApproval{IssuedAt: "2020-01-01T00:00:00Z", Schema: "ags.routeb-recovery-approval.v2", ApprovalID: "restore-one", Actor: "agent:reviewer", Purpose: "full-restore", HostInstance: "fixture-host", SnapshotID: "snapshot-one", ManifestSHA256: strings.Repeat("b", 64), DataLossAcknowledged: true, ExternalReconciliationAcknowledged: true}
	a.Delegation = &Delegation{ScopeGrantSHA256: f.SHA256, Review: DelegatedReview{Actor: a.Actor, SubjectSHA256: a.subject(), ReviewedAt: "2020-01-01T00:00:00Z", EvidenceRefs: []EvidenceReference{{Kind: "review", SHA256: strings.Repeat("c", 64)}}}}
	if !l.recoveryActor(a, true) {
		t.Fatal("exact delegated review refused")
	}
	a.IssuedAt = "2019-12-31T00:00:00Z"
	if l.recoveryActor(a, false) {
		t.Fatal("historical review after original issue time accepted")
	}
	a.IssuedAt = "2020-01-01T00:00:00Z"
	a.Purpose = "old-start"
	if l.recoveryActor(a, true) {
		t.Fatal("restore review reused for old start")
	}
	a.Purpose = "full-restore"
	a.SnapshotID = "snapshot-two"
	if l.recoveryActor(a, true) {
		t.Fatal("review moved to another snapshot")
	}
	a.SnapshotID = "snapshot-one"
	a.Actor = "human:operator"
	if l.recoveryActor(a, true) {
		t.Fatal("agent review mislabeled human")
	}
}
