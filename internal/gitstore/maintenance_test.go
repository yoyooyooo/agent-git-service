package gitstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func maintenanceFixture(t *testing.T) (*Store, string) {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Init(context.Background(), "owner/repo", "main", true); err != nil {
		t.Fatal(err)
	}
	dir, _ := s.GetRepoPath(context.Background(), "owner/repo")
	return s, dir
}
func maintenanceTestGit(t *testing.T, dir string, input []byte, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir", dir}, args...)...)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestMaintenancePreservesUserPRAndApplicationObjects(t *testing.T) {
	s, dir := maintenanceFixture(t)
	ctx := context.Background()
	sha, err := s.HeadSHA(ctx, "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CreatePRRef(ctx, "owner/repo", "owner/repo", sha, 7); err != nil {
		t.Fatal(err)
	}
	content := []byte("application-only revision, not reachable through a branch\n")
	oid := maintenanceTestGit(t, dir, content, "hash-object", "-w", "--stdin")
	path := filepath.Join(dir, "objects", oid[:2], oid[2:])
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err = os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	before := maintenanceTestGit(t, dir, nil, "show-ref", "--heads", "--tags")
	receipt, err := s.MaintainStorage(ctx, "owner/repo", func(context.Context) ([]string, error) { return []string{oid, sha, oid}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "completed" || receipt.ProtectedObjects != 2 || receipt.HistoryRewritten {
		t.Fatalf("receipt: %+v", receipt)
	}
	if after := maintenanceTestGit(t, dir, nil, "show-ref", "--heads", "--tags"); after != before {
		t.Fatal("user refs changed")
	}
	if got := maintenanceTestGit(t, dir, nil, "rev-parse", "refs/pull/7/head"); got != sha {
		t.Fatal("PR ref changed")
	}
	if got := maintenanceTestGit(t, dir, nil, "cat-file", "blob", oid); got != strings.TrimSpace(string(content)) {
		t.Fatal("application object lost")
	}
	loaded, err := s.ReadMaintenanceReceipt(ctx, "owner/repo")
	if err != nil || loaded.Status != "completed" {
		t.Fatalf("stored receipt: %+v %v", loaded, err)
	}
	// Protection is conservative even when a later consumer no longer enumerates
	// a historical fact. Only a separate authorized migration may retire it.
	if _, err = s.MaintainStorage(ctx, "owner/repo", func(context.Context) ([]string, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	maintenanceTestGit(t, dir, nil, "cat-file", "-e", oid)
	maintenanceTestGit(t, dir, nil, "fsck", "--connectivity-only")
}

func TestMaintenanceRefusesIncompleteApplicationInventory(t *testing.T) {
	for _, name := range []string{"query-failure", "no-provider"} {
		t.Run(name, func(t *testing.T) {
			s, dir := maintenanceFixture(t)
			before := maintenanceTestGit(t, dir, nil, "show-ref")
			loader := func(context.Context) ([]string, error) { return nil, errors.New("private database detail") }
			if name == "no-provider" {
				loader = nil
			}
			receipt, err := s.MaintainStorage(context.Background(), "owner/repo", loader)
			if err == nil || receipt.Status != "failed" {
				t.Fatalf("expected fail-closed: %+v %v", receipt, err)
			}
			if strings.Contains(err.Error(), "private database detail") {
				t.Fatal("database detail leaked")
			}
			if after := maintenanceTestGit(t, dir, nil, "show-ref"); after != before {
				t.Fatal("refs changed on failed preflight")
			}
		})
	}
}

func TestMaintenanceYieldsToForegroundAndDrainsNativeChild(t *testing.T) {
	s, _ := maintenanceFixture(t)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.WithMaintenance(context.Background(), func(ctx context.Context) error {
			cmd := exec.CommandContext(ctx, "sleep", "30")
			configureMaintenanceCommand(cmd)
			if err := cmd.Start(); err != nil {
				return err
			}
			close(started)
			return cmd.Wait()
		})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("maintenance did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, release, err := s.BeginMutation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("maintenance was not cancelled")
		}
	case <-ctx.Done():
		t.Fatal("native child outlived lease")
	}
	if err = s.WithMaintenance(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatal("lease not released", err)
	}
}

func TestMaintenanceNeverQueuesAheadOfActiveMutation(t *testing.T) {
	s, _ := maintenanceFixture(t)
	ctx, release, err := s.BeginMutation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err = s.WithMaintenance(ctx, func(context.Context) error { t.Fatal("maintenance entered while mutation active"); return nil }); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("expected busy: %v", err)
	}
}
