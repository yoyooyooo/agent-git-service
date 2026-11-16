package routebstage0

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/ngaut/agent-git-service/internal/buildinfo"
	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
	"golang.org/x/sys/unix"
)

type loaded struct {
	registration    Registration
	registrationSHA string
	host            HostProfile
	catalog         *interpreterauthority.RegisteredCatalog
	invocation      Invocation
	recovery        *RecoveryRegistration
	recoverySHA     string
}

type bundleManifest struct {
	Schema string            `json:"schema"`
	Entry  string            `json:"entry"`
	Files  map[string]string `json:"files"`
}

func load(id string) (*loaded, error) {
	if !interpreterauthority.ValidID(id) || len(id) > 48 {
		return nil, refused("stage0_registration_invalid")
	}
	raw, err := interpreterauthority.ReadProtected(registrationPath(id), 1<<20)
	if err != nil {
		return nil, err
	}
	var r Registration
	if err = interpreterauthority.StrictJSON(raw, &r); err != nil {
		return nil, err
	}
	if err = r.Validate(); err != nil {
		return nil, err
	}
	if r.ID != id {
		return nil, refused("stage0_registration_binding")
	}
	if buildinfo.Revision != r.Binding.ToolRevision || buildinfo.Tree != r.Binding.ToolTree {
		return nil, refused("stage0_build_identity_mismatch")
	}
	hostRaw, err := interpreterauthority.ReadProtectedFile(r.HostProfile, 65536)
	if err != nil {
		return nil, err
	}
	var h HostProfile
	if err = interpreterauthority.StrictJSON(hostRaw, &h); err != nil {
		return nil, err
	}
	if err = r.validateHost(h); err != nil {
		return nil, err
	}
	l := &loaded{registration: r, registrationSHA: hash(raw), host: h}
	if err = l.verifyDefinitions(); err != nil {
		return nil, err
	}
	if r.ScopeGrant != nil {
		if _, err = interpreterauthority.ReadProtectedFile(*r.ScopeGrant, 1<<20); err != nil {
			return nil, err
		}
	}
	if err = l.verifyBundle(); err != nil {
		return nil, err
	}
	if _, err = interpreterauthority.ReadProtectedFile(r.NativeImage, 64<<20); err != nil {
		return nil, err
	}
	if _, err = interpreterauthority.ReadProtectedFile(r.NativeRecoveryImage, 64<<20); err != nil {
		return nil, err
	}
	if err = l.verifyInputs(); err != nil {
		return nil, err
	}
	if l.catalog, err = interpreterauthority.OpenRegistered(r.Interpreter.ID, r.Interpreter.SHA256, r.Binding); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *loaded) verifyBundle() error {
	raw, err := interpreterauthority.ReadProtected(filepath.Join(l.host.ToolBundle, "manifest.json"), 1<<20)
	if err != nil {
		return err
	}
	if hash(raw) != l.registration.Binding.ToolManifestSHA256 {
		return refused("stage0_bundle_changed")
	}
	var manifest bundleManifest
	if err = interpreterauthority.StrictJSON(raw, &manifest); err != nil {
		return err
	}
	if manifest.Schema != "ags.upgrade-tool-bundle.v2" || manifest.Entry != "upgrade_host.py" || len(manifest.Files) < 10 || len(manifest.Files) > 128 {
		return refused("stage0_bundle_invalid")
	}
	names := []string{"manifest.json"}
	for name, digest := range manifest.Files {
		if !interpreterauthority.ValidID(name) || !validDigest(digest) {
			return refused("stage0_bundle_invalid")
		}
		member, err := interpreterauthority.ReadProtected(filepath.Join(l.host.ToolBundle, name), 8<<20)
		if err != nil {
			return err
		}
		if hash(member) != digest {
			return refused("stage0_bundle_changed")
		}
		names = append(names, name)
	}
	for _, required := range []string{"routeb_stage0_client.py", "routeb_stage0_probe.py", "routeb_native_delegation.py", "routeb_native_recovery.py", "routeb_launcher.py", "routeb_host_transaction.py", "routeb_native_bootstrap.py", "routeb_host_package.py", "ags-runtime.py"} {
		if !validDigest(manifest.Files[required]) {
			return refused("stage0_bundle_incomplete")
		}
	}
	entries, err := os.ReadDir(l.host.ToolBundle)
	if err != nil {
		return refused("stage0_bundle_unavailable")
	}
	actual := []string{}
	for _, e := range entries {
		actual = append(actual, e.Name())
	}
	sort.Strings(actual)
	sort.Strings(names)
	if len(actual) != len(names) {
		return refused("stage0_bundle_members_changed")
	}
	for i := range names {
		if actual[i] != names[i] {
			return refused("stage0_bundle_members_changed")
		}
	}
	return nil
}

