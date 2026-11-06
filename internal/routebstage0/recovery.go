package routebstage0

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
)

// RecoveryRegistration is a separately provisioned, write-once late binding.
// The original source/host/interpreter registration does not change when a
// real backup obtains its manifest digest. This authority cannot widen roots,
// manager targets, operation IDs or executable commands.
type RecoveryRegistration struct {
	Schema                     string                    `json:"schema"`
	RegistrationID             string                    `json:"registration_id"`
	RegistrationSHA256         string                    `json:"registration_sha256"`
	HostInstance               string                    `json:"host_instance"`
	ToolManifestSHA256         string                    `json:"tool_manifest_sha256"`
	InterpreterAuthoritySHA256 string                    `json:"interpreter_authority_sha256"`
	BackupDirectory            string                    `json:"backup_directory"`
	SnapshotID                 string                    `json:"snapshot_id"`
	ManifestSHA256             string                    `json:"manifest_sha256"`
	GroupSHA256                string                    `json:"group_sha256"`
	RestoreApproval            interpreterauthority.File `json:"restore_approval"`
	OldStartApproval           interpreterauthority.File `json:"old_start_approval"`
}

type RecoveryApproval struct {
	Schema                             string      `json:"schema"`
	ApprovalID                         string      `json:"approval_id"`
	Actor                              string      `json:"actor"`
	Purpose                            string      `json:"purpose"`
	Decision                           string      `json:"decision"`
	IssuedAt                           string      `json:"issued_at"`
	ExpiresAt                          string      `json:"expires_at"`
	HostInstance                       string      `json:"host_instance"`
	SnapshotID                         string      `json:"snapshot_id"`
	ManifestSHA256                     string      `json:"manifest_sha256"`
	DataLossAcknowledged               bool        `json:"data_loss_acknowledged"`
	ExternalReconciliationAcknowledged bool        `json:"external_reconciliation_acknowledged"`
	Delegation                         *Delegation `json:"delegation,omitempty"`
}

// RestoreCompletion is written only by the root supervisor after its exact
// spawned restore child supplies a digest. User-owned result JSON alone is
// never proof that a restoration ran, nor permission to start a process.
type RestoreCompletion struct {
	Schema                     string                    `json:"schema"`
	RegistrationSHA256         string                    `json:"registration_sha256"`
	RecoveryRegistrationSHA256 string                    `json:"recovery_registration_sha256"`
	RestoreInvocationID        string                    `json:"restore_invocation_id"`
	StoppedInvocationID        string                    `json:"stopped_invocation_id"`
	BootID                     string                    `json:"boot_id"`
	ContextSHA256              string                    `json:"context_sha256"`
	Result                     interpreterauthority.File `json:"result"`
}

type RestoreArtifact struct {
	Schema                     string         `json:"schema"`
	Root                       string         `json:"root"`
	RegistrationSHA256         string         `json:"registration_sha256"`
	RecoveryRegistrationSHA256 string         `json:"recovery_registration_sha256"`
	SnapshotID                 string         `json:"snapshot_id"`
	ManifestSHA256             string         `json:"manifest_sha256"`
	GroupSHA256                string         `json:"group_sha256"`
	Restored                   map[string]any `json:"restored"`
	Verification               map[string]any `json:"verification"`
}

func (l *loaded) restoreResultPath() string {
	return filepath.Join(filepath.Dir(l.host.Root), "recovery/native-recovery/restore-result.json")
}

func (l *loaded) verifyRestoreArtifact(file interpreterauthority.File) error {
	if l.recovery == nil || file.Path != l.restoreResultPath() || !validFile(file, 16<<20) {
		return refused("stage0_restore_result_binding")
	}
	raw, err := readInput(file, l.host.UID)
	if err != nil {
		return err
	}
	var value RestoreArtifact
	if err = decodeExact(raw, &value); err != nil {
		return err
	}
	r := l.recovery
	if value.Schema != "ags.routeb-native-restore-result.v1" || value.Root != l.host.Root || value.RegistrationSHA256 != l.registrationSHA ||
		value.RecoveryRegistrationSHA256 != l.recoverySHA || value.SnapshotID != r.SnapshotID || value.ManifestSHA256 != r.ManifestSHA256 || value.GroupSHA256 != r.GroupSHA256 ||
		value.Restored["schema"] != "ags.routeb-restored-group.v2" || value.Restored["applied"] != true || value.Restored["started"] != false || value.Restored["snapshot_id"] != r.SnapshotID ||
		value.Verification["manifest_sha256"] != r.ManifestSHA256 {
		return refused("stage0_restore_result_binding")
	}
	return nil
}

