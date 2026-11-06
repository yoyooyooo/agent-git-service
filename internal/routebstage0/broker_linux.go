//go:build linux

package routebstage0

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
)

func (s *supervisor) serveInvocation(id string) (Invocation, error) {
	for _, i := range s.loaded.registration.Invocations {
		if i.Operation == "serve" && i.ID == id {
			return i, nil
		}
	}
	return Invocation{}, refused("stage0_runtime_invocation_unknown")
}

func (s *supervisor) validateRuntimeContext(c Context, i Invocation) error {
	l := s.loaded
	b := l.registration.Binding
	if c.Schema != "ags.routeb-stage0-context.v1" || c.RegistrationID != l.registration.ID || c.RegistrationSHA256 != l.registrationSHA ||
		c.Root != l.host.Root || c.Operation != "serve" || c.Phase != i.Phase || c.InvocationID != i.ID || c.AttemptID != i.AttemptID || c.AuthorityEpoch != i.AuthorityEpoch ||
		c.AuthorizationID != i.AuthorizationID || c.BootID != s.platform.boot || c.InterpreterAuthoritySHA256 != l.registration.Interpreter.SHA256 ||
		c.ToolRevision != b.ToolRevision || c.ToolTree != b.ToolTree || c.ToolManifestSHA256 != b.ToolManifestSHA256 {
		return refused("stage0_runtime_context_binding")
	}
	parent, err := processIdentity(c.Supervisor.PID, l.registration.NativeImage.Path, l.registration.NativeImage.SHA256)
	if err != nil || parent != c.Supervisor || parent.UID != 0 || parent.GID != 0 || parent.ParentPID != 1 {
		return refused("stage0_runtime_parent_changed")
	}
	child, err := processIdentity(c.Child.PID, s.loaderPath, s.loaderSHA)
	if err != nil || child != c.Child || child.ParentPID != parent.PID || child.UID != l.host.UID || child.GID != l.host.GID {
		return refused("stage0_runtime_child_changed")
	}
	status, err := procRead(child.PID, "status", 65536)
	if err != nil {
		return err
	}
	if _, err = privilegePolicy(status, false); err != nil {
		return err
	}
	for _, kind := range []string{"pid", "user", "mnt", "net"} {
		p, e1 := namespace(parent.PID, kind)
		ch, e2 := namespace(child.PID, kind)
		if e1 != nil || e2 != nil || p != ch {
			return refused("stage0_runtime_namespace")
		}
		if kind != "mnt" {
			init, e := namespace(1, kind)
			if e != nil || p != init {
				return refused("stage0_runtime_namespace")
			}
		}
	}
	if err = verifyMappings(child.PID, l.catalog); err != nil {
		return err
	}
	if err = l.verifyUnit(i, false); err != nil {
		return err
	}
	properties, err := managerProperties(i.Unit, "MainPID", "ActiveState", "SubState")
	if err != nil || properties["MainPID"] != decimal(parent.PID) || properties["ActiveState"] != "active" || properties["SubState"] != "running" {
		return refused("stage0_runtime_manager_changed")
	}
	return nil
}

