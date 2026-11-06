package routebstage0

import (
	"encoding/hex"
	"strings"
)

// ManagerStart anchors the manager's invocation while the registered parent
// is still running. It is separate from Context so a saved context remains
// insufficient to admit a process or prove that its parent later exited.
type ManagerStart struct {
	Schema              string `json:"schema"`
	RegistrationSHA256  string `json:"registration_sha256"`
	InvocationID        string `json:"invocation_id"`
	Phase               string `json:"phase"`
	BootID              string `json:"boot_id"`
	Unit                string `json:"unit"`
	ManagerInvocationID string `json:"manager_invocation_id"`
	ContextSHA256       string `json:"context_sha256"`
}

type ManagerStopObservation struct {
	InvocationID   string `json:"invocation_id"`
	MainPID        int    `json:"main_pid"`
	ExecMainPID    int    `json:"exec_main_pid"`
	ControlPID     int    `json:"control_pid"`
	ExecMainCode   int    `json:"exec_main_code"`
	ExecMainStatus int    `json:"exec_main_status"`
	Result         string `json:"result"`
	ServiceResult  string `json:"service_result"`
	ExitCode       string `json:"exit_code"`
	ExitStatus     string `json:"exit_status"`
}

// ManagerCompletion is written only by the fixed ExecStopPost entry, after
// systemd has observed the main process exit and before inactive-unit GC can
// discard that status. Hashes cover the original files, not a re-encoding.
type ManagerCompletion struct {
	Schema              string                 `json:"schema"`
	RegistrationSHA256  string                 `json:"registration_sha256"`
	InvocationID        string                 `json:"invocation_id"`
	Phase               string                 `json:"phase"`
	BootID              string                 `json:"boot_id"`
	Unit                string                 `json:"unit"`
	ManagerInvocationID string                 `json:"manager_invocation_id"`
	StartAnchorSHA256   string                 `json:"start_anchor_sha256"`
	ContextSHA256       string                 `json:"context_sha256"`
	ExitSHA256          string                 `json:"exit_sha256"`
	Hook                ProcessIdentity        `json:"hook"`
	Manager             ManagerStopObservation `json:"manager"`
}

func validManagerInvocation(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 16 && value == strings.ToLower(value) && value != strings.Repeat("0", 32)
}

func managerStartInvocation(r Registration, operation, managerID string, anchors map[string]ManagerStart) (Invocation, error) {
	var selected Invocation
	if !validManagerInvocation(managerID) {
		return selected, refused("stage0_manager_invocation_invalid")
	}
	for _, i := range r.Invocations {
		anchor, exists := anchors[i.ID]
		if !exists || i.Operation != operation || anchor.ManagerInvocationID != managerID {
			continue
		}
		if selected.ID != "" || anchor.InvocationID != i.ID || anchor.Phase != i.Phase || anchor.Unit != i.Unit {
			return Invocation{}, refused("stage0_manager_invocation_ambiguous")
		}
		selected = i
	}
	if selected.ID == "" {
		return selected, refused("stage0_manager_invocation_missing")
	}
	return selected, nil
}

func (l *loaded) verifyManagerContext(i Invocation, context Context, boot string) error {
	recoverySHA := ""
	if i.Operation == "restore" || i.Operation == "old-start" || i.Phase == "serve-old" {
		recoverySHA = l.recoverySHA
		if !validDigest(recoverySHA) {
			return refused("stage0_manager_context_binding")
		}
	}
	grantSHA := ""
	if l.registration.ScopeGrant != nil {
		grantSHA = l.registration.ScopeGrant.SHA256
	}
	b := l.registration.Binding
	if context.Schema != "ags.routeb-stage0-context.v1" || context.RegistrationID != l.registration.ID ||
		context.RegistrationSHA256 != l.registrationSHA || context.Root != l.host.Root ||
		context.Operation != i.Operation || context.Phase != i.Phase || context.InvocationID != i.ID ||
		context.AttemptID != i.AttemptID || context.AuthorityEpoch != i.AuthorityEpoch || context.AuthorizationID != i.AuthorizationID ||
		context.BootID != boot || context.InterpreterAuthoritySHA256 != l.registration.Interpreter.SHA256 ||
		context.ToolRevision != b.ToolRevision || context.ToolTree != b.ToolTree || context.ToolManifestSHA256 != b.ToolManifestSHA256 ||
		context.RecoveryRegistrationSHA256 != recoverySHA || context.ScopeGrantSHA256 != grantSHA ||
		context.Supervisor.PID <= 1 || context.Supervisor.StartTicks == 0 || context.Supervisor.ParentPID != 1 ||
		context.Supervisor.UID != 0 || context.Supervisor.GID != 0 || context.Supervisor.ExecutableSHA256 != l.registration.NativeImage.SHA256 ||
		context.Child.PID <= 1 || context.Child.PID == context.Supervisor.PID || context.Child.StartTicks == 0 ||
		context.Child.ParentPID != context.Supervisor.PID || context.Child.UID != l.host.UID || context.Child.GID != l.host.GID ||
		!validDigest(context.Child.ExecutableSHA256) {
		return refused("stage0_manager_context_binding")
	}
	return nil
}

