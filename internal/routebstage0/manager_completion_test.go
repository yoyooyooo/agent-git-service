package routebstage0

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type managerEvidenceFixture struct {
	loaded                         loaded
	invocation                     Invocation
	context                        Context
	exit                           ExitRecord
	anchor                         ManagerStart
	completion                     ManagerCompletion
	contextRaw, exitRaw, anchorRaw []byte
}

func managerFixture(t *testing.T, operation string) managerEvidenceFixture {
	t.Helper()
	r := fixtureRegistration()
	i := r.Invocations[0]
	i.Operation, i.Phase, i.ID = operation, operation, operation+"-one"
	if operation == "serve" {
		i.Phase, i.ID = "serve-initial", "serve-initial-one"
	}
	i.Unit = unitName(r.ID, operation)
	r.Invocations = []Invocation{i}
	l := loaded{registration: r, registrationSHA: strings.Repeat("d", 64),
		host: HostProfile{Root: "/home/example/.local/state/ags-routeb-native/fixture-native/ags", UID: 1000, GID: 1000}}
	if operation == "restore" {
		l.recoverySHA = strings.Repeat("9", 64)
	}
	c := Context{Schema: "ags.routeb-stage0-context.v1", RegistrationID: r.ID, RegistrationSHA256: l.registrationSHA,
		Root: l.host.Root, Operation: operation, Phase: i.Phase, InvocationID: i.ID,
		AttemptID: i.AttemptID, AuthorityEpoch: i.AuthorityEpoch, AuthorizationID: i.AuthorizationID,
		BootID:                     "11111111-2222-3333-4444-555555555555",
		Supervisor:                 ProcessIdentity{502, 100, 1, 0, 0, r.NativeImage.SHA256},
		Child:                      ProcessIdentity{503, 200, 502, 1000, 1000, strings.Repeat("b", 64)},
		InterpreterAuthoritySHA256: r.Interpreter.SHA256, ToolRevision: r.Binding.ToolRevision,
		ToolTree: r.Binding.ToolTree, ToolManifestSHA256: r.Binding.ToolManifestSHA256,
		RecoveryRegistrationSHA256: l.recoverySHA}
	exit := ExitRecord{"ags.routeb-stage0-exit.v1", l.registrationSHA, i.ID, i.Phase, c.BootID,
		c.Supervisor, c.Child, 0, operation == "serve", false, true, true, true}
	encode := func(value any) []byte {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return append(raw, '\n')
	}
	contextRaw, exitRaw := encode(c), encode(exit)
	managerID := strings.Repeat("e", 32)
	anchor := ManagerStart{"ags.routeb-stage0-manager-start.v1", l.registrationSHA, i.ID, i.Phase, c.BootID, i.Unit, managerID, hash(contextRaw)}
	anchorRaw := encode(anchor)
	hook := ProcessIdentity{504, 300, 1, 0, 0, r.NativeImage.SHA256}
	observation := ManagerStopObservation{managerID, 0, c.Supervisor.PID, hook.PID, 1, 0, "success", "success", "exited", "0"}
	completion := ManagerCompletion{"ags.routeb-stage0-manager-completion.v1", l.registrationSHA, i.ID, i.Phase, c.BootID,
		i.Unit, managerID, hash(anchorRaw), hash(contextRaw), hash(exitRaw), hook, observation}
	return managerEvidenceFixture{l, i, c, exit, anchor, completion, contextRaw, exitRaw, anchorRaw}
}

func (f managerEvidenceFixture) stopped(properties map[string]string, gone bool, members []int) error {
	if err := f.loaded.verifyManagerContext(f.invocation, f.context, f.context.BootID); err != nil {
		return err
	}
	return validateStoppedManager(f.completion, f.anchor, f.context, f.exit, f.anchorRaw, f.contextRaw, f.exitRaw,
		f.invocation, properties, gone, members)
}

func collectedManager() map[string]string {
	return map[string]string{"MainPID": "0", "ActiveState": "inactive", "SubState": "dead", "Result": "success",
		"ExecMainCode": "0", "ExecMainStatus": "0", "ExecMainPID": "0", "InvocationID": ""}
}

func TestManagerCompletionSurvivesGCOnlyWithStoredProof(t *testing.T) {
	for _, operation := range []string{"serve", "probe", "restore"} {
		t.Run(operation, func(t *testing.T) {
			f := managerFixture(t, operation)
			// Exercise the same exclusive writer and JSON decoder used by the
			// root path, in a retained ordinary-user filesystem fixture.
			dir, err := os.MkdirTemp("", "routeb-manager-completion-")
			if err != nil {
				t.Fatal(err)
			}
			t.Log("retained", dir)
			path := filepath.Join(dir, "completion.json")
			if err = writeExclusiveFile(path, f.completion); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil || decodeExact(raw, &f.completion) != nil {
				t.Fatal("stored completion unavailable", err)
			}
			if err = f.stopped(collectedManager(), true, nil); err != nil {
				t.Fatal("discarded manager properties lost durable proof", err)
			}
			f.completion = ManagerCompletion{}
			if f.stopped(collectedManager(), true, nil) == nil {
				t.Fatal("inactive GC zeros admitted a missing completion")
			}
		})
	}
}

