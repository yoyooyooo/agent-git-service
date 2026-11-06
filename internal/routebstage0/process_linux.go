//go:build linux

package routebstage0

import (
	"debug/elf"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
	"golang.org/x/sys/unix"
)

type platformFacts struct{ boot, pidNamespace, userNamespace, mountNamespace, netNamespace, ptrace, privileges string }

func procRead(pid int, name string, limit int) ([]byte, error) {
	if pid <= 0 {
		return nil, refused("stage0_process_invalid")
	}
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), name))
	if err != nil || len(raw) > limit {
		return nil, refused("stage0_process_unavailable")
	}
	return raw, nil
}

func statBirth(raw []byte) (int, uint64, error) {
	text := string(raw)
	end := strings.LastIndex(text, ") ")
	if end < 0 {
		return 0, 0, refused("stage0_process_stat_invalid")
	}
	fields := strings.Fields(text[end+2:])
	if len(fields) < 20 {
		return 0, 0, refused("stage0_process_stat_invalid")
	}
	parent, e1 := strconv.Atoi(fields[1])
	birth, e2 := strconv.ParseUint(fields[19], 10, 64)
	if e1 != nil || e2 != nil || parent < 0 || birth == 0 || fields[0] == "Z" || fields[0] == "X" {
		return 0, 0, refused("stage0_process_not_living")
	}
	return parent, birth, nil
}

func statusCredentials(raw []byte) (uint32, uint32, error) {
	values := map[string]uint32{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, rest, found := strings.Cut(line, ":")
		if !found || (key != "Uid" && key != "Gid") {
			continue
		}
		if _, ok := values[key]; ok {
			return 0, 0, refused("stage0_process_credentials")
		}
		parts := strings.Fields(rest)
		if len(parts) != 4 {
			return 0, 0, refused("stage0_process_credentials")
		}
		value, err := strconv.ParseUint(parts[0], 10, 32)
		if err != nil {
			return 0, 0, refused("stage0_process_credentials")
		}
		for _, part := range parts[1:] {
			if part != parts[0] {
				return 0, 0, refused("stage0_process_credentials")
			}
		}
		values[key] = uint32(value)
	}
	if len(values) != 2 {
		return 0, 0, refused("stage0_process_credentials")
	}
	return values["Uid"], values["Gid"], nil
}

func processIdentity(pid int, expectedPath, expectedHash string) (ProcessIdentity, error) {
	raw, err := procRead(pid, "stat", 65536)
	if err != nil {
		return ProcessIdentity{}, err
	}
	parent, birth, err := statBirth(raw)
	if err != nil {
		return ProcessIdentity{}, err
	}
	status, err := procRead(pid, "status", 65536)
	if err != nil {
		return ProcessIdentity{}, err
	}
	uid, gid, err := statusCredentials(status)
	if err != nil {
		return ProcessIdentity{}, err
	}
	actual, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil || actual != expectedPath {
		return ProcessIdentity{}, refused("stage0_process_image")
	}
	// The independently verified catalog/native image pins bytes; the proc
	// link pins which inode actually backs this living process.
	expected, err := os.Stat(expectedPath)
	if err != nil {
		return ProcessIdentity{}, refused("stage0_process_image")
	}
	mapped, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil || !os.SameFile(expected, mapped) {
		return ProcessIdentity{}, refused("stage0_process_image")
	}
	fresh, err := procRead(pid, "stat", 65536)
	if err != nil {
		return ProcessIdentity{}, err
	}
	p, b, err := statBirth(fresh)
	if err != nil || p != parent || b != birth {
		return ProcessIdentity{}, refused("stage0_process_replaced")
	}
	return ProcessIdentity{pid, birth, parent, uid, gid, expectedHash}, nil
}

func namespace(pid int, kind string) (string, error) {
	value, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "ns", kind))
	if err != nil || !strings.HasPrefix(value, kind+":[") || !strings.HasSuffix(value, "]") {
		return "", refused("stage0_namespace_unavailable")
	}
	return value, nil
}

func platformPrerequisites(l *loaded) (platformFacts, ProcessIdentity, error) {
	if os.Getuid() != 0 || os.Geteuid() != 0 || os.Getgid() != 0 || os.Getegid() != 0 || os.Getppid() != 1 {
		return platformFacts{}, ProcessIdentity{}, refused("stage0_root_service_manager_required")
	}
	if err := interpreterauthority.ProtectedDirectory(SocketDirectory, false); err != nil {
		return platformFacts{}, ProcessIdentity{}, err
	}
	if err := interpreterauthority.ProtectedDirectory(filepath.Join(rootFor(l.registration.ID), "state"), true); err != nil {
		return platformFacts{}, ProcessIdentity{}, err
	}
	actual, err := os.Readlink("/proc/self/exe")
	if err := l.verifyAccountResources(); err != nil {
		return platformFacts{}, ProcessIdentity{}, err
	}
	if err != nil || actual != l.registration.NativeImage.Path {
		return platformFacts{}, ProcessIdentity{}, refused("stage0_native_image")
	}
	image, err := elf.Open(actual)
	if err != nil {
		return platformFacts{}, ProcessIdentity{}, refused("stage0_native_image")
	}
	defer image.Close()
	for _, p := range image.Progs {
		if p.Type == elf.PT_INTERP || p.Type == elf.PT_DYNAMIC {
			return platformFacts{}, ProcessIdentity{}, refused("stage0_static_native_image_required")
		}
	}
	current, err := processIdentity(os.Getpid(), actual, l.registration.NativeImage.SHA256)
	if err != nil {
		return platformFacts{}, ProcessIdentity{}, err
	}
	account, err := user.LookupId(strconv.Itoa(int(l.host.UID)))
	if err != nil || account.Username != l.host.User || account.HomeDir != l.host.Home || account.Gid != strconv.Itoa(int(l.host.GID)) {
		return platformFacts{}, ProcessIdentity{}, refused("stage0_account_changed")
	}
	machine, err := interpreterauthority.ReadProtected("/etc/machine-id", 4096)
	if err != nil {
		return platformFacts{}, ProcessIdentity{}, err
	}
	machineText := strings.ToLower(strings.TrimSpace(string(machine)))
	decoded, decodeErr := hex.DecodeString(machineText)
	if decodeErr != nil || len(decoded) != 16 || hash([]byte("ags.host-machine.v1\x00machine-id\x00"+machineText)) != l.host.MachineIDSHA256 {
		return platformFacts{}, ProcessIdentity{}, refused("stage0_machine_changed")
	}
	facts, err := currentPlatformFacts()
	if err != nil {
		return platformFacts{}, ProcessIdentity{}, err
	}
	return facts, current, nil
}

