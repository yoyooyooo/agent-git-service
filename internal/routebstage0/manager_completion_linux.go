//go:build linux

package routebstage0

import (
	"bytes"
	"os"
	"strconv"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
)

func (s *supervisor) recordManagerStart() error {
	l, i := s.loaded, s.loaded.invocation
	raw, err := interpreterauthority.ReadProtected(l.statePath("active", i.ID), 65536)
	if err != nil {
		return err
	}
	var context Context
	if err = decodeExact(raw, &context); err != nil || context != s.context {
		return refused("stage0_manager_context_binding")
	}
	if err = l.verifyManagerContext(i, context, s.platform.boot); err != nil {
		return err
	}
	properties, err := managerProperties(i.Unit, "InvocationID", "MainPID", "ActiveState", "SubState")
	managerID := properties["InvocationID"]
	if err != nil || !validManagerInvocation(managerID) || managerID != os.Getenv("INVOCATION_ID") ||
		properties["MainPID"] != decimal(s.parent.PID) || properties["ActiveState"] != "active" || properties["SubState"] != "running" {
		return refused("stage0_manager_start_unproven")
	}
	anchor := ManagerStart{"ags.routeb-stage0-manager-start.v1", l.registrationSHA, i.ID, i.Phase, s.platform.boot, i.Unit, managerID, hash(raw)}
	return writeExclusive(l.statePath("manager-start", i.ID), anchor)
}

func (l *loaded) readManagerRecords(i Invocation, boot string) (Context, ExitRecord, ManagerStart, []byte, []byte, []byte, error) {
	var context Context
	var exit ExitRecord
	var anchor ManagerStart
	contextRaw, err := interpreterauthority.ReadProtected(l.statePath("active", i.ID), 65536)
	if err != nil {
		return context, exit, anchor, nil, nil, nil, err
	}
	exitRaw, err := interpreterauthority.ReadProtected(l.statePath("exits", i.ID), 65536)
	if err != nil {
		return context, exit, anchor, nil, nil, nil, err
	}
	anchorRaw, err := interpreterauthority.ReadProtected(l.statePath("manager-start", i.ID), 65536)
	if err != nil {
		return context, exit, anchor, nil, nil, nil, err
	}
	if decodeExact(contextRaw, &context) != nil || decodeExact(exitRaw, &exit) != nil || decodeExact(anchorRaw, &anchor) != nil {
		return context, exit, anchor, nil, nil, nil, refused("stage0_manager_records_invalid")
	}
	if err = l.verifyManagerContext(i, context, boot); err == nil {
		err = validateManagerStart(anchor, context, contextRaw, i)
	}
	if err == nil {
		err = validateManagerExit(context, exit, i)
	}
	return context, exit, anchor, contextRaw, exitRaw, anchorRaw, err
}

func stopObservation(properties map[string]string) (ManagerStopObservation, error) {
	value := ManagerStopObservation{InvocationID: properties["InvocationID"], Result: properties["Result"],
		ServiceResult: os.Getenv("SERVICE_RESULT"), ExitCode: os.Getenv("EXIT_CODE"), ExitStatus: os.Getenv("EXIT_STATUS")}
	for name, target := range map[string]*int{"MainPID": &value.MainPID, "ExecMainPID": &value.ExecMainPID,
		"ControlPID": &value.ControlPID, "ExecMainCode": &value.ExecMainCode, "ExecMainStatus": &value.ExecMainStatus} {
		number, err := strconv.Atoi(properties[name])
		if err != nil || number < 0 || decimal(number) != properties[name] {
			return value, refused("stage0_manager_completion_unproven")
		}
		*target = number
	}
	if value.InvocationID != os.Getenv("INVOCATION_ID") {
		return value, refused("stage0_manager_invocation_changed")
	}
	return value, nil
}

