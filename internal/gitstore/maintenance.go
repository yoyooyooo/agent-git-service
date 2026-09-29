package gitstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrMaintenanceBusy = errors.New("repository maintenance yielded to foreground work")

// StorageStats describes physical Git storage, not expected savings. Git reports
// these sizes in KiB. Ref counts include internal PR and retention namespaces.
type StorageStats struct {
	LooseObjects  int64          `json:"loose_objects"`
	LooseKiB      int64          `json:"loose_kib"`
	PackedObjects int64          `json:"packed_objects"`
	PackKiB       int64          `json:"pack_kib"`
	Packs         int64          `json:"packs"`
	GarbageKiB    int64          `json:"garbage_kib"`
	RefCounts     map[string]int `json:"ref_counts"`
	RefDigest     string         `json:"ref_digest"`
}

type MaintenanceReceipt struct {
	Schema                    string       `json:"schema"`
	StartedAt                 time.Time    `json:"started_at"`
	FinishedAt                time.Time    `json:"finished_at"`
	Status                    string       `json:"status"`
	ErrorCode                 string       `json:"error_code,omitempty"`
	Phase                     string       `json:"phase,omitempty"`
	InventorySource           string       `json:"inventory_source,omitempty"`
	ProtectedObjects          int          `json:"protected_objects"`
	MissingApplicationObjects int          `json:"missing_application_objects"`
	PruningEnabled            bool         `json:"pruning_enabled"`
	Before                    StorageStats `json:"before"`
	After                     StorageStats `json:"after"`
	// Repacking changes representation only; this is deliberately not a history
	// rewrite or a claim about filesystem snapshots / external replicas.
	HistoryRewritten bool `json:"history_rewritten"`
}

func maintenanceGit(ctx context.Context, dir string, input []byte, args ...string) ([]byte, error) {
	base := []string{"--no-pager", "--git-dir", dir, "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "gc.autoDetach=false", "-c", "gc.reflogExpire=never", "-c", "gc.reflogExpireUnreachable=never", "-c", "core.hooksPath=" + os.DevNull}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	// Inherited Git routing/config variables must not redirect maintenance to
	// a different object store. No network or credential helpers are needed.
	cmd.Env = []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=file")
	cmd.Stdin = bytes.NewReader(input)
	// No credential or command output is included in errors/receipts.
	stdout := &boundedMaintenanceOutput{limit: 16 * 1024 * 1024}
	cmd.Stdout = stdout
	cmd.Stderr = io.Discard
	configureMaintenanceCommand(cmd)
	err := cmd.Run()
	finishMaintenanceCommand(cmd, ctx.Err() != nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("git maintenance command %s failed", args[0])
	}
	return stdout.Bytes(), nil
}

type boundedMaintenanceOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedMaintenanceOutput) Bytes() []byte { return b.buffer.Bytes() }
func (b *boundedMaintenanceOutput) Write(p []byte) (int, error) {
	// Do not embed bytes.Buffer: its promoted ReadFrom would let io.Copy
	// bypass this bound when exec copies child stdout.
	if len(p) > b.limit-b.buffer.Len() {
		return 0, errors.New("maintenance output exceeds budget")
	}
	return b.buffer.Write(p)
}

func (s *Store) InspectStorage(ctx context.Context, fullName string) (StorageStats, error) {
	dir, err := s.repoPath(ctx, fullName)
	if err != nil {
		return StorageStats{}, err
	}
	return inspectStorage(ctx, dir)
}
func inspectStorage(ctx context.Context, dir string) (StorageStats, error) {
	out, err := maintenanceGit(ctx, dir, nil, "count-objects", "-v")
	if err != nil {
		return StorageStats{}, err
	}
	st := StorageStats{RefCounts: map[string]int{}}
	fields := map[string]*int64{"count": &st.LooseObjects, "size": &st.LooseKiB, "in-pack": &st.PackedObjects, "packs": &st.Packs, "size-pack": &st.PackKiB, "size-garbage": &st.GarbageKiB}
	for _, line := range strings.Split(string(out), "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		if key == "alternate" {
			return st, errors.New("maintenance does not support alternate object stores")
		}
		if dst := fields[key]; dst != nil {
			v, e := strconv.ParseInt(val, 10, 64)
			if e != nil || v < 0 {
				return st, errors.New("invalid Git storage statistics")
			}
			*dst = v
		}
	}
	refs, err := maintenanceGit(ctx, dir, nil, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return st, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "/", 3)
		if len(parts) != 3 {
			return st, errors.New("invalid repository ref")
		}
		st.RefCounts[parts[1]]++
	}
	sum := sha256.Sum256(refs)
	st.RefDigest = hex.EncodeToString(sum[:])
	return st, nil
}

