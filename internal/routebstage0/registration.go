// Package routebstage0 implements the fixed root service-manager entry for
// isolated Route B operations. A catalog receipt is never an admission token.
package routebstage0

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
)

const (
	Schema          = "ags.routeb-stage0.v1"
	Registry        = "/usr/local/libexec/ags-routeb-native"
	SocketDirectory = "/run/ags-routeb-stage0"
	Protocol        = "ags.routeb-stage0-peer.v1"
)

type Reference struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

type Inputs struct {
	Package           *interpreterauthority.File `json:"package,omitempty"`
	Approval          *interpreterauthority.File `json:"approval,omitempty"`
	BootstrapApproval *interpreterauthority.File `json:"bootstrap_approval,omitempty"`
}

// Invocations are independent one-use slots. A serve slot additionally needs
// a root-private issued ticket, bound to this exact registered phase/attempt.
// Restart=no does not by itself prevent replay of an operator start request.
type Invocation struct {
	ID                 string                    `json:"id"`
	Operation          string                    `json:"operation"`
	Phase              string                    `json:"phase"`
	AttemptID          string                    `json:"attempt_id"`
	AuthorityEpoch     uint64                    `json:"authority_epoch"`
	AuthorizationID    string                    `json:"authorization_id"`
	Unit               string                    `json:"unit"`
	Definition         interpreterauthority.File `json:"definition"`
	DefinitionRecovery interpreterauthority.File `json:"definition_recovery"`
}

type Registration struct {
	Schema              string                       `json:"schema"`
	ID                  string                       `json:"id"`
	Scope               string                       `json:"scope"`
	Binding             interpreterauthority.Binding `json:"binding"`
	NativeImage         interpreterauthority.File    `json:"native_image"`
	NativeRecoveryImage interpreterauthority.File    `json:"native_recovery_image"`
	HostProfile         interpreterauthority.File    `json:"host_profile"`
	Interpreter         Reference                    `json:"interpreter"`
	Inputs              Inputs                       `json:"inputs"`
	Invocations         []Invocation                 `json:"invocations"`
	ScopeGrant          *interpreterauthority.File   `json:"scope_grant,omitempty"`
}

type HostProfile struct {
	Schema          string   `json:"schema"`
	LogicalHost     string   `json:"logical_host"`
	HostInstance    string   `json:"host_instance"`
	UID             uint32   `json:"uid"`
	GID             uint32   `json:"gid"`
	User            string   `json:"user"`
	Home            string   `json:"home"`
	Root            string   `json:"root"`
	Platform        string   `json:"platform"`
	Architecture    string   `json:"arch"`
	MachineIDKind   string   `json:"machine_id_kind"`
	MachineIDSHA256 string   `json:"machine_id_sha256"`
	Aliases         []string `json:"aliases"`
	Scope           string   `json:"scope"`
	TransactionID   string   `json:"transaction_id"`
	ToolBundle      string   `json:"tool_bundle"`
}

var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func refused(code string) error { return errors.New(code) }
func hash(raw []byte) string    { s := sha256.Sum256(raw); return hex.EncodeToString(s[:]) }
func validDigest(s string) bool { return digest.MatchString(s) && s != strings.Repeat("0", 64) }
func absolute(s string) bool {
	return filepath.IsAbs(s) && filepath.Clean(s) == s && !strings.ContainsAny(s, "\\\x00\r\n\t")
}
func within(s, base string) bool { return s == base || strings.HasPrefix(s, base+"/") }
func rootFor(id string) string   { return filepath.Join(Registry, id) }
func registrationPath(id string) string {
	return filepath.Join(rootFor(id), "stage0-registration.json")
}
func socketPath(pid int) string { return filepath.Join(SocketDirectory, strconv.Itoa(pid)+".sock") }
func decimal(n int) string      { return strconv.Itoa(n) }

func validFile(f interpreterauthority.File, limit int64) bool {
	return absolute(f.Path) && validDigest(f.SHA256) && f.Size > 0 && f.Size <= limit
}

