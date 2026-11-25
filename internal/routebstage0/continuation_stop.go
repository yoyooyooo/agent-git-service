package routebstage0

// StoppedIdentity is a fresh root-peer readback, not a user-supplied stop claim.
// The original stop files and failed controllers remain immutable in the root
// state directory; no original operation slot is refunded by this response.
type StoppedIdentity struct {
	Schema                      string            `json:"schema"`
	Runtime                     RuntimeIdentity   `json:"runtime"`
	Exit                        ExitRecord        `json:"exit"`
	ManagerCompletion           ManagerCompletion `json:"manager_completion"`
	TransactionInvocationID     string            `json:"transaction_invocation_id"`
	FailedControllerInvocations []string          `json:"failed_controller_invocations"`
}

func validateOriginalStop(before RuntimeIdentity, result ExitRecord, context Context, exit ExitRecord, i Invocation) error {
	if i.Phase != "serve-initial" || before.Context != context || before.ManagerUnit != i.Unit || result != exit ||
		len(before.PIDs) < 2 || len(before.PIDs) > 256 {
		return refused("stage0_original_stop_binding")
	}
	seen := map[int]bool{}
	parent, child := false, false
	for _, p := range before.PIDs {
		if p.PID <= 1 || p.StartTicks == 0 || !validDigest(p.ExecutableSHA256) || seen[p.PID] {
			return refused("stage0_original_stop_binding")
		}
		seen[p.PID] = true
		parent = parent || p == context.Supervisor
		child = child || p == context.Child
	}
	if !parent || !child {
		return refused("stage0_original_stop_binding")
	}
	return validateManagerExit(context, exit, i)
}

// Only the host entry's caught ContractError/Failure exit is supported. Neither
// an arbitrary nonzero code nor a user journal establishes a terminal parent.
func validateFailedController(r Registration, registrationSHA string, i Invocation, claim Claim,
	context Context, exit ExitRecord, anchor ManagerStart, contextRaw []byte,
	properties map[string]string, processesGone bool, members []int) error {
	if i.Operation != "transaction" && i.Operation != "continue-installed" {
		return refused("stage0_failed_controller_operation")
	}
	if err := validateClaim(r, registrationSHA, i, claim); err != nil {
		return err
	}
	if err := validateManagerStart(anchor, context, contextRaw, i); err != nil {
		return err
	}
	if claim.Supervisor != context.Supervisor || claim.BootID != context.BootID ||
		exit.Schema != "ags.routeb-stage0-exit.v1" || exit.RegistrationSHA256 != registrationSHA ||
		exit.InvocationID != i.ID || exit.Phase != i.Phase || exit.BootID != context.BootID ||
		exit.Supervisor != context.Supervisor || exit.Child != context.Child || exit.ChildExitCode != 78 ||
		exit.SignalForwarded || exit.ForcedKill || !exit.ContextVerified || !exit.AuthorityRechecked || exit.NormalExit {
		return refused("stage0_failed_controller_binding")
	}
	if !processesGone || properties["MainPID"] != "0" || properties["ControlPID"] != "0" || len(members) != 0 {
		return refused("stage0_failed_controller_not_terminal")
	}
	// PID1 can retain the real failed status or GC a dead unit. The durable
	// context/anchor/exit still prove which failed controller was consumed.
	retained := properties["InvocationID"] == anchor.ManagerInvocationID &&
		properties["ExecMainPID"] == decimal(context.Supervisor.PID) && properties["ExecMainCode"] == "1" &&
		properties["ExecMainStatus"] == "78" && properties["Result"] == "exit-code" &&
		properties["ActiveState"] == "failed" && properties["SubState"] == "failed"
	collected := properties["InvocationID"] == "" && properties["ExecMainPID"] == "0" &&
		properties["ExecMainCode"] == "0" && properties["ExecMainStatus"] == "0" && properties["Result"] == "success" &&
		properties["ActiveState"] == "inactive" && properties["SubState"] == "dead"
	if !retained && !collected {
		return refused("stage0_failed_controller_manager_unknown")
	}
	return nil
}
