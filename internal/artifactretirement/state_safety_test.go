package artifactretirement

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func safetyIntent() Intent {
	return Intent{Schema: IntentSchema, OperationID: "exact-retirement", Repository: "owner/repo", DefaultBranch: "main", ExpectedAncestor: strings.Repeat("1", 40), RecoveryArchive: "off-host://fixture/recovery.bundle", RecoveryArchiveSHA256: strings.Repeat("2", 64), RecoveryRefsSHA256: strings.Repeat("6", 64), AllowSignatureRemoval: true, RetiredBlobs: []BlobSpec{{OID: strings.Repeat("3", 40), Bytes: 123, SHA256: strings.Repeat("4", 64)}}}
}

func TestRetirementRejectsTraversalAndSymlinkedState(t *testing.T) {
	for _, id := range []string{".", "..", "../other"} {
		in := safetyIntent()
		in.OperationID = id
		if err := validateIntent(in); err == nil {
			t.Fatalf("unsafe operation ID accepted: %q", id)
		}
	}
	in := safetyIntent()
	in.Repository = "../repo"
	if err := validateIntent(in); err == nil {
		t.Fatal("traversal repository accepted")
	}
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateStateDirectory(filepath.Join(link, "new")); err == nil {
		t.Fatal("symlinked state accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatal("created state through rejected symlink")
	}
	public := filepath.Join(root, "public")
	if err := os.Mkdir(public, 0755); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateStateDirectory(public); err == nil {
		t.Fatal("unowned permissions silently repaired")
	}
	if st, _ := os.Stat(public); st.Mode().Perm() != 0755 {
		t.Fatal("existing state permissions changed")
	}
}

func TestRetirementAbsenceVerificationDoesNotHideCancellationOrFailure(t *testing.T) {
	bare, _, _, oid, _ := testFixture(t)
	if exists, err := objectPhysicallyExists(context.Background(), bare, oid); err != nil || !exists {
		t.Fatalf("present object: %v %v", exists, err)
	}
	missing := strings.Repeat("9", 40)
	if exists, err := objectPhysicallyExists(context.Background(), bare, missing); err != nil || exists {
		t.Fatalf("missing object: %v %v", exists, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := VerifyRetiredAbsent(ctx, bare, []BlobSpec{{OID: oid}}); err == nil {
		t.Fatal("cancelled observation reported retired absence")
	}
	if err := VerifyRetiredAbsent(context.Background(), filepath.Join(t.TempDir(), "missing.git"), []BlobSpec{{OID: oid}}); err == nil {
		t.Fatal("inaccessible store reported retired absence")
	}
}

func TestRetirementIntentFingerprintAndPendingGateSurviveRestart(t *testing.T) {
	in := safetyIntent()
	root := t.TempDir()
	config := t.TempDir()
	path := filepath.Join(config, "intent.json")
	save := func() {
		t.Helper()
		data, _ := json.Marshal(in)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save()
	gate, err := AcquireStartupGate(root, path, 7, in)
	if err != nil {
		t.Fatal(err)
	}
	if err = gate.MarkPending(); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireStartupGate(root, path, 7, in); err == nil {
		t.Fatal("second process acquired pending operation lock")
	}
	gate.Close()
	if err = CheckStartupGate(root, ""); err == nil {
		t.Fatal("removing intent configuration bypassed pending migration")
	}
	if err = CheckStartupGate(root, path); err != nil {
		t.Fatal(err)
	}
	original, err := IntentSHA256(in)
	if err != nil {
		t.Fatal(err)
	}
	in.RecoveryArchiveSHA256 = strings.Repeat("5", 64)
	save()
	changed, err := IntentSHA256(in)
	if err != nil {
		t.Fatal(err)
	}
	if changed == original {
		t.Fatal("recovery evidence not fingerprinted")
	}
	if err = CheckStartupGate(root, path); err == nil {
		t.Fatal("changed recovery evidence resumed old operation")
	}
	in.RecoveryArchiveSHA256 = strings.Repeat("2", 64)
	save()
	if _, err := AcquireStartupGate(root, path, 8, in); err == nil {
		t.Fatal("replacement repository adopted interrupted operation")
	}
	gate, err = AcquireStartupGate(root, path, 7, in)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	if err = gate.Complete(); err != nil {
		t.Fatal(err)
	}
	if err = CheckStartupGate(root, ""); err != nil {
		t.Fatal("completed operation left startup blocked", err)
	}
}

func TestRetirementCheckpointRejectsTrailingJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	if err := os.WriteFile(path, []byte("{}\n{}"), 0600); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := LoadCheckpoint(path, &value); err == nil {
		t.Fatal("ambiguous trailing state accepted")
	}
}
