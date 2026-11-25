package routebstage0

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
	"golang.org/x/sys/unix"
)

type Ticket struct {
	Schema                     string `json:"schema"`
	RegistrationSHA256         string `json:"registration_sha256"`
	InvocationID               string `json:"invocation_id"`
	Phase                      string `json:"phase"`
	AttemptID                  string `json:"attempt_id"`
	AuthorityEpoch             uint64 `json:"authority_epoch"`
	AuthorizationID            string `json:"authorization_id"`
	IssuedByInvocationID       string `json:"issued_by_invocation_id"`
	RecoveryRegistrationSHA256 string `json:"recovery_registration_sha256"`
}

type Claim struct {
	Schema             string          `json:"schema"`
	RegistrationSHA256 string          `json:"registration_sha256"`
	Invocation         Invocation      `json:"invocation"`
	Supervisor         ProcessIdentity `json:"supervisor"`
	BootID             string          `json:"boot_id"`
}

func (l *loaded) statePath(kind, name string) string {
	return filepath.Join(rootFor(l.registration.ID), "state", kind, name+".json")
}

func writeExclusive(path string, value any) error {
	if err := interpreterauthority.ProtectedDirectory(filepath.Dir(path), true); err != nil {
		return err
	}
	return writeExclusiveFile(path, value)
}

// The production entry above always verifies the root-private parent first.
// Keep the exclusive, durable file operation separable for an ordinary-user
// filesystem regression without creating a root-owned test namespace.
func writeExclusiveFile(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return refused("stage0_state_encoding")
	}
	raw = append(raw, '\n')
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return refused("stage0_state_exists_or_unavailable")
	}
	f := os.NewFile(uintptr(fd), path)
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return refused("stage0_state_durability_unknown")
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return refused("stage0_state_durability_unknown")
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return refused("stage0_state_durability_unknown")
	}
	return nil
}

func (l *loaded) withStateLock(fn func() error) error {
	base := filepath.Join(rootFor(l.registration.ID), "state")
	if err := interpreterauthority.ProtectedDirectory(base, true); err != nil {
		return err
	}
	fd, err := unix.Open(filepath.Join(base, "lock"), unix.O_WRONLY|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return refused("stage0_state_lock_unavailable")
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != 0 || st.Mode&07777 != 0600 || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return refused("stage0_state_lock_unsafe")
	}
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return refused("stage0_state_busy")
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	return fn()
}

func absentFile(path string) (bool, error) {
	_, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, refused("stage0_state_unknown")
	}
	return false, nil
}

func (l *loaded) readTicket(i Invocation) (Ticket, error) {
	return l.readTicketEvidence(i, true)
}

func ticketPurpose(r Registration, i Invocation, t Ticket) (string, error) {
	if i.Phase == "serve-initial" {
		if t.IssuedByInvocationID == "provision" {
			return "", nil
		}
	} else {
		for _, issuer := range r.Invocations {
			if issuer.ID != t.IssuedByInvocationID {
				continue
			}
			if i.Phase == "serve-old" && issuer.Operation == "old-start" {
				return "old-start", nil
			}
			if i.Phase == "serve-candidate" && (issuer.Operation == "transaction" || issuer.Operation == "continue-installed") {
				return issuer.Operation, nil
			}
		}
	}
	return "", refused("stage0_ticket_issuer")
}