// CompleteManager is a separate PID1 control-process entry. It never enters
// Run, consumes a claim/ticket, starts a child, or selects an invocation by
// order or mtime. The shared serve unit is resolved through its real manager ID.
func CompleteManager(id, operation string) error {
	l, err := load(id)
	if err != nil {
		return err
	}
	facts, self, err := platformPrerequisites(l)
	if err != nil {
		return err
	}
	unit := unitName(id, operation)
	properties, err := managerProperties(unit, "InvocationID", "MainPID", "ExecMainPID", "ControlPID", "ExecMainCode", "ExecMainStatus", "Result")
	if err != nil {
		return err
	}
	observation, err := stopObservation(properties)
	if err != nil {
		return err
	}
	anchors := map[string]ManagerStart{}
	for _, candidate := range l.registration.Invocations {
		if candidate.Operation != operation {
			continue
		}
		path := l.statePath("manager-start", candidate.ID)
		missing, err := absentFile(path)
		if err != nil {
			return err
		}
		if missing {
			continue
		}
		raw, err := interpreterauthority.ReadProtected(path, 65536)
		var anchor ManagerStart
		if err != nil || decodeExact(raw, &anchor) != nil {
			return refused("stage0_manager_start_invalid")
		}
		anchors[candidate.ID] = anchor
	}
	i, err := managerStartInvocation(l.registration, operation, observation.InvocationID, anchors)
	if err != nil {
		return err
	}
	l.invocation = i
	if i.Operation == "restore" || i.Operation == "old-start" || i.Phase == "serve-old" {
		if err = l.loadRecoveryCompletion(); err != nil {
			return err
		}
	}
	if err = l.verifyUnit(i, false); err != nil {
		return err
	}
	context, exit, anchor, contextRaw, exitRaw, anchorRaw, err := l.readManagerRecords(i, facts.boot)
	if err != nil {
		return err
	}
	if err = validateManagerStop(observation, self, context, anchor); err != nil {
		return err
	}
	if !processGone(context.Supervisor) || !processGone(context.Child) {
		return refused("stage0_manager_parent_not_exited")
	}
	// This hook itself occupies the service cgroup. Empty-cgroup confirmation
	// belongs to the later consumer, after this control process has exited.
	if err = l.recheckFiles(); err != nil {
		return err
	}
	fresh, err := managerProperties(unit, "InvocationID", "MainPID", "ExecMainPID", "ControlPID", "ExecMainCode", "ExecMainStatus", "Result")
	if err != nil {
		return err
	}
	for key, previous := range properties {
		if fresh[key] != previous {
			return refused("stage0_manager_completion_changed")
		}
	}
	now, err := processIdentity(os.Getpid(), l.registration.NativeImage.Path, l.registration.NativeImage.SHA256)
	if err != nil || now != self {
		return refused("stage0_manager_hook_changed")
	}
	for path, original := range map[string][]byte{l.statePath("active", i.ID): contextRaw, l.statePath("exits", i.ID): exitRaw, l.statePath("manager-start", i.ID): anchorRaw} {
		raw, err := interpreterauthority.ReadProtected(path, 65536)
		if err != nil || !bytes.Equal(raw, original) {
			return refused("stage0_manager_records_changed")
		}
	}
	completion := ManagerCompletion{"ags.routeb-stage0-manager-completion.v1", l.registrationSHA, i.ID, i.Phase, facts.boot, i.Unit,
		anchor.ManagerInvocationID, hash(anchorRaw), hash(contextRaw), hash(exitRaw), self, observation}
	if err = validateManagerCompletion(completion, anchor, context, exit, anchorRaw, contextRaw, exitRaw, i); err != nil {
		return err
	}
	return writeExclusive(l.statePath("manager-completion", i.ID), completion)
}

func (l *loaded) completedManager(i Invocation, boot string) (Context, ExitRecord, ManagerCompletion, error) {
	context, exit, anchor, contextRaw, exitRaw, anchorRaw, err := l.readManagerRecords(i, boot)
	var completion ManagerCompletion
	if err != nil {
		return context, exit, completion, err
	}
	raw, err := interpreterauthority.ReadProtected(l.statePath("manager-completion", i.ID), 65536)
	if err != nil {
		return context, exit, completion, err
	}
	if err = decodeExact(raw, &completion); err != nil {
		return context, exit, completion, err
	}
	if err = validateManagerCompletion(completion, anchor, context, exit, anchorRaw, contextRaw, exitRaw, i); err != nil {
		return context, exit, completion, err
	}
	properties, err := managerProperties(i.Unit, "MainPID", "ActiveState", "SubState", "Result")
	if err != nil {
		return context, exit, completion, refused("stage0_manager_not_stopped")
	}
	members, err := cgroupMembers(i.Unit)
	if err != nil {
		return context, exit, completion, refused("stage0_manager_cgroup_not_empty")
	}
	err = validateStoppedManager(completion, anchor, context, exit, anchorRaw, contextRaw, exitRaw, i, properties,
		processGone(context.Supervisor) && processGone(context.Child) && processGone(completion.Hook), members)
	return context, exit, completion, err
}
