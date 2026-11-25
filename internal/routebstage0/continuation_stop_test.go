package routebstage0

import (
	"testing"
)

func TestInstalledContinuationRequiresBoundTerminalFailedController(t *testing.T) {
	f := managerFixture(t, "transaction")
	f.exit.ChildExitCode, f.exit.NormalExit = 78, false
	claim := Claim{"ags.routeb-stage0-claim.v1", f.loaded.registrationSHA, f.invocation, f.context.Supervisor, f.context.BootID}
	properties := map[string]string{"MainPID": "0", "ControlPID": "0", "ActiveState": "failed", "SubState": "failed",
		"Result": "exit-code", "InvocationID": f.anchor.ManagerInvocationID, "ExecMainPID": decimal(f.context.Supervisor.PID),
		"ExecMainCode": "1", "ExecMainStatus": "78"}
	verify := func() error {
		return validateFailedController(f.loaded.registration, f.loaded.registrationSHA, f.invocation, claim,
			f.context, f.exit, f.anchor, f.contextRaw, properties, true, nil)
	}
	if err := verify(); err != nil {
		t.Fatal("known host failure could not be read independently", err)
	}
	properties = collectedManager()
	properties["ControlPID"] = "0"
	if err := verify(); err != nil {
		t.Fatal("immutable failure plus terminal collected unit refused", err)
	}
	for _, code := range []int{-1, 0, 1, 137} {
		f.exit.ChildExitCode = code
		if verify() == nil {
			t.Fatalf("unknown or normal child exit %d became an installed continuation", code)
		}
	}
	f.exit.ChildExitCode = 78
	claim.Supervisor.StartTicks++
	if verify() == nil {
		t.Fatal("another root generation supplied the failed controller claim")
	}
	claim.Supervisor = f.context.Supervisor
	properties["MainPID"] = decimal(f.context.Supervisor.PID)
	if verify() == nil {
		t.Fatal("running controller became terminal")
	}
	properties["MainPID"] = "0"
	if validateFailedController(f.loaded.registration, f.loaded.registrationSHA, f.invocation, claim,
		f.context, f.exit, f.anchor, f.contextRaw, properties, false, nil) == nil {
		t.Fatal("unobserved parent exit became terminal")
	}
	if validateFailedController(f.loaded.registration, f.loaded.registrationSHA, f.invocation, claim,
		f.context, f.exit, f.anchor, f.contextRaw, properties, true, []int{991}) == nil {
		t.Fatal("a remaining cgroup writer became terminal")
	}
	f.exit.NormalExit = true
	if verify() == nil {
		t.Fatal("failed exit was relabelled a normal completion")
	}
}

func TestOriginalStopCannotBeReplacedByUserOrAnotherGeneration(t *testing.T) {
	f := managerFixture(t, "serve")
	before := RuntimeIdentity{f.context, []ProcessIdentity{f.context.Supervisor, f.context.Child}, f.invocation.Unit}
	if err := validateOriginalStop(before, f.exit, f.context, f.exit, f.invocation); err != nil {
		t.Fatal(err)
	}
	changed := before
	changed.Context.Child.StartTicks++
	if validateOriginalStop(changed, f.exit, f.context, f.exit, f.invocation) == nil {
		t.Fatal("different stopped generation was accepted")
	}
	result := f.exit
	result.ChildExitCode = 78
	if validateOriginalStop(before, result, f.context, f.exit, f.invocation) == nil {
		t.Fatal("failed exit substituted for a normal stop")
	}
	before.PIDs = []ProcessIdentity{f.context.Supervisor, f.context.Supervisor}
	if validateOriginalStop(before, f.exit, f.context, f.exit, f.invocation) == nil {
		t.Fatal("missing child was hidden by duplicated root PID")
	}
}
