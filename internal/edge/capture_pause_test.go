package edge_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

// Block the actual pack-objects process, not a fake lock implementation. A
// primary WriteFile/DeleteRepo must complete while that process cannot progress.
// A marker after packing starts also proves the source pin has already finished.
func TestPrimaryCaptureReleasesWritesBeforeNativePacking(t *testing.T) {
	for _, mode := range []string{"write", "delete"} {
		t.Run(mode, func(t *testing.T) {
			f := newControlFixture(t)
			old, err := f.svc.Git.HeadSHA(f.ctx, f.repo.FullName, "main")
			if err != nil {
				t.Fatal(err)
			}
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			realGit, err = filepath.Abs(realGit)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			entered, release := filepath.Join(dir, "packing"), filepath.Join(dir, "release")
			// Test-owned paths are quoted; no runtime user input or credentials.
			script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in\n*\" pack-objects \"*)\n  : > %q\n  while [ ! -f %q ]; do /bin/sleep 0.01; done\n  ;;\nesac\nexec %q \"$@\"\n", entered, release, realGit)
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			type result struct {
				plan edgeprotocol.ReadPlan
				err  error
			}
			done := make(chan result, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			go func() {
				plan, err := f.peer.PrepareRead(ctx, "edge-fixture-test", "Bearer "+f.token, f.request())
				done <- result{plan: plan, err: err}
			}()
			joined := false
			t.Cleanup(func() {
				_ = os.WriteFile(release, nil, 0600)
				cancel()
				if !joined {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("prepare did not drain")
					}
				}
			})
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(entered); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("native packing was not reached")
				}
				time.Sleep(5 * time.Millisecond)
			}
			writeCtx, stop := context.WithTimeout(f.ctx, 3*time.Second)
			defer stop()
			var newHead string
			if mode == "write" {
				newHead, err = f.svc.Git.WriteFile(writeCtx, f.repo.FullName, "main", "while-packing.txt", "concurrent write", []byte("allowed\n"))
			} else {
				err = f.svc.DeleteRepo(writeCtx, f.repo.FullName)
			}
			if err != nil {
				t.Fatalf("%s remained blocked behind packing: %v", mode, err)
			}
			select {
			case <-done:
				joined = true
				t.Fatal("prepare finished before the blocked pack was released")
			default:
			}
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			var got result
			select {
			case got = <-done:
				joined = true
			case <-time.After(10 * time.Second):
				t.Fatal("prepare failed to finish after packing was released")
			}
			if mode == "delete" {
				if got.err == nil {
					t.Fatal("deleted repository passed final reauthorization")
				}
				return
			}
			if got.err != nil || got.plan.Snapshot.HEAD.OID != old {
				t.Fatalf("capture changed while packing: %v", got.err)
			}
			view, err := f.retained.Acquire(context.Background(), got.plan.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			view.Release()
			next := f.prepare(t)
			if next.Snapshot.HEAD.OID != newHead || strings.TrimSpace(newHead) == "" {
				t.Fatal("later discovery did not observe concurrent write")
			}
		})
	}
}