func currentPlatformFacts() (platformFacts, error) {
	var f platformFacts
	status, err := procRead(os.Getpid(), "status", 65536)
	if err != nil {
		return f, err
	}
	f.privileges, err = privilegePolicy(status, true)
	if err != nil {
		return f, err
	}
	raw, err := os.ReadFile("/proc/sys/kernel/yama/ptrace_scope")
	if err != nil {
		return f, refused("stage0_yama_required")
	}
	f.ptrace = strings.TrimSpace(string(raw))
	if f.ptrace != "1" && f.ptrace != "2" && f.ptrace != "3" {
		return f, refused("stage0_yama_required")
	}
	raw, err = os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return f, refused("stage0_boot_unavailable")
	}
	f.boot = strings.TrimSpace(string(raw))
	if len(f.boot) != 36 {
		return f, refused("stage0_boot_invalid")
	}
	f.pidNamespace, err = namespace(os.Getpid(), "pid")
	if err != nil {
		return f, err
	}
	initPID, e := namespace(1, "pid")
	if e != nil || initPID != f.pidNamespace {
		return f, refused("stage0_initial_namespace_required")
	}
	f.userNamespace, err = namespace(os.Getpid(), "user")
	if err != nil {
		return f, err
	}
	initUser, e := namespace(1, "user")
	if e != nil || initUser != f.userNamespace {
		return f, refused("stage0_initial_namespace_required")
	}
	f.mountNamespace, err = namespace(os.Getpid(), "mnt")
	if err != nil {
		return f, err
	}
	f.netNamespace, err = namespace(os.Getpid(), "net")
	if err != nil {
		return f, err
	}
	initNet, err := namespace(1, "net")
	if err != nil || initNet != f.netNamespace {
		return f, refused("stage0_initial_network_namespace_required")
	}
	var procFS unix.Statfs_t
	if unix.Statfs("/proc", &procFS) != nil || procFS.Type != unix.PROC_SUPER_MAGIC {
		return f, refused("stage0_procfs_required")
	}
	return f, nil
}

func verifyChildNamespaces(pid int, facts platformFacts) error {
	status, err := procRead(pid, "status", 65536)
	if err != nil {
		return err
	}
	if _, err = privilegePolicy(status, false); err != nil {
		return err
	}
	for kind, want := range map[string]string{"pid": facts.pidNamespace, "user": facts.userNamespace, "mnt": facts.mountNamespace, "net": facts.netNamespace} {
		got, err := namespace(pid, kind)
		if err != nil || got != want {
			return refused("stage0_child_namespace")
		}
	}
	return nil
}

func verifyMappings(pid int, catalog *interpreterauthority.RegisteredCatalog) error {
	raw, err := procRead(pid, "maps", 4<<20)
	if err != nil {
		return err
	}
	known := catalog.Files()
	main := catalog.Evidence().Executable
	seenMain := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 5 {
			return refused("stage0_mapping_invalid")
		}
		inode, err := strconv.ParseUint(parts[4], 10, 64)
		if err != nil {
			return refused("stage0_mapping_invalid")
		}
		if inode == 0 {
			if len(parts) > 5 && (!strings.HasPrefix(parts[5], "[") || !strings.HasSuffix(parts[5], "]")) {
				return refused("stage0_mapping_unknown")
			}
			continue
		}
		if len(parts) != 6 || !absolute(parts[5]) {
			return refused("stage0_mapping_unknown")
		}
		entry, ok := known[parts[5]]
		if !ok || entry.Inode != inode {
			return refused("stage0_mapping_unknown")
		}
		var major, minor uint64
		if _, err = fmt.Sscanf(parts[3], "%x:%x", &major, &minor); err != nil || unix.Mkdev(uint32(major), uint32(minor)) != entry.Device {
			return refused("stage0_mapping_changed")
		}
		if parts[5] == main {
			seenMain = true
		}
	}
	if !seenMain {
		return refused("stage0_python_main_not_mapped")
	}
	return nil
}

func childProcessAttributes(uid, gid uint32) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{}}, Setsid: true, Pdeathsig: syscall.SIGTERM}
}
