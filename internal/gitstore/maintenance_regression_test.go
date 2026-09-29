package gitstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceMissingApplicationObjectPreservesAllUnreachableData(t *testing.T) {
	s, dir := maintenanceFixture(t)
	oid := maintenanceTestGit(t, dir, []byte("unreachable but not authorised for pruning\n"), "hash-object", "-w", "--stdin")
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "objects", oid[:2], oid[2:]), old, old); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.MaintainStorage(context.Background(), "owner/repo", func(context.Context) ([]string, error) { return []string{strings.Repeat("a", 40)}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "compacted_no_prune" || receipt.PruningEnabled || receipt.MissingApplicationObjects != 1 || receipt.ErrorCode != "application_objects_missing" {
		t.Fatalf("unsafe claim %+v", receipt)
	}
	maintenanceTestGit(t, dir, nil, "cat-file", "-e", oid)
	if got := maintenanceTestGit(t, dir, nil, "for-each-ref", "--format=%(refname)", "refs/ags/retention/"); got != "" {
		t.Fatal("invented a missing object root")
	}
}

func TestMaintenanceAppliesGraceOnlyToUnprotectedObjects(t *testing.T) {
	s, dir := maintenanceFixture(t)
	oldOID := maintenanceTestGit(t, dir, []byte("old unreferenced object\n"), "hash-object", "-w", "--stdin")
	freshOID := maintenanceTestGit(t, dir, []byte("new object awaiting ref publication\n"), "hash-object", "-w", "--stdin")
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "objects", oldOID[:2], oldOID[2:]), old, old); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.MaintainStorage(context.Background(), "owner/repo", func(context.Context) ([]string, error) { return nil, nil })
	if err != nil || !receipt.PruningEnabled || receipt.Status != "completed" {
		t.Fatalf("maintenance %+v %v", receipt, err)
	}
	maintenanceTestGit(t, dir, nil, "cat-file", "-e", freshOID)
	if err := exec.Command("git", "--git-dir", dir, "cat-file", "-e", oldOID).Run(); err == nil {
		t.Fatal("old unprotected object was not reclaimed")
	}
}

func TestMaintenanceOtherRepositoryWorkContinuesAndSameRepositoryCancels(t *testing.T) {
	s, _ := maintenanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Init(ctx, "other/repo", "main", true); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.withRepositoryMaintenance(ctx, "owner/repo", func(run context.Context) error { close(started); <-run.Done(); return run.Err() })
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("maintenance did not start")
	}
	if _, err := s.WriteFile(ctx, "other/repo", "main", "readme.txt", "other work", []byte("not blocked\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("other repository interrupted maintenance")
	default:
	}
	if _, err := s.HeadSHA(ctx, "owner/repo", "main"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("same repo failed to preempt: %v", err)
	}
}

func TestPreparedCommitAndLazyTreeCacheSurviveRepacking(t *testing.T) {
	s, dir := maintenanceFixture(t)
	ctx := context.Background()
	_, err := s.CommitFiles(ctx, "owner/repo", "main", "seed", []FileMutation{{Path: "cold/deep/file.txt", Content: []byte("cold\n")}, {Path: "warm/file.txt", Content: []byte("warm\n")}})
	if err != nil {
		t.Fatal(err)
	}
	// Publish one pack, then prepare against it so repacking replaces the native
	// files referenced by the cached go-git storage object.
	if _, err = s.MaintainStorage(ctx, "owner/repo", func(context.Context) ([]string, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	prepared, err := s.PrepareCommitFilesAt(ctx, "owner/repo", "main", "prepare", []FileMutation{{Path: "warm/file.txt", Content: []byte("changed\n")}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.MaintainStorage(ctx, "owner/repo", func(context.Context) ([]string, error) { return []string{prepared.SHA}, nil }); err != nil {
		t.Fatal(err)
	}
	if err = s.PublishPreparedCommit(ctx, "owner/repo", "main", prepared); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitFiles(ctx, "owner/repo", "main", "open previously cold subtree", []FileMutation{{Path: "cold/deep/file.txt", Content: []byte("after repack\n")}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadFile(ctx, "owner/repo", "cold/deep/file.txt")
	if err != nil || string(got) != "after repack\n" {
		t.Fatalf("cache read: %q %v", got, err)
	}
	maintenanceTestGit(t, dir, nil, "fsck", "--connectivity-only")
}