// Runtime-owned inputs are data, not executable authority. Their exact bytes
// are independently pinned by root registration and re-read by the Python
// package loader. This function cannot select a script from input content.
func readInput(f interpreterauthority.File, uid uint32) ([]byte, error) {
	fd, err := unix.Open(f.Path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, refused("stage0_input_unavailable")
	}
	file := os.NewFile(uintptr(fd), f.Path)
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, refused("stage0_input_unavailable")
	}
	s := before.Sys().(*syscall.Stat_t)
	if !before.Mode().IsRegular() || s.Uid != uid || s.Nlink != 1 || before.Mode().Perm() != 0600 || before.Size() != f.Size || f.Size > 16<<20 {
		return nil, refused("stage0_input_unsafe")
	}
	raw, err := io.ReadAll(io.LimitReader(file, (16<<20)+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(raw)) != f.Size || hash(raw) != f.SHA256 {
		return nil, refused("stage0_input_changed")
	}
	return raw, nil
}

func (l *loaded) verifyInputs() error {
	for _, f := range []*interpreterauthority.File{l.registration.Inputs.Package, l.registration.Inputs.Approval, l.registration.Inputs.BootstrapApproval} {
		if f != nil {
			if _, err := readInput(*f, l.host.UID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *loaded) recheckFiles() error {
	if err := l.verifyDefinitions(); err != nil {
		return err
	}
	if l.registration.ScopeGrant != nil {
		if _, err := interpreterauthority.ReadProtectedFile(*l.registration.ScopeGrant, 1<<20); err != nil {
			return err
		}
	}
	raw, err := interpreterauthority.ReadProtected(registrationPath(l.registration.ID), 1<<20)
	if err != nil || hash(raw) != l.registrationSHA {
		return refused("stage0_registration_changed")
	}
	if _, err = interpreterauthority.ReadProtectedFile(l.registration.HostProfile, 65536); err != nil {
		return err
	}
	if _, err = interpreterauthority.ReadProtectedFile(l.registration.NativeImage, 64<<20); err != nil {
		return err
	}
	if _, err = interpreterauthority.ReadProtectedFile(l.registration.NativeRecoveryImage, 64<<20); err != nil {
		return err
	}
	if err = l.verifyBundle(); err != nil {
		return err
	}
	if err = l.verifyInputs(); err != nil {
		return err
	}
	if l.recovery != nil {
		if err = l.recheckRecovery(); err != nil {
			return err
		}
	}
	return l.catalog.Recheck()
}

func decodeExact(raw []byte, value any) error {
	return interpreterauthority.StrictJSON(bytes.TrimSpace(raw), value)
}

func (l *loaded) verifyDefinitions() error {
	seen := map[string]bool{}
	for _, invocation := range l.registration.Invocations {
		for _, file := range []interpreterauthority.File{invocation.Definition, invocation.DefinitionRecovery} {
			if seen[file.Path] {
				continue
			}
			if _, err := interpreterauthority.ReadProtectedFile(file, 65536); err != nil {
				return err
			}
			seen[file.Path] = true
		}
	}
	return nil
}