func operationPhase(operation, phase string) bool {
	switch operation {
	case "probe", "bootstrap-plan", "bootstrap", "plan", "transaction", "doctor", "restore", "old-start":
		return phase == operation
	case "serve":
		return phase == "serve-initial" || phase == "serve-candidate" || phase == "serve-old"
	default:
		return false
	}
}

func unitName(id, operation string) string {
	if operation == "serve" {
		return "ags-hostpkg-test-" + id + ".service"
	}
	return "ags-routeb-control-" + id + "-" + operation + ".service"
}

func (r Registration) Validate() error {
	if r.ScopeGrant != nil && (r.ScopeGrant.Path != filepath.Join(rootFor(r.ID), "scope-grant.json") || !validFile(*r.ScopeGrant, 1<<20)) {
		return refused("stage0_scope_grant_invalid")
	}
	if r.Schema != Schema || !interpreterauthority.ValidID(r.ID) || len(r.ID) > 48 || r.Scope != "isolated-native" || r.Binding.Validate() != nil {
		return refused("stage0_registration_invalid")
	}
	if r.NativeImage.Path != filepath.Join(rootFor(r.ID), "stage0/ags-routeb-stage0") || !validFile(r.NativeImage, 64<<20) ||
		r.NativeRecoveryImage.Path != filepath.Join(rootFor(r.ID), "stage0/recovery/ags-routeb-stage0") || !validFile(r.NativeRecoveryImage, 64<<20) ||
		r.NativeRecoveryImage.SHA256 != r.NativeImage.SHA256 || r.NativeRecoveryImage.Size != r.NativeImage.Size ||
		r.HostProfile.Path != filepath.Join(rootFor(r.ID), "host-profile.json") || !validFile(r.HostProfile, 65536) ||
		r.HostProfile.SHA256 != r.Binding.HostProfileSHA256 || !interpreterauthority.ValidID(r.Interpreter.ID) || !validDigest(r.Interpreter.SHA256) {
		return refused("stage0_registration_binding")
	}
	if len(r.Invocations) == 0 || len(r.Invocations) > 16 {
		return refused("stage0_invocation_count")
	}
	ids, phases := map[string]bool{}, map[string]bool{}
	definitions := map[string]interpreterauthority.File{}
	for _, i := range r.Invocations {
		if !interpreterauthority.ValidID(i.ID) || !interpreterauthority.ValidID(i.AttemptID) || !interpreterauthority.ValidID(i.AuthorizationID) || i.AuthorityEpoch != 1 ||
			!operationPhase(i.Operation, i.Phase) || ids[i.ID] || phases[i.Phase] || i.Unit != unitName(r.ID, i.Operation) ||
			i.Definition.Path != filepath.Join("/etc/systemd/system", i.Unit) || !validFile(i.Definition, 65536) ||
			i.DefinitionRecovery.Path != filepath.Join(rootFor(r.ID), "unit-recovery", i.Unit) || !validFile(i.DefinitionRecovery, 65536) ||
			i.DefinitionRecovery.SHA256 != i.Definition.SHA256 || i.DefinitionRecovery.Size != i.Definition.Size {
			return refused("stage0_invocation_invalid")
		}
		ids[i.ID], phases[i.Phase] = true, true
		for _, file := range []interpreterauthority.File{i.Definition, i.DefinitionRecovery} {
			if previous, exists := definitions[file.Path]; exists && previous != file {
				return refused("stage0_definition_binding_conflict")
			}
			definitions[file.Path] = file
		}
		if i.Operation == "bootstrap" || i.Operation == "bootstrap-plan" {
			if r.Inputs.BootstrapApproval == nil {
				return refused("stage0_input_missing")
			}
		}
		if i.Operation == "transaction" || i.Operation == "plan" {
			if r.Inputs.Package == nil || r.Inputs.Approval == nil {
				return refused("stage0_input_missing")
			}
		}
		if i.Operation == "restore" || i.Operation == "old-start" {
			if r.Inputs.Package == nil || r.Inputs.Approval == nil {
				return refused("stage0_input_missing")
			}
		}
	}
	for _, f := range []*interpreterauthority.File{r.Inputs.Package, r.Inputs.Approval, r.Inputs.BootstrapApproval} {
		if f != nil && !validFile(*f, 16<<20) {
			return refused("stage0_input_invalid")
		}
	}
	return nil
}