func (l *loaded) readTicketEvidence(i Invocation, checkTime bool) (Ticket, error) {
	raw, err := interpreterauthority.ReadProtected(l.statePath("tickets", i.ID), 65536)
	if err != nil {
		return Ticket{}, err
	}
	var t Ticket
	if err = decodeExact(raw, &t); err != nil {
		return t, err
	}
	if t.Schema != "ags.routeb-stage0-ticket.v1" || t.RegistrationSHA256 != l.registrationSHA || t.InvocationID != i.ID || t.Phase != i.Phase ||
		t.AttemptID != i.AttemptID || t.AuthorityEpoch != i.AuthorityEpoch || t.AuthorizationID != i.AuthorizationID {
		return t, refused("stage0_ticket_binding")
	}
	purpose, err := ticketPurpose(l.registration, i, t)
	if err != nil {
		return t, err
	}
	expected := ""
	if purpose == "old-start" {
		op := ""
		if checkTime {
			op = "old-start"
		}
		if err = l.loadRecoveryEvidence(op); err != nil {
			return t, err
		}
		expected = l.recoverySHA
	} else if purpose == "continue-installed" {
		if err = l.loadContinuation(checkTime); err != nil {
			return t, err
		}
		expected = l.continuationSHA
	}
	if t.RecoveryRegistrationSHA256 != expected {
		return t, refused("stage0_ticket_recovery_binding")
	}
	return t, nil
}

func (l *loaded) claim(operation string, supervisor ProcessIdentity, boot string) error {
	return l.withStateLock(func() error {
		matches := []Invocation{}
		if operation == "serve" {
			var err error
			matches, err = selectServeInvocations(l.registration.Invocations, l.claimed, func(i Invocation, current bool) (bool, error) {
				missing, err := absentFile(l.statePath("tickets", i.ID))
				if err != nil || missing {
					return false, err
				}
				_, err = l.readTicketEvidence(i, current)
				return true, err
			})
			if err != nil {
				return err
			}
		} else {
			for _, i := range l.registration.Invocations {
				if i.Operation == operation {
					matches = append(matches, i)
				}
			}
		}
		if len(matches) != 1 {
			return refused("stage0_invocation_missing_ambiguous_or_consumed")
		}
		l.invocation = matches[0]
		return writeExclusive(l.statePath("claims", l.invocation.ID), Claim{"ags.routeb-stage0-claim.v1", l.registrationSHA, l.invocation, supervisor, boot})
	})
}

// Claimed tickets are historical identity, never fresh start permission. Keep
// this selection shared with the filesystem adapter so expired consumed
// candidates cannot block a separately approved old-start slot.
func selectServeInvocations(invocations []Invocation, claimed func(Invocation) (bool, error), ticket func(Invocation, bool) (bool, error)) ([]Invocation, error) {
	var result []Invocation
	for _, i := range invocations {
		if i.Operation != "serve" {
			continue
		}
		consumed, err := claimed(i)
		if err != nil {
			return nil, err
		}
		exists, err := ticket(i, !consumed)
		if err != nil {
			return nil, err
		}
		if consumed && !exists {
			return nil, refused("stage0_claim_ticket_missing")
		}
		if exists && !consumed {
			result = append(result, i)
		}
	}
	return result, nil
}

func validateClaim(r Registration, registrationSHA string, i Invocation, claim Claim) error {
	p := claim.Supervisor
	if claim.Schema != "ags.routeb-stage0-claim.v1" || claim.RegistrationSHA256 != registrationSHA || claim.Invocation != i ||
		len(claim.BootID) != 36 || p.PID <= 1 || p.StartTicks == 0 || p.ParentPID != 1 || p.UID != 0 || p.GID != 0 || p.ExecutableSHA256 != r.NativeImage.SHA256 {
		return refused("stage0_claim_binding")
	}
	return nil
}

func (l *loaded) claimed(i Invocation) (bool, error) {
	path := l.statePath("claims", i.ID)
	missing, err := absentFile(path)
	if err != nil || missing {
		return false, err
	}
	raw, err := interpreterauthority.ReadProtected(path, 65536)
	var claim Claim
	if err != nil {
		return false, err
	}
	if err = decodeExact(raw, &claim); err != nil {
		return false, err
	}
	return true, validateClaim(l.registration, l.registrationSHA, i, claim)
}

func processGone(identity ProcessIdentity) bool {
	err := syscall.Kill(identity.PID, 0)
	return err == syscall.ESRCH
}