func observedDescendants(root int) ([]ProcessIdentity, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil || len(entries) > 131072 {
		return nil, refused("stage0_process_inventory_unknown")
	}
	parents := map[int]int{}
	for _, entry := range entries {
		pid, e := strconv.Atoi(entry.Name())
		if e != nil || pid <= 0 {
			continue
		}
		raw, e := procRead(pid, "stat", 65536)
		if e != nil {
			continue
		}
		ppid, _, e := statBirth(raw)
		if e == nil {
			parents[pid] = ppid
		}
	}
	found := map[int]bool{root: true}
	changed := true
	for changed {
		changed = false
		for pid, parent := range parents {
			if found[parent] && !found[pid] {
				found[pid] = true
				changed = true
			}
		}
	}
	if len(found) > 256 {
		return nil, refused("stage0_process_inventory_limit")
	}
	pids := []int{}
	for pid := range found {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	result := []ProcessIdentity{}
	for _, pid := range pids {
		path, err := os.Readlink(filepath.Join("/proc", decimal(pid), "exe"))
		if err != nil || !absolute(path) {
			return nil, refused("stage0_descendant_image_unknown")
		}
		file, err := os.Open(filepath.Join("/proc", decimal(pid), "exe"))
		if err != nil {
			return nil, refused("stage0_descendant_image_unknown")
		}
		info, e := file.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1<<30 {
			file.Close()
			return nil, refused("stage0_descendant_image_unknown")
		}
		digest := sha256.New()
		n, e := io.Copy(digest, io.LimitReader(file, (1<<30)+1))
		file.Close()
		if e != nil || n != info.Size() {
			return nil, refused("stage0_descendant_image_unknown")
		}
		identity, e := processIdentity(pid, path, hex.EncodeToString(digest.Sum(nil)))
		if e != nil {
			return nil, e
		}
		result = append(result, identity)
	}
	return result, nil
}

func (s *supervisor) runtimeIdentity() (RuntimeIdentity, error) {
	if s.loaded.invocation.Operation == "serve" {
		return RuntimeIdentity{}, refused("stage0_control_operation_required")
	}
	found := []RuntimeIdentity{}
	for _, i := range s.loaded.registration.Invocations {
		if i.Operation != "serve" {
			continue
		}
		path := s.loaded.statePath("active", i.ID)
		missing, err := absentFile(path)
		if err != nil {
			return RuntimeIdentity{}, err
		}
		if missing {
			continue
		}
		raw, err := interpreterauthority.ReadProtected(path, 65536)
		if err != nil {
			return RuntimeIdentity{}, err
		}
		var c Context
		if err = decodeExact(raw, &c); err != nil {
			return RuntimeIdentity{}, err
		}
		if processGone(c.Supervisor) && processGone(c.Child) {
			continue
		}
		if err = s.validateRuntimeContext(c, i); err != nil {
			return RuntimeIdentity{}, err
		}
		pids, err := observedDescendants(c.Supervisor.PID)
		if err != nil {
			return RuntimeIdentity{}, err
		}
		group, err := cgroupMembers(i.Unit)
		if err != nil {
			return RuntimeIdentity{}, err
		}
		observed := []int{}
		for _, p := range pids {
			observed = append(observed, p.PID)
		}
		sort.Ints(observed)
		if !slices.Equal(group, observed) {
			return RuntimeIdentity{}, refused("stage0_cgroup_process_tree_mismatch")
		}
		found = append(found, RuntimeIdentity{c, pids, i.Unit})
	}
	if len(found) != 1 {
		return RuntimeIdentity{}, refused("stage0_runtime_missing_or_ambiguous")
	}
	return found[0], nil
}

func (s *supervisor) runtimeExit(id string) (ExitRecord, error) {
	result, _, err := s.runtimeExitEvidence(id)
	return result, err
}

func (s *supervisor) runtimeExitEvidence(id string) (ExitRecord, ManagerCompletion, error) {
	i, err := s.serveInvocation(id)
	if err != nil {
		return ExitRecord{}, ManagerCompletion{}, err
	}
	_, result, completion, err := s.loaded.completedManager(i, s.platform.boot)
	return result, completion, err
}

func (s *supervisor) managerAction(method string) error {
	l := s.loaded
	operation := l.invocation.Operation
	if method == "manager_stop" {
		if operation != "transaction" && operation != "restore" {
			return refused("stage0_manager_operation_invalid")
		}
		before, err := s.runtimeIdentity()
		if err != nil {
			return err
		}
		if operation == "transaction" && before.Context.Phase != "serve-initial" {
			return refused("stage0_manager_phase_invalid")
		}
		if operation == "restore" && before.Context.Phase != "serve-candidate" {
			return refused("stage0_manager_phase_invalid")
		}
		if err = writeExclusive(l.statePath("effects", l.invocation.ID+"-manager-stop-intent"), before); err != nil {
			return err
		}
		if _, err = systemctl("stop", before.ManagerUnit); err != nil {
			return err
		}
		result, err := s.runtimeExit(before.Context.InvocationID)
		if err != nil {
			return err
		}
		for _, pid := range before.PIDs {
			if !processGone(pid) {
				return refused("stage0_descendant_still_present")
			}
		}
		if err = writeExclusive(l.statePath("effects", l.invocation.ID+"-manager-stop-result"), result); err != nil {
			return err
		}
		s.stopped = &before
		return nil
	}
	if method != "manager_start" || (operation != "transaction" && operation != "old-start") {
		return refused("stage0_manager_operation_invalid")
	}
	phase := "serve-candidate"
	if operation == "transaction" {
		if s.stopped == nil {
			return refused("stage0_normal_stop_required")
		}
		if _, err := s.runtimeExit(s.stopped.Context.InvocationID); err != nil {
			return err
		}
	} else {
		if _, err := s.restoreResult(); err != nil {
			return err
		}
		phase = "serve-old"
		properties, err := managerProperties(unitName(l.registration.ID, "serve"), "MainPID", "ActiveState", "SubState", "Result")
		if err != nil || properties["MainPID"] != "0" || properties["ActiveState"] != "inactive" || properties["SubState"] != "dead" || properties["Result"] != "success" {
			return refused("stage0_recovery_manager_not_stopped")
		}
		group, err := cgroupMembers(unitName(l.registration.ID, "serve"))
		if err != nil || len(group) != 0 {
			return refused("stage0_runtime_cgroup_not_empty")
		}
	}
	var target *Invocation
	for _, i := range l.registration.Invocations {
		if i.Phase == phase {
			copy := i
			target = &copy
		}
	}
	if target == nil {
		return refused("stage0_start_phase_unregistered")
	}
	if err := l.verifyUnit(*target, false); err != nil {
		return err
	}
	ticket := Ticket{"ags.routeb-stage0-ticket.v1", l.registrationSHA, target.ID, target.Phase, target.AttemptID, target.AuthorityEpoch, target.AuthorizationID, l.invocation.ID, l.recoverySHA}
	if err := l.withStateLock(func() error {
		if err := writeExclusive(l.statePath("effects", l.invocation.ID+"-manager-start-intent"), ticket); err != nil {
			return err
		}
		return writeExclusive(l.statePath("tickets", target.ID), ticket)
	}); err != nil {
		return err
	}
	_, err := systemctl("start", target.Unit)
	return err
}