func TestManagerCompletionRejectsChangedBindingsAndStopResults(t *testing.T) {
	cases := map[string]func(*managerEvidenceFixture){
		"manager-id":      func(f *managerEvidenceFixture) { f.completion.Manager.InvocationID = strings.Repeat("f", 32) },
		"invocation":      func(f *managerEvidenceFixture) { f.completion.InvocationID = "another-slot" },
		"anchor-hash":     func(f *managerEvidenceFixture) { f.completion.StartAnchorSHA256 = strings.Repeat("1", 64) },
		"context-hash":    func(f *managerEvidenceFixture) { f.completion.ContextSHA256 = strings.Repeat("1", 64) },
		"exit-hash":       func(f *managerEvidenceFixture) { f.completion.ExitSHA256 = strings.Repeat("1", 64) },
		"context-bytes":   func(f *managerEvidenceFixture) { f.contextRaw = append(f.contextRaw, ' ') },
		"exit-bytes":      func(f *managerEvidenceFixture) { f.exitRaw = append(f.exitRaw, ' ') },
		"wrong-root":      func(f *managerEvidenceFixture) { f.context.Root += "-other" },
		"wrong-boot":      func(f *managerEvidenceFixture) { f.completion.BootID = "another-boot" },
		"context-birth":   func(f *managerEvidenceFixture) { f.context.Supervisor.StartTicks++ },
		"child-nonzero":   func(f *managerEvidenceFixture) { f.exit.ChildExitCode = 78 },
		"not-forwarded":   func(f *managerEvidenceFixture) { f.exit.SignalForwarded = false },
		"forced":          func(f *managerEvidenceFixture) { f.exit.ForcedKill = true },
		"unverified":      func(f *managerEvidenceFixture) { f.exit.ContextVerified = false },
		"authority":       func(f *managerEvidenceFixture) { f.exit.AuthorityRechecked = false },
		"not-normal":      func(f *managerEvidenceFixture) { f.exit.NormalExit = false },
		"control-pid":     func(f *managerEvidenceFixture) { f.completion.Manager.ControlPID++ },
		"main-alive":      func(f *managerEvidenceFixture) { f.completion.Manager.MainPID = f.context.Supervisor.PID },
		"exec-main-pid":   func(f *managerEvidenceFixture) { f.completion.Manager.ExecMainPID++ },
		"manager-signal":  func(f *managerEvidenceFixture) { f.completion.Manager.ExecMainCode = 2 },
		"manager-nonzero": func(f *managerEvidenceFixture) { f.completion.Manager.ExecMainStatus = 78 },
		"manager-failed":  func(f *managerEvidenceFixture) { f.completion.Manager.Result = "signal" },
		"env-killed":      func(f *managerEvidenceFixture) { f.completion.Manager.ExitCode = "killed" },
		"env-term":        func(f *managerEvidenceFixture) { f.completion.Manager.ExitStatus = "TERM" },
		"env-failed":      func(f *managerEvidenceFixture) { f.completion.Manager.ServiceResult = "exit-code" },
		"hook-uid":        func(f *managerEvidenceFixture) { f.completion.Hook.UID = 1000 },
		"hook-gid":        func(f *managerEvidenceFixture) { f.completion.Hook.GID = 1000 },
		"hook-parent":     func(f *managerEvidenceFixture) { f.completion.Hook.ParentPID = 502 },
		"hook-image":      func(f *managerEvidenceFixture) { f.completion.Hook.ExecutableSHA256 = strings.Repeat("1", 64) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := managerFixture(t, "serve")
			mutate(&f)
			if f.stopped(collectedManager(), true, nil) == nil {
				t.Fatal("accepted changed completion")
			}
		})
	}
	f := managerFixture(t, "probe")
	f.exit.SignalForwarded = true
	if f.stopped(collectedManager(), true, nil) == nil {
		t.Fatal("signalled control process was accepted")
	}
}

func TestManagerCompletionStillRequiresCurrentQuiescence(t *testing.T) {
	f := managerFixture(t, "serve")
	if f.stopped(collectedManager(), false, nil) == nil || f.stopped(collectedManager(), true, []int{900}) == nil {
		t.Fatal("durable completion hid a living process or cgroup member")
	}
	for key, value := range map[string]string{"MainPID": "900", "ActiveState": "active", "SubState": "running", "Result": "exit-code"} {
		properties := collectedManager()
		properties[key] = value
		if f.stopped(properties, true, nil) == nil {
			t.Fatal("current manager disagreement accepted", key)
		}
	}
}

