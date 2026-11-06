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
	l := s.loaded
	raw, err := interpreterauthority.ReadProtected(l.statePath("active", invocation.ID), 65536)
	if err != nil {
		return Context{}, err
	}
	var c Context
	if err = decodeExact(raw, &c); err != nil {
		return c, err
	}
	raw, err = interpreterauthority.ReadProtected(l.statePath("exits", invocation.ID), 65536)
	if err != nil {
		return c, err
	}
	var exit ExitRecord
	if err = decodeExact(raw, &exit); err != nil {
		return c, err
	}
	if c.RegistrationSHA256 != l.registrationSHA || c.RecoveryRegistrationSHA256 != l.recoverySHA || c.InvocationID != invocation.ID || c.Operation != invocation.Operation || c.Phase != invocation.Phase ||
		c.BootID != s.platform.boot || exit.Schema != "ags.routeb-stage0-exit.v1" || exit.RegistrationSHA256 != l.registrationSHA || exit.InvocationID != invocation.ID || exit.Phase != invocation.Phase ||
		exit.BootID != c.BootID || exit.Supervisor != c.Supervisor || exit.Child != c.Child || exit.ChildExitCode != 0 || exit.SignalForwarded || exit.ForcedKill || !exit.ContextVerified || !exit.AuthorityRechecked || !exit.NormalExit ||
		!processGone(c.Supervisor) || !processGone(c.Child) {
		return c, refused("stage0_restore_normal_exit_unproven")
	}
	properties, err := managerProperties(invocation.Unit, "MainPID", "ActiveState", "SubState", "ExecMainCode", "ExecMainStatus", "Result")
	if err != nil || properties["MainPID"] != "0" || properties["ActiveState"] != "inactive" || properties["SubState"] != "dead" || properties["ExecMainCode"] != "1" || properties["ExecMainStatus"] != "0" || properties["Result"] != "success" {
		return c, refused("stage0_restore_normal_exit_unproven")
	}
	group, err := cgroupMembers(invocation.Unit)
	if err != nil || len(group) != 0 {
		return c, refused("stage0_restore_cgroup_not_empty")
	}
	return c, nil
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
