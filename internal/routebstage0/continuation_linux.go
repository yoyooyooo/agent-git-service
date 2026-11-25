//go:build linux

package routebstage0

import "github.com/ngaut/agent-git-service/internal/interpreterauthority"

func (l *loaded) failedController(i Invocation, boot string) error {
	context, exit, anchor, contextRaw, _, _, err := l.readManagerIdentity(i, boot)
	if err != nil {
		return err
	}
	raw, err := interpreterauthority.ReadProtected(l.statePath("claims", i.ID), 65536)
	var claim Claim
	if err != nil || decodeExact(raw, &claim) != nil {
		return refused("stage0_failed_controller_claim_unknown")
	}
	properties, err := managerProperties(i.Unit, "MainPID", "ControlPID", "ActiveState", "SubState", "Result",
		"InvocationID", "ExecMainPID", "ExecMainCode", "ExecMainStatus")
	if err != nil {
		return err
	}
	members, err := cgroupMembers(i.Unit)
	if err != nil {
		return err
	}
	return validateFailedController(l.registration, l.registrationSHA, i, claim, context, exit, anchor, contextRaw,
		properties, processGone(context.Supervisor) && processGone(context.Child), members)
}

// An intent is already a possibly attempted effect, even without a ticket or a
// running process. Only the candidate domain is required to be absent; the
// original service and controllers must retain their real manager anchors.
func (l *loaded) candidateNotAttempted() error {
	candidate, err := invocationForPhase(l.registration, "serve-candidate")
	if err != nil {
		return err
	}
	paths := []string{}
	for _, kind := range []string{"tickets", "claims", "active", "exits", "manager-start", "manager-completion", "failures", "failure-exits"} {
		paths = append(paths, l.statePath(kind, candidate.ID))
	}
	for _, i := range l.registration.Invocations {
		if i.Operation == "transaction" || i.Operation == "continue-installed" {
			paths = append(paths, l.statePath("effects", i.ID+"-manager-start-intent"))
		}
	}
	for _, path := range paths {
		missing, err := absentFile(path)
		if err != nil || !missing {
			return refused("stage0_candidate_start_attempted_or_unknown")
		}
	}
	return nil
}

func (l *loaded) originalStop(boot string, current bool) (StoppedIdentity, error) {
	var value StoppedIdentity
	transaction, err := invocationForPhase(l.registration, "transaction")
	if err != nil {
		return value, err
	}
	i, err := invocationForPhase(l.registration, "serve-initial")
	if err != nil {
		return value, err
	}
	read := func(name string, target any) error {
		raw, err := interpreterauthority.ReadProtected(l.statePath("effects", transaction.ID+"-manager-stop-"+name), 65536)
		if err != nil {
			return err
		}
		return decodeExact(raw, target)
	}
	if err = read("intent", &value.Runtime); err != nil {
		return value, err
	}
	if err = read("result", &value.Exit); err != nil {
		return value, err
	}
	var context Context
	var exit ExitRecord
	if current {
		context, exit, value.ManagerCompletion, err = l.completedManager(i, boot)
	} else {
		context, exit, value.ManagerCompletion, err = l.managerCompletionEvidence(i, boot)
	}
	if err != nil {
		return value, err
	}
	if err = validateOriginalStop(value.Runtime, value.Exit, context, exit, i); err != nil {
		return value, err
	}
	if current {
		for _, p := range value.Runtime.PIDs {
			if !processGone(p) {
				return value, refused("stage0_descendant_still_present")
			}
		}
	}
	value.Schema = "ags.routeb-stage0-stopped.v1"
	value.TransactionInvocationID = transaction.ID
	return value, nil
}

func (l *loaded) prestartStopped(boot, ownContinuation string) (StoppedIdentity, error) {
	var value StoppedIdentity
	if err := l.candidateNotAttempted(); err != nil {
		return value, err
	}
	transaction, err := invocationForPhase(l.registration, "transaction")
	if err != nil {
		return value, err
	}
	if err = l.failedController(transaction, boot); err != nil {
		return value, err
	}
	failed := []string{transaction.ID}
	for _, i := range l.registration.Invocations {
		if i.Operation != "continue-installed" {
			continue
		}
		claimed, err := l.claimed(i)
		if err != nil {
			return value, err
		}
		if !claimed {
			continue
		}
		if i.ID == ownContinuation && l.invocation == i {
			continue // The caller's live root peer is independently rechecked.
		}
		if err = l.failedController(i, boot); err != nil {
			return value, err
		}
		failed = append(failed, i.ID)
	}
	value, err = l.originalStop(boot, true)
	value.FailedControllerInvocations = failed
	return value, err
}

func (s *supervisor) stoppedIdentity() (StoppedIdentity, error) {
	l := s.loaded
	own := ""
	switch l.invocation.Operation {
	case "continue-installed":
		if err := l.loadContinuation(true); err != nil {
			return StoppedIdentity{}, err
		}
		own = l.invocation.ID
	case "restore":
		if err := l.loadRecovery("restore"); err != nil {
			return StoppedIdentity{}, err
		}
	default:
		return StoppedIdentity{}, refused("stage0_stopped_consumer_invalid")
	}
	value, err := l.prestartStopped(s.platform.boot, own)
	if err == nil {
		s.stopped = &value.Runtime
	}
	return value, err
}
