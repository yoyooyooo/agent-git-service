//go:build linux

package routebstage0

import (
	"encoding/json"
	"os"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
)

func contextDigest(value Context) string {
	raw, _ := json.Marshal(value)
	return hash(append(raw, '\n'))
}

func (s *supervisor) completeRestore(resultSHA string) (RestoreCompletion, error) {
	l := s.loaded
	if l.invocation.Operation != "restore" || l.recovery == nil || !validDigest(resultSHA) || s.stopped == nil || s.restoreSealed {
		return RestoreCompletion{}, refused("stage0_restore_completion_invalid")
	}
	if _, err := s.runtimeExit(s.stopped.Context.InvocationID); err != nil {
		return RestoreCompletion{}, err
	}
	for _, pid := range s.stopped.PIDs {
		if !processGone(pid) {
			return RestoreCompletion{}, refused("stage0_descendant_still_present")
		}
	}
	info, err := os.Lstat(l.restoreResultPath())
	if err != nil {
		return RestoreCompletion{}, refused("stage0_restore_result_missing")
	}
	file := interpreterauthority.File{Path: l.restoreResultPath(), SHA256: resultSHA, Size: info.Size()}
	if err = l.verifyRestoreArtifact(file); err != nil {
		return RestoreCompletion{}, err
	}
	value := RestoreCompletion{"ags.routeb-stage0-restoration.v1", l.registrationSHA, l.recoverySHA, l.invocation.ID, s.stopped.Context.InvocationID, s.platform.boot, contextDigest(s.context), file}
	if err = writeExclusive(l.statePath("recovery", l.invocation.ID), value); err != nil {
		return RestoreCompletion{}, err
	}
	s.restoreSealed = true
	return value, nil
}

// A control process must have actually exited normally before its root-sealed
// work can be consumed by another operation. A before-exit record alone does
// not prove systemd observed the parent exit or that the cgroup drained.
func (s *supervisor) completedControl(invocation Invocation) (Context, error) {
	context, _, _, err := s.loaded.completedManager(invocation, s.platform.boot)
	return context, err
}

func (s *supervisor) restoreResult() (RestoreCompletion, error) {
	l := s.loaded
	if l.recovery == nil || !(l.invocation.Operation == "old-start" || l.invocation.Phase == "serve-old") {
		return RestoreCompletion{}, refused("stage0_restore_consumer_invalid")
	}
	var invocation *Invocation
	for _, i := range l.registration.Invocations {
		if i.Operation == "restore" {
			copy := i
			invocation = &copy
		}
	}
	if invocation == nil {
		return RestoreCompletion{}, refused("stage0_restore_unregistered")
	}
	raw, err := interpreterauthority.ReadProtected(l.statePath("recovery", invocation.ID), 65536)
	if err != nil {
		return RestoreCompletion{}, err
	}
	var value RestoreCompletion
	if err = decodeExact(raw, &value); err != nil {
		return value, err
	}
	if value.Schema != "ags.routeb-stage0-restoration.v1" || value.RegistrationSHA256 != l.registrationSHA || value.RecoveryRegistrationSHA256 != l.recoverySHA || value.RestoreInvocationID != invocation.ID || value.BootID != s.platform.boot {
		return value, refused("stage0_restore_completion_binding")
	}
	stopped, err := s.serveInvocation(value.StoppedInvocationID)
	if err != nil || stopped.Phase != "serve-candidate" {
		return value, refused("stage0_restore_stopped_phase")
	}
	c, err := s.completedControl(*invocation)
	if err != nil {
		return value, err
	}
	if contextDigest(c) != value.ContextSHA256 {
		return value, refused("stage0_restore_completion_binding")
	}
	if err = l.verifyRestoreArtifact(value.Result); err != nil {
		return value, err
	}
	return value, nil
}