type EvidenceReference struct {
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256"`
}
type DelegatedReview struct {
	Actor         string              `json:"actor"`
	SubjectSHA256 string              `json:"subject_sha256"`
	ReviewedAt    string              `json:"reviewed_at"`
	EvidenceRefs  []EvidenceReference `json:"evidence_refs"`
}
type Delegation struct {
	ScopeGrantSHA256 string          `json:"scope_grant_sha256"`
	Review           DelegatedReview `json:"review"`
}

func (a RecoveryApproval) subject() string {
	raw, _ := json.Marshal(map[string]any{"approval_id": a.ApprovalID, "purpose": a.Purpose, "host_instance": a.HostInstance, "snapshot_id": a.SnapshotID, "manifest_sha256": a.ManifestSHA256,
		"data_loss_acknowledged": a.DataLossAcknowledged, "external_reconciliation_acknowledged": a.ExternalReconciliationAcknowledged})
	return hash(append(raw, '\n'))
}

func (l *loaded) recoveryActor(a RecoveryApproval, checkTime bool) bool {
	if a.Schema == "ags.routeb-recovery-approval.v1" {
		return strings.HasPrefix(a.Actor, "human:") && len(a.Actor) > 6 && a.Delegation == nil
	}
	if a.Schema != "ags.routeb-recovery-approval.v2" || !strings.HasPrefix(a.Actor, "agent:") || len(a.Actor) <= 6 || a.Delegation == nil || l.registration.ScopeGrant == nil {
		return false
	}
	d := a.Delegation
	if d.ScopeGrantSHA256 != l.registration.ScopeGrant.SHA256 || d.Review.Actor != a.Actor || d.Review.SubjectSHA256 != a.subject() || len(d.Review.EvidenceRefs) < 1 || len(d.Review.EvidenceRefs) > 32 {
		return false
	}
	seen := map[string]bool{}
	for _, ref := range d.Review.EvidenceRefs {
		key := ref.Kind + ":" + ref.SHA256
		if !interpreterauthority.ValidID(ref.Kind) || !validDigest(ref.SHA256) || seen[key] {
			return false
		}
		seen[key] = true
	}
	reviewed, err := time.Parse(time.RFC3339, d.Review.ReviewedAt)
	return err == nil && (!checkTime || !reviewed.After(time.Now()))
}

func (l *loaded) recoveryPath() string {
	return filepath.Join(rootFor(l.registration.ID), "recovery-registration.json")
}

func (l *loaded) readRecoveryApproval(reference interpreterauthority.File, purpose string, checkTime bool) (RecoveryApproval, error) {
	var value RecoveryApproval
	if !validFile(reference, 1<<20) || !within(reference.Path, filepath.Join(filepath.Dir(l.host.Root), "recovery/approvals")) {
		return value, refused("stage0_recovery_approval_path")
	}
	raw, err := readInput(reference, l.host.UID)
	if err != nil {
		return value, err
	}
	if err = decodeExact(raw, &value); err != nil {
		return value, err
	}
	r := l.recovery
	if !l.recoveryActor(value, checkTime) || !interpreterauthority.ValidID(value.ApprovalID) ||
		value.Purpose != purpose || value.Decision != "approve" || !value.DataLossAcknowledged || !value.ExternalReconciliationAcknowledged ||
		value.HostInstance != r.HostInstance || value.SnapshotID != r.SnapshotID || value.ManifestSHA256 != r.ManifestSHA256 {
		return value, refused("stage0_recovery_approval_binding")
	}
	operation := "restore"
	if purpose == "old-start" {
		operation = "old-start"
	}
	found := false
	for _, i := range l.registration.Invocations {
		if i.Operation == operation && i.AuthorizationID == value.ApprovalID {
			found = true
		}
	}
	if !found {
		return value, refused("stage0_recovery_approval_operation")
	}
	if !recoveryApprovalTime(value, checkTime, time.Now()) {
		return value, refused("stage0_recovery_approval_time")
	}
	return value, nil
}

