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
	if i.Phase == "serve-initial" {
		if t.IssuedByInvocationID != "provision" {
			return t, refused("stage0_ticket_issuer")
		}
	} else {
		wanted := "transaction"
		if i.Phase == "serve-old" {
			wanted = "old-start"
		}
		found := false
		for _, issuer := range l.registration.Invocations {
			if issuer.ID == t.IssuedByInvocationID && issuer.Operation == wanted {
				found = true
			}
		}
		if !found {
			return t, refused("stage0_ticket_issuer")
		}
	}
	if i.Phase == "serve-old" {
		if err := l.loadRecovery(); err != nil {
			return t, err
		}
		if t.RecoveryRegistrationSHA256 != l.recoverySHA {
			return t, refused("stage0_ticket_recovery_binding")
		}
	} else if t.RecoveryRegistrationSHA256 != "" {
		return t, refused("stage0_ticket_recovery_binding")
	}
	return t, nil
}

func (l *loaded) claim(operation string, supervisor ProcessIdentity, boot string) error {
	return l.withStateLock(func() error {
		matches := []Invocation{}
		for _, i := range l.registration.Invocations {
			if i.Operation != operation {
				continue
			}
			if operation == "serve" {
				missing, err := absentFile(l.statePath("tickets", i.ID))
				if err != nil {
					return err
				}
				if missing {
					continue
				}
				if _, err = l.readTicket(i); err != nil {
					return err
				}
				unclaimed, err := absentFile(l.statePath("claims", i.ID))
				if err != nil {
					return err
				}
				if !unclaimed {
					continue
				}
			}
			matches = append(matches, i)
		}
		if len(matches) != 1 {
			return refused("stage0_invocation_missing_ambiguous_or_consumed")
		}
		l.invocation = matches[0]
		return writeExclusive(l.statePath("claims", l.invocation.ID), Claim{"ags.routeb-stage0-claim.v1", l.registrationSHA, l.invocation, supervisor, boot})
	})
}

func processGone(identity ProcessIdentity) bool {
	err := syscall.Kill(identity.PID, 0)
	return err == syscall.ESRCH
}
