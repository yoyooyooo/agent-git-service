package routebstage0

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
)

// Synthetic private files and an in-memory registration. This does not create
// a root seal, manager, host registration or production admission.
func TestRestoreResultAndApprovalBudgetsUseRealFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "ags")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	l := &loaded{
		host:            HostProfile{Root: root, UID: uint32(os.Getuid())},
		registrationSHA: strings.Repeat("a", 64),
		recoverySHA:     strings.Repeat("b", 64),
		recovery: &RecoveryRegistration{SnapshotID: "synthetic-snapshot", ManifestSHA256: strings.Repeat("c", 64),
			GroupSHA256: strings.Repeat("d", 64), HostInstance: "fixture-host"},
		registration: Registration{Invocations: []Invocation{
			{Operation: "restore", AuthorizationID: "restore-one"},
			{Operation: "old-start", AuthorizationID: "old-start-one"},
		}},
	}
	resultPath := l.restoreResultPath()
	// Exact 16 MiB boundary. The padding is legal JSON whitespace, not a
	// measured business restore payload. The body still uses the result schema.
	if err := l.verifyRestoreArtifact(writePrivate(t, resultPath, padded(t, restoreBody(t, root), 16<<20))); err != nil {
		t.Fatal(err)
	}
	over := writePrivate(t, resultPath, padded(t, restoreBody(t, root), (16<<20)+1))
	if err := l.verifyRestoreArtifact(over); err == nil || err.Error() != "stage0_restore_result_binding" {
		t.Fatalf("16MiB+1 result: %v", err)
	}
	moved := over
	moved.Path = filepath.Join(dir, "other.json")
	if err := os.WriteFile(moved.Path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.verifyRestoreArtifact(moved); err == nil || err.Error() != "stage0_restore_result_binding" {
		t.Fatalf("moved result: %v", err)
	}
	unsafe := writePrivate(t, resultPath, restoreBody(t, root))
	if err := os.Chmod(resultPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.verifyRestoreArtifact(unsafe); err == nil || err.Error() != "stage0_input_unsafe" {
		t.Fatalf("mode result: %v", err)
	}

	for _, item := range []struct {
		name, purpose, id string
	}{
		{"restore", "full-restore", "restore-one"},
		{"old-start", "old-start", "old-start-one"},
	} {
		t.Run(item.name, func(t *testing.T) {
			path := filepath.Join(filepath.Dir(root), "recovery/approvals", item.name+".json")
			if _, err := l.readRecoveryApproval(writePrivate(t, path, padded(t, approvalBody(t, item.purpose, item.id), 1<<20)), item.purpose, true); err != nil {
				t.Fatal(err)
			}
			over := writePrivate(t, path, padded(t, approvalBody(t, item.purpose, item.id), (1<<20)+1))
			if _, err := l.readRecoveryApproval(over, item.purpose, true); err == nil || err.Error() != "stage0_recovery_approval_path" {
				t.Fatalf("1MiB+1 approval: %v", err)
			}
		})
	}
}

func writePrivate(t *testing.T, path string, raw []byte) interpreterauthority.File {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("private file not retained: %v", err)
	}
	return interpreterauthority.File{Path: path, SHA256: hash(raw), Size: int64(len(raw))}
}

func padded(t *testing.T, body []byte, size int) []byte {
	t.Helper()
	if len(body) > size {
		t.Fatalf("body %d exceeds %d", len(body), size)
	}
	raw := make([]byte, size)
	copy(raw, body)
	for i := len(body); i < size; i++ {
		raw[i] = ' '
	}
	return raw
}

func restoreBody(t *testing.T, root string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"schema": "ags.routeb-native-restore-result.v1", "root": root,
		"registration_sha256": strings.Repeat("a", 64), "recovery_registration_sha256": strings.Repeat("b", 64),
		"snapshot_id": "synthetic-snapshot", "manifest_sha256": strings.Repeat("c", 64), "group_sha256": strings.Repeat("d", 64),
		"restored": map[string]any{"schema": "ags.routeb-restored-group.v2", "applied": true, "started": false,
			"snapshot_id": "synthetic-snapshot", "members": []any{map[string]any{"id": "member-000000", "role": "config",
				"target": filepath.Join(root, "state/member-000000.json"), "provision_separately": false}}},
		"verification": map[string]any{"manifest_sha256": strings.Repeat("c", 64)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func approvalBody(t *testing.T, purpose, id string) []byte {
	t.Helper()
	now := time.Now().UTC()
	raw, err := json.Marshal(map[string]any{
		"schema": "ags.routeb-recovery-approval.v1", "approval_id": id, "actor": "human:fixture",
		"purpose": purpose, "decision": "approve", "issued_at": now.Add(-time.Minute).Format(time.RFC3339),
		"expires_at": now.Add(time.Hour).Format(time.RFC3339), "host_instance": "fixture-host",
		"snapshot_id": "synthetic-snapshot", "manifest_sha256": strings.Repeat("c", 64),
		"data_loss_acknowledged": true, "external_reconciliation_acknowledged": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