func TestSharedServeUnitSelectsOnlyItsActualManagerInvocation(t *testing.T) {
	f := managerFixture(t, "serve")
	r := f.loaded.registration
	r.Invocations = nil
	anchors := map[string]ManagerStart{}
	for index, phase := range []string{"serve-initial", "serve-candidate", "serve-old"} {
		i, anchor := f.invocation, f.anchor
		i.ID, i.Phase = phase+"-one", phase
		anchor.InvocationID, anchor.Phase = i.ID, phase
		anchor.ManagerInvocationID = strings.Repeat(string(rune('a'+index)), 32)
		r.Invocations = append(r.Invocations, i)
		anchors[i.ID] = anchor
	}
	for _, i := range r.Invocations {
		selected, err := managerStartInvocation(r, "serve", anchors[i.ID].ManagerInvocationID, anchors)
		if err != nil || selected != i {
			t.Fatal("did not select exact manager incarnation", i.Phase, err)
		}
	}
	for _, managerID := range []string{"", strings.Repeat("0", 32), strings.Repeat("f", 32)} {
		if _, err := managerStartInvocation(r, "serve", managerID, anchors); err == nil {
			t.Fatal("missing/unknown manager incarnation accepted")
		}
	}
	duplicate := anchors[r.Invocations[1].ID]
	duplicate.ManagerInvocationID = anchors[r.Invocations[0].ID].ManagerInvocationID
	anchors[r.Invocations[1].ID] = duplicate
	if _, err := managerStartInvocation(r, "serve", duplicate.ManagerInvocationID, anchors); err == nil {
		t.Fatal("duplicate manager incarnation accepted")
	}
}

func TestCompletionFileIsExclusiveAndRejectsSymlinks(t *testing.T) {
	dir, err := os.MkdirTemp("", "routeb-completion-exclusive-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("retained", dir)
	path := filepath.Join(dir, "completion.json")
	f := managerFixture(t, "serve")
	if err = writeExclusiveFile(path, f.completion); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeExclusiveFile(path, ManagerCompletion{}); err == nil {
		t.Fatal("completion overwrite accepted")
	}
	link := filepath.Join(dir, "alias.json")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err = writeExclusiveFile(link, ManagerCompletion{}); err == nil {
		t.Fatal("completion symlink accepted")
	}
	after, err := os.ReadFile(path)
	info, statErr := os.Stat(path)
	if err != nil || statErr != nil || string(before) != string(after) || info.Mode().Perm() != 0600 {
		t.Fatal("existing completion changed", err, statErr)
	}
}

func TestManagerCompletionJSONIsClosed(t *testing.T) {
	f := managerFixture(t, "serve")
	raw, err := json.Marshal(f.completion)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"{}", strings.Replace(string(raw), `"schema":`, `"Schema":`, 1),
		strings.Replace(string(raw), `"schema":`, `"schema":"duplicate","schema":`, 1),
		strings.Replace(string(raw), `"manager":`, `"unexpected":0,"manager":`, 1)} {
		var completion ManagerCompletion
		if decodeExact([]byte(value), &completion) == nil {
			t.Fatal("missing, alias, duplicate or extra completion field accepted")
		}
	}
}

func TestCompletionObservationDoesNotRenewExpiredExecutionApproval(t *testing.T) {
	value := RecoveryApproval{IssuedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2026-01-01T00:01:00Z"}
	now := time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)
	if recoveryApprovalTime(value, true, now) || !recoveryApprovalTime(value, false, now) {
		t.Fatal("execution and post-exit observation used the same expiry admission")
	}
	value.ExpiresAt = value.IssuedAt
	if recoveryApprovalTime(value, false, now) {
		t.Fatal("completion accepted malformed historical approval interval")
	}
}

func TestEffectiveCompletionCommandIsExactlyOneFixedArgv(t *testing.T) {
	f := managerFixture(t, "serve")
	command := managerStopCommand(f.loaded.registration, f.invocation)
	render := "{ path=" + f.loaded.registration.NativeImage.Path + " ; argv[]=" + command + " ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }"
	if !validEffectiveStopPost(render, command) {
		t.Fatal("fixed effective stop command refused")
	}
	for _, value := range []string{"", render + " " + render,
		strings.Replace(render, "ignore_errors=no", "ignore_errors=yes", 1),
		strings.Replace(render, "manager-complete", "serve", 1),
		strings.Replace(render, "--operation serve", "--operation serve --unsafe", 1),
		strings.Replace(render, " ; pid=0", " ; caller=unknown ; pid=0", 1)} {
		if validEffectiveStopPost(value, command) {
			t.Fatal("effective manager extension accepted", value)
		}
	}
}
