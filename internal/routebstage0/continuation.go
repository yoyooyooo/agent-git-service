package routebstage0

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
)

// Continuation has its own late, write-once authority. It must not consume the
// independent full-restore/old-start registration if a later start fails.
type ContinuationSubject struct {
	RegistrationSHA256      string `json:"registration_sha256"`
	TransactionInvocationID string `json:"transaction_invocation_id"`
	HostInstance            string `json:"host_instance"`
	BindingSHA256           string `json:"binding_sha256"`
	Generation              string `json:"generation"`
	HostJournalSHA256       string `json:"host_journal_sha256"`
	ColdJournalSHA256       string `json:"cold_journal_sha256"`
	SnapshotID              string `json:"snapshot_id"`
	ManifestSHA256          string `json:"manifest_sha256"`
	GroupSHA256             string `json:"group_sha256"`
}

func (s ContinuationSubject) digest() string {
	raw, _ := json.Marshal(map[string]string{
		"registration_sha256": s.RegistrationSHA256, "transaction_invocation_id": s.TransactionInvocationID,
		"host_instance": s.HostInstance, "binding_sha256": s.BindingSHA256, "generation": s.Generation,
		"host_journal_sha256": s.HostJournalSHA256, "cold_journal_sha256": s.ColdJournalSHA256,
		"snapshot_id": s.SnapshotID, "manifest_sha256": s.ManifestSHA256, "group_sha256": s.GroupSHA256,
	})
	return hash(append(raw, '\n'))
}

type ContinuationRegistration struct {
	Schema          string                    `json:"schema"`
	RegistrationID  string                    `json:"registration_id"`
	Subject         ContinuationSubject       `json:"subject"`
	BackupDirectory string                    `json:"backup_directory"`
	Approval        interpreterauthority.File `json:"approval"`
}

type ContinuationApproval struct {
	Schema        string      `json:"schema"`
	ApprovalID    string      `json:"approval_id"`
	Actor         string      `json:"actor"`
	Purpose       string      `json:"purpose"`
	Decision      string      `json:"decision"`
	IssuedAt      string      `json:"issued_at"`
	ExpiresAt     string      `json:"expires_at"`
	SubjectSHA256 string      `json:"subject_sha256"`
	Delegation    *Delegation `json:"delegation,omitempty"`
}

func (a ContinuationApproval) subject() string {
	raw, _ := json.Marshal(map[string]string{"approval_id": a.ApprovalID, "purpose": a.Purpose, "subject_sha256": a.SubjectSHA256})
	return hash(append(raw, '\n'))
}

func (l *loaded) continuationPath() string {
	return filepath.Join(rootFor(l.registration.ID), "continuation-registration.json")
}

func invocationForPhase(r Registration, phase string) (Invocation, error) {
	for _, i := range r.Invocations {
		if i.Phase == phase {
			return i, nil
		}
	}
	return Invocation{}, refused("stage0_continuation_unregistered")
}

func (l *loaded) validateContinuation(r ContinuationRegistration) error {
	s := r.Subject
	transaction, err := invocationForPhase(l.registration, "transaction")
	if err != nil || transaction.Operation != "transaction" || transaction.ID != s.TransactionInvocationID {
		return refused("stage0_continuation_transaction_binding")
	}
	for _, phase := range []string{"continue-installed", "serve-initial", "serve-candidate"} {
		if _, err = invocationForPhase(l.registration, phase); err != nil {
			return err
		}
	}
	if r.Schema != "ags.routeb-stage0-continuation.v1" || r.RegistrationID != l.registration.ID ||
		s.RegistrationSHA256 != l.registrationSHA || s.HostInstance != l.host.HostInstance ||
		!interpreterauthority.ValidID(s.Generation) || !interpreterauthority.ValidID(s.SnapshotID) ||
		!validDigest(s.BindingSHA256) || !validDigest(s.HostJournalSHA256) || !validDigest(s.ColdJournalSHA256) ||
		!validDigest(s.ManifestSHA256) || !validDigest(s.GroupSHA256) ||
		r.BackupDirectory != filepath.Join(filepath.Dir(l.host.Root), "recovery/backups", s.SnapshotID) ||
		r.Approval.Path != filepath.Join(filepath.Dir(l.host.Root), "continuation/approval.json") || !validFile(r.Approval, 1<<20) {
		return refused("stage0_continuation_registration_binding")
	}
	return nil
}

func (l *loaded) readContinuationApproval(checkTime bool) (ContinuationApproval, error) {
	var a ContinuationApproval
	raw, err := readInput(l.continuation.Approval, l.host.UID)
	if err != nil {
		return a, err
	}
	if err = decodeExact(raw, &a); err != nil {
		return a, err
	}
	i, err := invocationForPhase(l.registration, "continue-installed")
	if err != nil || a.ApprovalID != i.AuthorizationID || a.Purpose != "continue-installed" || a.Decision != "approve" ||
		a.SubjectSHA256 != l.continuation.Subject.digest() ||
		!l.approvalActor(a.Schema, "ags.routeb-continuation-approval.v1", "ags.routeb-continuation-approval.v2",
			a.Actor, a.IssuedAt, a.subject(), a.Delegation, checkTime) {
		return a, refused("stage0_continuation_approval_binding")
	}
	if !approvalTime(a.IssuedAt, a.ExpiresAt, checkTime, time.Now()) {
		return a, refused("stage0_continuation_approval_time")
	}
	return a, nil
}

