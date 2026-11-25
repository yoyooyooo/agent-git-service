package routebstage0

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
)

func continuationFixture() Registration {
	r := fixtureRegistration()
	file := func(path string) interpreterauthority.File {
		return interpreterauthority.File{Path: path, SHA256: strings.Repeat("a", 64), Size: 1}
	}
	p, a := file("/home/example/transaction/package.json"), file("/home/example/transaction/approval.json")
	r.Inputs.Package, r.Inputs.Approval = &p, &a
	r.Invocations = nil
	for _, phase := range []string{"transaction", "continue-installed", "serve-initial", "serve-candidate"} {
		op := phase
		if strings.HasPrefix(phase, "serve-") {
			op = "serve"
		}
		unit := unitName(r.ID, op)
		r.Invocations = append(r.Invocations, Invocation{ID: phase + "-one", Operation: op, Phase: phase,
			AttemptID: phase + "-attempt", AuthorityEpoch: 1, AuthorizationID: phase + "-approval", Unit: unit,
			Definition:         file(filepath.Join("/etc/systemd/system", unit)),
			DefinitionRecovery: file(filepath.Join(rootFor(r.ID), "unit-recovery", unit))})
	}
	return r
}

func TestServeSelectionKeepsConsumedContinuationHistorical(t *testing.T) {
	r := continuationFixture()
	candidate, _ := invocationForPhase(r, "serve-candidate")
	old := candidate
	old.ID, old.Phase = "serve-old-one", "serve-old"
	r.Invocations = append(r.Invocations, old)
	now := time.Date(2026, 11, 25, 5, 0, 0, 0, time.UTC)
	registrationSHA := strings.Repeat("d", 64)
	claim := Claim{"ags.routeb-stage0-claim.v1", registrationSHA, candidate,
		ProcessIdentity{502, 100, 1, 0, 0, r.NativeImage.SHA256}, "11111111-2222-3333-4444-555555555555"}
	// Use the actual exclusive state encoder and claim validator. A claimed slot
	// is not a boolean hint: its registered identity remains required.
	dir, err := os.MkdirTemp("", "routeb-consumed-ticket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("retained", dir)
	path := filepath.Join(dir, "claim.json")
	if err = writeExclusiveFile(path, claim); err != nil {
		t.Fatal(err)
	}
	claimed := func(i Invocation) (bool, error) {
		if i.ID != candidate.ID {
			return false, nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		var stored Claim
		if err = decodeExact(raw, &stored); err != nil {
			return false, err
		}
		return true, validateClaim(r, registrationSHA, i, stored)
	}
	ticket := func(i Invocation, current bool) (bool, error) {
		issued, expires := now.Add(-time.Hour), now.Add(time.Hour)
		if i.ID == candidate.ID {
			expires = now.Add(-time.Minute) // Old authority is no longer a new permission.
		} else if i.ID != old.ID {
			return false, nil
		}
		if !approvalTime(issued.Format(time.RFC3339), expires.Format(time.RFC3339), current, now) {
			return true, refused("stage0_continuation_approval_time")
		}
		return true, nil
	}
	got, err := selectServeInvocations(r.Invocations, claimed, ticket)
	if err != nil || len(got) != 1 || got[0] != old {
		t.Fatalf("expired consumed candidate blocked fresh old-start: %v, %v", got, err)
	}
	_, err = selectServeInvocations(r.Invocations, func(Invocation) (bool, error) { return false, nil }, ticket)
	if err == nil {
		t.Fatal("an unclaimed expired candidate became a new start permission")
	}
	claim.Invocation.AttemptID = "another-attempt"
	raw, _ := json.Marshal(claim)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = selectServeInvocations(r.Invocations, claimed, ticket); err == nil {
		t.Fatal("a corrupt consumed claim was treated as safe to skip")
	}
}

func TestServeSelectionRefusesUnknownClaimAndMissingHistoricalTicket(t *testing.T) {
	r := continuationFixture()
	for _, missing := range []bool{false, true} {
		_, err := selectServeInvocations(r.Invocations, func(Invocation) (bool, error) {
			if !missing {
				return false, refused("stage0_state_unknown")
			}
			return true, nil
		}, func(Invocation, bool) (bool, error) { return false, nil })
		if err == nil {
			t.Fatal("unknown claim or missing ticket refunded a consumed slot")
		}
	}
}

func TestContinuationRegistrationPreservesOriginalAndCandidateSlots(t *testing.T) {
	r := continuationFixture()
	if err := r.Validate(); err != nil {
		t.Fatalf("registered installed continuation refused: %v", err)
	}
	for _, phase := range []string{"transaction", "serve-initial", "serve-candidate"} {
		t.Run("missing-"+phase, func(t *testing.T) {
			v := continuationFixture()
			var keep []Invocation
			for _, i := range v.Invocations {
				if i.Phase != phase {
					keep = append(keep, i)
				}
			}
			v.Invocations = keep
			if v.Validate() == nil {
				t.Fatal("continuation admitted without its original or unique start target")
			}
		})
	}
	for _, phase := range []string{"continue-installed", "serve-candidate"} {
		v := continuationFixture()
		for _, i := range v.Invocations {
			if i.Phase == phase {
				i.ID += "-again"
				v.Invocations = append(v.Invocations, i)
				break
			}
		}
		if v.Validate() == nil {
			t.Fatal("a second slot could refund a consumed continuation or candidate")
		}
	}
}

func TestContinuationUsesTheFixedHostEntry(t *testing.T) {
	r := continuationFixture()
	h := HostProfile{Root: "/home/example/transaction/ags", ToolBundle: rootFor(r.ID) + "/bundle", LogicalHost: "imile-win"}
	got, err := childArguments(r, h, "/publisher/python", "continue-installed")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/publisher/python", "-I", "-B", "-S", h.ToolBundle + "/routeb_host_transaction.py",
		"continue-installed", "--host", h.LogicalHost, "--package", r.Inputs.Package.Path,
		"--approval", r.Inputs.Approval.Path, "--execute"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("continuation escaped the registered host entry: %q", got)
	}
}

func TestContinuationSubjectMatchesPythonCanonicalIdentity(t *testing.T) {
	subject := ContinuationSubject{strings.Repeat("a", 64), "transaction-one", "fixture-host", strings.Repeat("b", 64),
		"cold-one", strings.Repeat("c", 64), strings.Repeat("d", 64), "snapshot-one", strings.Repeat("e", 64), strings.Repeat("f", 64)}
	if subject.digest() != "b2f56fac656b3d5e1034d2270cbea49296279b13bc1fc9268c9fded5f3949e3b" {
		t.Fatal("Go/Python disagree on the frozen failed-window subject")
	}
	approval := ContinuationApproval{ApprovalID: "continue-one", Purpose: "continue-installed", SubjectSHA256: subject.digest()}
	if approval.subject() != "c920b9d371f906ce755020929ecb9bd20563125cbe95484848d603b7da24b51d" {
		t.Fatal("the delegated approval review lost its distinct purpose/approval binding")
	}
}

func TestIsolatedLinuxHostBindsRegisteredNonrootAccount(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("Linux host registration runs on the Linux source-test runner")
	}
	r := fixtureRegistration()
	h := HostProfile{Schema: "ags.routeb-native-identity.v1", Scope: "isolated-native", TransactionID: r.ID,
		LogicalHost: "disposable-runner", HostInstance: r.Binding.HostInstance, UID: 1001, GID: 1234,
		User: "runner", Home: "/home/runner", Root: "/home/runner/.local/state/ags-routeb-native/" + r.ID + "/ags",
		ToolBundle: rootFor(r.ID) + "/bundle", Platform: "linux", Architecture: "x86_64",
		MachineIDKind: "machine-id", MachineIDSHA256: strings.Repeat("d", 64), Aliases: []string{"runner-vm"}}
	if err := r.validateHost(h); err != nil {
		t.Fatal("isolated registration still requires a production account:", err)
	}
	for name, mutate := range map[string]func(*HostProfile){
		"production-scope": func(h *HostProfile) { h.Scope = "production" },
		"root-user":        func(h *HostProfile) { h.UID = 0 },
		"root-group":       func(h *HostProfile) { h.GID = 0 },
		"other-home":       func(h *HostProfile) { h.Home = "/home/another" },
		"production-root":  func(h *HostProfile) { h.Root = "/home/runner/.ags" },
		"unknown-machine":  func(h *HostProfile) { h.MachineIDSHA256 = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := h
			mutate(&changed)
			if r.validateHost(changed) == nil {
				t.Fatal("isolated account adaptation widened the registered identity")
			}
		})
	}
}