func (r Registration) validateHost(h HostProfile) error {
	expectedArch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[r.Binding.Architecture]
	expectedPlatform := map[string]string{"linux": "linux", "darwin": "darwin"}[r.Binding.Platform]
	if h.Schema != "ags.routeb-native-identity.v1" || h.Scope != r.Scope || h.TransactionID != r.ID || h.HostInstance != r.Binding.HostInstance ||
		h.User != "yoyo" || h.UID == 0 || h.GID == 0 || !absolute(h.Home) ||
		h.Root != filepath.Join(h.Home, ".local/state/ags-routeb-native", r.ID, "ags") || h.ToolBundle != filepath.Join(rootFor(r.ID), "bundle") ||
		strings.ToLower(h.Platform) != expectedPlatform || h.Architecture != expectedArch || !validDigest(h.MachineIDSHA256) || len(h.Aliases) == 0 || len(h.Aliases) > 8 {
		return refused("stage0_host_binding")
	}
	if runtime.GOOS != r.Binding.Platform || runtime.GOARCH != r.Binding.Architecture {
		return refused("stage0_platform_mismatch")
	}
	if (r.Binding.Platform == "linux" && (h.LogicalHost != "imile-win" || h.UID != 1000 || h.GID != 1000 || h.MachineIDKind != "machine-id")) ||
		(r.Binding.Platform == "darwin" && (h.LogicalHost != "mini" || h.UID != 501 || h.GID != 20 || h.MachineIDKind != "ioplatformuuid")) {
		return refused("stage0_host_binding")
	}
	for _, a := range h.Aliases {
		if !interpreterauthority.ValidID(a) {
			return refused("stage0_host_alias_invalid")
		}
	}
	for _, f := range []*interpreterauthority.File{r.Inputs.Package, r.Inputs.Approval, r.Inputs.BootstrapApproval} {
		if f != nil && !within(f.Path, filepath.Dir(h.Root)) {
			return refused("stage0_input_outside_transaction")
		}
	}
	return nil
}

// childArguments is closed over registered files. Caller arguments never
// become a script, interpreter option, environment entry or manager target.
func childArguments(r Registration, h HostProfile, executable, operation string) ([]string, error) {
	args := []string{executable, "-I", "-B", "-S"}
	bundle := h.ToolBundle
	switch operation {
	case "probe":
		args = append(args, filepath.Join(bundle, "routeb_stage0_probe.py"), "--root", h.Root)
	case "bootstrap-plan", "bootstrap":
		if r.Inputs.BootstrapApproval == nil {
			return nil, refused("stage0_input_missing")
		}
		action := "plan"
		if operation == "bootstrap" {
			action = "activate"
		}
		args = append(args, filepath.Join(bundle, "routeb_native_bootstrap.py"), action, "--root", h.Root, "--approval", r.Inputs.BootstrapApproval.Path)
	case "plan", "transaction":
		if r.Inputs.Package == nil || r.Inputs.Approval == nil {
			return nil, refused("stage0_input_missing")
		}
		action := "plan"
		if operation == "transaction" {
			action = "run"
		}
		args = append(args, filepath.Join(bundle, "routeb_host_transaction.py"), action, "--host", h.LogicalHost, "--package", r.Inputs.Package.Path, "--approval", r.Inputs.Approval.Path)
		if operation == "transaction" {
			args = append(args, "--execute")
		}
	case "serve", "doctor":
		args = append(args, filepath.Join(bundle, "routeb_launcher.py"), "--root", h.Root, operation)
	case "restore", "old-start":
		if r.Inputs.Package == nil || r.Inputs.Approval == nil {
			return nil, refused("stage0_input_missing")
		}
		args = append(args, filepath.Join(bundle, "routeb_native_recovery.py"), operation, "--root", h.Root, "--package", r.Inputs.Package.Path, "--approval", r.Inputs.Approval.Path)
	default:
		return nil, refused("stage0_operation_invalid")
	}
	return args, nil
}