func (s *Store) ReadMaintenanceReceipt(ctx context.Context, fullName string) (MaintenanceReceipt, error) {
	var receipt MaintenanceReceipt
	dir, err := s.repoPath(ctx, fullName)
	if err != nil {
		return receipt, err
	}
	p := filepath.Join(dir, "ags-maintenance.json")
	stat, err := os.Lstat(p)
	if err != nil {
		return receipt, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 64*1024 {
		return receipt, errors.New("invalid maintenance receipt")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return receipt, err
	}
	err = json.Unmarshal(data, &receipt)
	if err != nil {
		return receipt, errors.New("invalid maintenance receipt JSON")
	}
	if receipt.Schema != "ags.git-maintenance.v1" || receipt.StartedAt.IsZero() || receipt.FinishedAt.Before(receipt.StartedAt) {
		return receipt, errors.New("invalid maintenance receipt identity")
	}
	switch receipt.Status {
	case "completed", "compacted_no_prune", "failed", "deferred":
	default:
		return receipt, errors.New("invalid maintenance receipt state")
	}
	return receipt, nil
}

// WithMaintenance is the conservative all-repository entry for operations
// whose complete scope is not known. Normal GC uses a specific repository.
func (s *Store) WithMaintenance(ctx context.Context, fn func(context.Context) error) error {
	return s.withRepositoryMaintenance(ctx, "", fn)
}

// MaintainStorage protects application-owned object roots before native GC.
// It does not delete user/PR refs, expire reflogs or rewrite any commit. Callers
// must include every durable application root and fail closed on query failure.
// Stale protection refs intentionally remain: only an explicit history migration
// may retire an application fact. This prevents GC from inventing retention policy.
func (s *Store) MaintainStorage(ctx context.Context, fullName string, loadRoots func(context.Context) ([]string, error)) (MaintenanceReceipt, error) {
	receipt := MaintenanceReceipt{Schema: "ags.git-maintenance.v1", StartedAt: time.Now().UTC(), Status: "running", Phase: "admission"}
	dir, err := s.repoPath(ctx, fullName)
	if err != nil {
		return receipt, err
	}
	err = s.withRepositoryMaintenance(ctx, fullName, func(runCtx context.Context) (runErr error) {
		receipt.Phase = "storage_lock"
		release, e := lockRepositoryMaintenance(dir)
		if e != nil {
			return e
		}
		defer release()
		// Finalise while this exact repository is still protected. A refused
		// symlink/lock preflight must not write a receipt through the bad path.
		defer func() {
			finishMaintenanceReceipt(&receipt, runErr)
			if receipt.Status != "deferred" {
				if saveErr := saveMaintenanceReceipt(dir, receipt); saveErr != nil && runErr == nil {
					runErr = saveErr
					receipt.Status = "failed"
					receipt.ErrorCode = "receipt_write_failed"
				}
			}
		}()
		receipt.Phase = "storage_inventory"
		before, e := inspectStorage(runCtx, dir)
		if e != nil {
			return e
		}
		receipt.Before = before
		receipt.Phase = "application_inventory"
		if loadRoots == nil {
			return errors.New("application object protection is required")
		}
		roots, e := loadRoots(runCtx)
		// Foreground access can cancel a database query. Do not turn a normal
		// yield into a persisted failure/backoff by discarding that context.
		if runCtx.Err() != nil {
			return runCtx.Err()
		}
		if e != nil {
			receipt.ErrorCode, receipt.InventorySource = rootInventoryDiagnostic(e)
			return errors.New("application object inventory failed")
		}
		receipt.Phase = "root_validation"
		set := map[string]bool{}
		if len(roots) > 100000 {
			return errors.New("application object inventory exceeds budget")
		}
		for _, oid := range roots {
			if oid == "" {
				continue
			}
			if !canonicalMaintenanceOID(oid) {
				return errors.New("invalid application object root")
			}
			set[oid] = true
		}
		ordered := make([]string, 0, len(set))
		for oid := range set {
			ordered = append(ordered, oid)
		}
		sort.Strings(ordered)
		receipt.Phase = "object_inventory"
		if len(ordered) > 0 {
			out, e := maintenanceGit(runCtx, dir, []byte(strings.Join(ordered, "\n")+"\n"), "cat-file", "--batch-check=%(objectname) %(objecttype)")
			if e != nil {
				return e
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if len(lines) != len(ordered) {
				return errors.New("incomplete application object inventory")
			}
			present := make([]string, 0, len(ordered))
			for i, line := range lines {
				parts := strings.Fields(line)
				if len(parts) != 2 || parts[0] != ordered[i] {
					return errors.New("invalid application object inventory")
				}
				if parts[1] == "missing" {
					receipt.MissingApplicationObjects++
					continue
				}
				switch parts[1] {
				case "commit", "tree", "blob", "tag":
				default:
					return errors.New("invalid application object type")
				}
				present = append(present, ordered[i])
			}
			ordered = present
			receipt.Phase = "root_protection"
			current, e := maintenanceGit(runCtx, dir, nil, "for-each-ref", "--format=%(refname) %(objectname)", "refs/ags/retention/")
			if e != nil {
				return e
			}
			existing := map[string]bool{}
			for _, line := range strings.Split(strings.TrimSpace(string(current)), "\n") {
				if line == "" {
					continue
				}
				parts := strings.Fields(line)
				if len(parts) != 2 || parts[0] != "refs/ags/retention/"+parts[1] {
					return errors.New("invalid existing application retention ref")
				}
				existing[parts[1]] = true
			}
			var tx strings.Builder
			tx.WriteString("start\n")
			for _, oid := range ordered {
				if existing[oid] {
					fmt.Fprintf(&tx, "verify refs/ags/retention/%s %s\n", oid, oid)
				} else {
					fmt.Fprintf(&tx, "create refs/ags/retention/%s %s\n", oid, oid)
				}
			}
			tx.WriteString("prepare\ncommit\n")
			if _, e = maintenanceGit(runCtx, dir, []byte(tx.String()), "update-ref", "--stdin"); e != nil {
				return e
			}
		}
		receipt.ProtectedObjects = len(ordered)
		pinned, e := maintenanceGit(runCtx, dir, nil, "for-each-ref", "--format=%(refname) %(objectname)")
		if e != nil {
			return e
		}
		receipt.Phase = "space_preflight"
		if e = maintenanceSpaceAvailable(dir, before); e != nil {
			return e
		}
		receipt.Phase = "connectivity_preflight"
		if _, e = maintenanceGit(runCtx, dir, nil, "fsck", "--connectivity-only", "--no-dangling"); e != nil {
			return e
		}
		// Missing historical application facts are observable, not permission to
		// prune more aggressively. Keep every unreachable packed object and every
		// loose object in that case. This cannot claim the missing facts healed.
		args := []string{"-c", "pack.threads=2", "-c", "pack.windowMemory=128m"}
		if receipt.MissingApplicationObjects > 0 {
			args = append(args, "repack", "-a", "-d", "--keep-unreachable")
		} else {
			receipt.PruningEnabled = true
			args = append(args, "gc", "--quiet", "--prune=2.weeks.ago")
		}
		receipt.Phase = "native_maintenance"
		if _, e = maintenanceGit(runCtx, dir, nil, args...); e != nil {
			return e
		}
		receipt.Phase = "connectivity_readback"
		if _, e = maintenanceGit(runCtx, dir, nil, "fsck", "--connectivity-only", "--no-dangling"); e != nil {
			return e
		}
		receipt.Phase = "ref_readback"
		afterRefs, e := maintenanceGit(runCtx, dir, nil, "for-each-ref", "--format=%(refname) %(objectname)")
		if e != nil {
			return e
		}
		if !bytes.Equal(pinned, afterRefs) {
			return errors.New("repository refs changed during maintenance")
		}
		receipt.Phase = "storage_readback"
		receipt.After, e = inspectStorage(runCtx, dir)
		if e == nil {
			receipt.Phase = "complete"
		}
		return e
	})
	if receipt.FinishedAt.IsZero() {
		finishMaintenanceReceipt(&receipt, err)
	}
	return receipt, err
}

func finishMaintenanceReceipt(receipt *MaintenanceReceipt, err error) {
	receipt.FinishedAt = time.Now().UTC()
	switch {
	case err == nil:
		receipt.Status = "completed"
		if receipt.MissingApplicationObjects > 0 {
			receipt.Status = "compacted_no_prune"
			receipt.ErrorCode = "application_objects_missing"
		}
	case errors.Is(err, ErrMaintenanceBusy), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		receipt.Status = "deferred"
		receipt.ErrorCode = "foreground_or_shutdown"
	default:
		receipt.Status = "failed"
		if receipt.ErrorCode == "" {
			receipt.ErrorCode = "maintenance_failed"
		}
	}
}
func canonicalMaintenanceOID(oid string) bool {
	if len(oid) != 40 {
		return false
	}
	for _, c := range oid {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return oid != strings.Repeat("0", 40)
}
func saveMaintenanceReceipt(dir string, r MaintenanceReceipt) error {
	p := filepath.Join(dir, "ags-maintenance.json")
	if st, e := os.Lstat(p); e == nil && !st.Mode().IsRegular() {
		return errors.New("invalid maintenance receipt destination")
	} else if e != nil && !os.IsNotExist(e) {
		return e
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".ags-maintenance-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