func recoveryApprovalTime(value RecoveryApproval, checkTime bool, now time.Time) bool {
	issued, e1 := time.Parse(time.RFC3339, value.IssuedAt)
	expires, e2 := time.Parse(time.RFC3339, value.ExpiresAt)
	return e1 == nil && e2 == nil && expires.After(issued) && (!checkTime || (!now.Before(issued) && now.Before(expires)))
}

func (l *loaded) loadRecovery() error {
	return l.loadRecoveryEvidence(true)
}

// A completion observes an already authorized invocation; it cannot authorize
// a new restore/start. Run and ticket admission keep the fresh-time check.
func (l *loaded) loadRecoveryCompletion() error {
	return l.loadRecoveryEvidence(false)
}

func (l *loaded) loadRecoveryEvidence(checkTime bool) error {
	if l.recovery != nil {
		return l.recheckRecovery()
	}
	raw, err := interpreterauthority.ReadProtected(l.recoveryPath(), 1<<20)
	if err != nil {
		return err
	}
	var r RecoveryRegistration
	if err = decodeExact(raw, &r); err != nil {
		return err
	}
	if r.Schema != "ags.routeb-stage0-recovery.v1" || r.RegistrationID != l.registration.ID || r.RegistrationSHA256 != l.registrationSHA ||
		r.HostInstance != l.host.HostInstance || r.ToolManifestSHA256 != l.registration.Binding.ToolManifestSHA256 ||
		r.InterpreterAuthoritySHA256 != l.registration.Interpreter.SHA256 || !interpreterauthority.ValidID(r.SnapshotID) || !validDigest(r.ManifestSHA256) || !validDigest(r.GroupSHA256) ||
		r.BackupDirectory != filepath.Join(filepath.Dir(l.host.Root), "recovery/backups", r.SnapshotID) || r.RestoreApproval.Path == r.OldStartApproval.Path || r.RestoreApproval.SHA256 == r.OldStartApproval.SHA256 {
		return refused("stage0_recovery_registration_binding")
	}
	l.recovery = &r
	l.recoverySHA = hash(raw)
	if _, err = l.readRecoveryApproval(r.RestoreApproval, "full-restore", checkTime); err != nil {
		return err
	}
	if _, err = l.readRecoveryApproval(r.OldStartApproval, "old-start", checkTime); err != nil {
		return err
	}
	return l.verifySnapshotReference()
}

func (l *loaded) verifySnapshotReference() error {
	r := l.recovery
	path := filepath.Join(r.BackupDirectory, "manifest.json")
	info, err := os.Lstat(path)
	if err != nil {
		return refused("stage0_recovery_snapshot_missing")
	}
	raw, err := readInput(interpreterauthority.File{Path: path, SHA256: r.ManifestSHA256, Size: info.Size()}, l.host.UID)
	if err != nil {
		return err
	}
	var manifest map[string]any
	if err = decodeExact(raw, &manifest); err != nil {
		return err
	}
	if manifest["schema"] != "ags.backup-manifest.v2" || manifest["authority_evidence"] == nil || manifest["snapshot_id"] != r.SnapshotID || manifest["recovery_group_sha256"] != r.GroupSHA256 {
		return refused("stage0_recovery_snapshot_binding")
	}
	binding, ok := manifest["binding"].(map[string]any)
	if !ok || binding["host_instance"] != l.host.HostInstance || binding["transaction_id"] != l.registration.ID {
		return refused("stage0_recovery_snapshot_binding")
	}
	return nil
}

func (l *loaded) recheckRecovery() error {
	raw, err := interpreterauthority.ReadProtected(l.recoveryPath(), 1<<20)
	if err != nil || hash(raw) != l.recoverySHA {
		return refused("stage0_recovery_registration_changed")
	}
	if _, err = l.readRecoveryApproval(l.recovery.RestoreApproval, "full-restore", false); err != nil {
		return err
	}
	if _, err = l.readRecoveryApproval(l.recovery.OldStartApproval, "old-start", false); err != nil {
		return err
	}
	return l.verifySnapshotReference()
}