// Time fences a new invocation/ticket/start, not observation of an already
// authorized generation. Every caller still checks exact late-file bytes.
func (l *loaded) loadContinuation(checkTime bool) error {
	raw, err := interpreterauthority.ReadProtected(l.continuationPath(), 1<<20)
	if err != nil {
		return err
	}
	if l.continuation != nil && hash(raw) != l.continuationSHA {
		return refused("stage0_continuation_registration_changed")
	}
	var r ContinuationRegistration
	if err = decodeExact(raw, &r); err != nil {
		return err
	}
	if err = l.validateContinuation(r); err != nil {
		return err
	}
	l.continuation, l.continuationSHA = &r, hash(raw)
	if _, err = l.readContinuationApproval(checkTime); err != nil {
		return err
	}
	path := filepath.Join(r.BackupDirectory, "manifest.json")
	manifest, err := l.readContinuationDocument(path, r.Subject.ManifestSHA256)
	if err != nil {
		return err
	}
	binding, ok := manifest["binding"].(map[string]any)
	encoded, _ := json.Marshal(binding)
	if !ok || manifest["schema"] != "ags.backup-manifest.v2" || manifest["authority_evidence"] == nil ||
		manifest["snapshot_id"] != r.Subject.SnapshotID || manifest["recovery_group_sha256"] != r.Subject.GroupSHA256 ||
		hash(append(encoded, '\n')) != r.Subject.BindingSHA256 || binding["host_instance"] != r.Subject.HostInstance ||
		binding["transaction_id"] != r.RegistrationID {
		return refused("stage0_continuation_snapshot_binding")
	}
	return nil
}

func (l *loaded) readContinuationDocument(path, sha string) (map[string]any, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, refused("stage0_continuation_document_missing")
	}
	raw, err := readInput(interpreterauthority.File{Path: path, SHA256: sha, Size: info.Size()}, l.host.UID)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err = decodeExact(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func (l *loaded) verifyContinuationWindow(archived bool) error {
	s := l.continuation.Subject
	pending := filepath.Join(l.host.Root, "state/ags-autodeploy/local-v2/pending")
	hostPath, coldPath := filepath.Join(pending, "host-execution.json"), filepath.Join(pending, "cold-journal.json")
	if archived {
		hostPath, coldPath = filepath.Join(pending, "continuation/original-host-execution.json"), filepath.Join(pending, "continuation/original-cold-journal.json")
	}
	host, err := l.readContinuationDocument(hostPath, s.HostJournalSHA256)
	if err != nil {
		return err
	}
	cold, err := l.readContinuationDocument(coldPath, s.ColdJournalSHA256)
	if err != nil {
		return err
	}
	if host["schema"] != "ags.routeb-host-journal.v1" || host["root"] != l.host.Root ||
		host["transaction_id"] != l.registration.ID || host["host_instance"] != l.host.HostInstance ||
		host["phase"] != "installed" || host["status"] != "failed" || host["start_attempted"] != false ||
		host["binding_sha256"] != s.BindingSHA256 ||
		l.registration.Inputs.Package == nil || l.registration.Inputs.Approval == nil ||
		host["package_sha256"] != l.registration.Inputs.Package.SHA256 || host["approval_sha256"] != l.registration.Inputs.Approval.SHA256 ||
		cold["schema"] != "ags.cold-journal.v1" || cold["phase"] != "installed" || cold["start_attempted"] != false ||
		cold["generation"] != s.Generation || cold["binding_sha256"] != s.BindingSHA256 ||
		cold["backup_dir"] != l.continuation.BackupDirectory {
		return refused("stage0_continuation_window_binding")
	}
	for _, key := range []string{"handoff", "start_called"} {
		if _, exists := cold[key]; exists {
			return refused("stage0_continuation_start_unknown")
		}
	}
	return nil
}

// Select authority for the observed target, not the controller doing the
// observation. A restore controller can observe a continuation-issued candidate.
func (l *loaded) invocationLateHash(i Invocation, checkTime bool) (string, error) {
	if i.Phase == "serve-candidate" || i.Phase == "serve-old" {
		ticket, err := l.readTicketEvidence(i, checkTime)
		return ticket.RecoveryRegistrationSHA256, err
	}
	if i.Operation == "continue-installed" {
		if err := l.loadContinuation(checkTime); err != nil {
			return "", err
		}
		return l.continuationSHA, nil
	}
	if i.Operation == "restore" || i.Operation == "old-start" {
		if checkTime || !validDigest(l.recoverySHA) {
			op := ""
			if checkTime {
				op = i.Operation
			}
			if err := l.loadRecoveryEvidence(op); err != nil {
				return "", err
			}
		}
		return l.recoverySHA, nil
	}
	return "", nil
}