func validateManagerStart(anchor ManagerStart, context Context, contextRaw []byte, i Invocation) error {
	if anchor.Schema != "ags.routeb-stage0-manager-start.v1" || anchor.RegistrationSHA256 != context.RegistrationSHA256 ||
		anchor.InvocationID != context.InvocationID || anchor.Phase != context.Phase || anchor.BootID != context.BootID ||
		anchor.Unit != i.Unit || !validManagerInvocation(anchor.ManagerInvocationID) || anchor.ContextSHA256 != hash(contextRaw) {
		return refused("stage0_manager_start_binding")
	}
	return nil
}

func validateManagerExit(context Context, record ExitRecord, i Invocation) error {
	if record.Schema != "ags.routeb-stage0-exit.v1" || record.RegistrationSHA256 != context.RegistrationSHA256 ||
		record.InvocationID != context.InvocationID || record.Phase != context.Phase || record.BootID != context.BootID ||
		record.Supervisor != context.Supervisor || record.Child != context.Child || record.ChildExitCode != 0 ||
		record.SignalForwarded != (i.Operation == "serve") || record.ForcedKill ||
		!record.ContextVerified || !record.AuthorityRechecked || !record.NormalExit {
		return refused("stage0_manager_exit_binding")
	}
	return nil
}

func validateManagerStop(observation ManagerStopObservation, hook ProcessIdentity, context Context, anchor ManagerStart) error {
	if hook.PID <= 1 || hook.StartTicks == 0 || hook.ParentPID != 1 || hook.UID != 0 || hook.GID != 0 ||
		hook.PID == context.Supervisor.PID || hook.PID == context.Child.PID || hook.ExecutableSHA256 != context.Supervisor.ExecutableSHA256 ||
		observation.InvocationID != anchor.ManagerInvocationID || observation.MainPID != 0 ||
		observation.ExecMainPID != context.Supervisor.PID || observation.ControlPID != hook.PID ||
		observation.ExecMainCode != 1 || observation.ExecMainStatus != 0 || observation.Result != "success" ||
		observation.ServiceResult != "success" || observation.ExitCode != "exited" || observation.ExitStatus != "0" {
		return refused("stage0_manager_completion_unproven")
	}
	return nil
}

func validateManagerCompletion(value ManagerCompletion, anchor ManagerStart, context Context, record ExitRecord,
	anchorRaw, contextRaw, exitRaw []byte, i Invocation) error {
	if err := validateManagerStart(anchor, context, contextRaw, i); err != nil {
		return err
	}
	if err := validateManagerExit(context, record, i); err != nil {
		return err
	}
	if value.Schema != "ags.routeb-stage0-manager-completion.v1" || value.RegistrationSHA256 != anchor.RegistrationSHA256 ||
		value.InvocationID != anchor.InvocationID || value.Phase != anchor.Phase || value.BootID != anchor.BootID || value.Unit != anchor.Unit ||
		value.ManagerInvocationID != anchor.ManagerInvocationID || value.StartAnchorSHA256 != hash(anchorRaw) ||
		value.ContextSHA256 != hash(contextRaw) || value.ExitSHA256 != hash(exitRaw) {
		return refused("stage0_manager_completion_binding")
	}
	return validateManagerStop(value.Manager, value.Hook, context, anchor)
}

// Durable exit evidence and present quiescence are both required. Only the
// former supplies an exit code: systemd may have garbage-collected that code
// by the time this independent operation consumes it.
func validateStoppedManager(value ManagerCompletion, anchor ManagerStart, context Context, record ExitRecord,
	anchorRaw, contextRaw, exitRaw []byte, i Invocation, properties map[string]string, processesGone bool, members []int) error {
	if err := validateManagerCompletion(value, anchor, context, record, anchorRaw, contextRaw, exitRaw, i); err != nil {
		return err
	}
	if !processesGone {
		return refused("stage0_manager_process_not_exited")
	}
	if properties["MainPID"] != "0" || properties["ActiveState"] != "inactive" || properties["SubState"] != "dead" || properties["Result"] != "success" {
		return refused("stage0_manager_not_stopped")
	}
	if len(members) != 0 {
		return refused("stage0_manager_cgroup_not_empty")
	}
	return nil
}

func managerStopCommand(r Registration, i Invocation) string {
	return r.NativeImage.Path + " manager-complete --registration " + r.ID + " --operation " + i.Operation
}

// systemctl renders an ExecCommand as one brace-delimited record. The fixed
// path, argv and ignore-errors policy must match; only its observed timestamps
// and process status may change. Additional commands or fields are rejected.
func validEffectiveStopPost(value, command string) bool {
	if strings.ContainsAny(value, "\r\n\t") || strings.Count(value, "{") != 1 || strings.Count(value, "}") != 1 ||
		!strings.HasPrefix(value, "{ ") || !strings.HasSuffix(value, " }") {
		return false
	}
	fields := strings.Split(strings.TrimSuffix(strings.TrimPrefix(value, "{ "), " }"), " ; ")
	path, _, _ := strings.Cut(command, " ")
	if len(fields) != 8 || fields[0] != "path="+path || fields[1] != "argv[]="+command || fields[2] != "ignore_errors=no" {
		return false
	}
	for index, key := range []string{"start_time", "stop_time", "pid", "code", "status"} {
		name, text, ok := strings.Cut(fields[index+3], "=")
		if !ok || name != key || text == "" || strings.Contains(text, ";") {
			return false
		}
	}
	return true
}
