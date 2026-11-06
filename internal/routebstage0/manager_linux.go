//go:build linux

package routebstage0

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
)

func systemctl(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 125*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/systemctl", append([]string{"--system"}, args...)...)
	command.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=C", "LC_ALL=C"}
	output := &boundedOutput{maximum: 1 << 20}
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	if err != nil {
		return nil, refused("stage0_manager_command_failed")
	}
	return output.Bytes(), nil
}

func managerProperties(unit string, names ...string) (map[string]string, error) {
	raw, err := systemctl("show", unit, "--no-pager", "--property="+strings.Join(names, ","))
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || !wanted[key] {
			return nil, refused("stage0_manager_output")
		}
		if _, exists := out[key]; exists {
			return nil, refused("stage0_manager_output")
		}
		out[key] = value
	}
	if len(out) != len(names) {
		return nil, refused("stage0_manager_output")
	}
	return out, nil
}

func (l *loaded) verifyUnit(i Invocation, own bool) error {
	raw, err := interpreterauthority.ReadProtectedFile(i.Definition, 65536)
	if err != nil {
		return err
	}
	section := ""
	service := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.Contains(line, "\\") {
			return refused("stage0_unit_syntax")
		}
		if section != "Service" {
			continue
		}
		if _, exists := service[key]; exists {
			return refused("stage0_unit_duplicate")
		}
		service[key] = value
	}
	expected := l.registration.NativeImage.Path + " serve --registration " + l.registration.ID + " --operation " + i.Operation
	if service["ExecStart"] != expected || service["User"] != "root" || service["Group"] != "root" || service["Type"] != "simple" ||
		service["Restart"] != "no" || service["KillMode"] != "mixed" || (service["TimeoutStopSec"] != "120" && service["TimeoutStopSec"] != "120s") || (service["NoNewPrivileges"] != "yes" && service["NoNewPrivileges"] != "true") {
		return refused("stage0_fixed_unit_required")
	}
	for key := range service {
		allowed := map[string]bool{"ExecStart": true, "User": true, "Group": true, "Type": true, "Restart": true, "KillMode": true, "TimeoutStopSec": true,
			"WorkingDirectory": true, "StandardOutput": true, "StandardError": true, "UMask": true, "NoNewPrivileges": true}
		if !allowed[key] {
			return refused("stage0_unit_extension_unsupported")
		}
	}
	if (service["WorkingDirectory"] != "" && service["WorkingDirectory"] != l.host.ToolBundle) ||
		(service["StandardOutput"] != "" && service["StandardOutput"] != "journal") || (service["StandardError"] != "" && service["StandardError"] != "journal") ||
		(service["UMask"] != "" && service["UMask"] != "0077") || (service["NoNewPrivileges"] != "" && service["NoNewPrivileges"] != "yes" && service["NoNewPrivileges"] != "true") {
		return refused("stage0_unit_extension_unsupported")
	}
	properties, err := managerProperties(i.Unit, "FragmentPath", "DropInPaths", "User", "Group", "Type", "Restart", "KillMode", "MainPID", "NoNewPrivileges")
	if err != nil {
		return err
	}
	if properties["FragmentPath"] != i.Definition.Path || properties["DropInPaths"] != "" || properties["User"] != "root" || properties["Group"] != "root" || properties["Type"] != "simple" || properties["Restart"] != "no" || properties["KillMode"] != "mixed" || properties["NoNewPrivileges"] != "yes" {
		return refused("stage0_effective_unit_changed")
	}
	if own {
		if properties["MainPID"] != decimal(os.Getpid()) {
			return refused("stage0_service_manager_identity")
		}
		cgroup, err := procRead(os.Getpid(), "cgroup", 65536)
		if err != nil {
			return err
		}
		found := false
		for _, line := range strings.Split(string(cgroup), "\n") {
			parts := strings.SplitN(line, ":", 3)
			if len(parts) == 3 && (parts[1] == "" || parts[1] == "name=systemd") && parts[2] == "/system.slice/"+i.Unit {
				found = true
			}
		}
		if !found {
			return refused("stage0_service_manager_cgroup")
		}
	}
	return nil
}

func cgroupMembers(unit string) ([]int, error) {
	properties, err := managerProperties(unit, "ControlGroup")
	if err != nil {
		return nil, err
	}
	group := "/system.slice/" + unit
	if properties["ControlGroup"] != "" && properties["ControlGroup"] != group {
		return nil, refused("stage0_cgroup_changed")
	}
	root := "/sys/fs/cgroup"
	if _, err = os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		return nil, refused("stage0_cgroup2_required")
	}
	path := filepath.Join(root, strings.TrimPrefix(group, "/"), "cgroup.procs")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []int{}, nil
	}
	if err != nil || len(raw) > 65536 {
		return nil, refused("stage0_cgroup_unavailable")
	}
	values := []int{}
	seen := map[int]bool{}
	for _, text := range strings.Fields(string(raw)) {
		pid, e := strconv.Atoi(text)
		if e != nil || pid <= 0 || seen[pid] || len(values) >= 256 {
			return nil, refused("stage0_cgroup_invalid")
		}
		seen[pid] = true
		values = append(values, pid)
	}
	sort.Ints(values)
	// Delegation and child cgroups are not part of this fixed service profile.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return nil, refused("stage0_cgroup_unavailable")
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, refused("stage0_cgroup_descendants_unmodeled")
		}
	}
	return values, nil
}
