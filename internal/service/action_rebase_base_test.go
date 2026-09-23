package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgreeLiveRebaseBaseAllowsFastForwardAndRejectsRewrite(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", args, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("git", "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "a.txt")
	run("git", "commit", "-m", "first")
	first := run("git", "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "b.txt")
	run("git", "commit", "-m", "second")
	second := run("git", "rev-parse", "HEAD")

	ctx := context.Background()
	got, err := agreeLiveRebaseBase(ctx, dir, first, first, first)
	if err != nil || got != first {
		t.Fatalf("equal base: got=%s err=%v", got, err)
	}
	got, err = agreeLiveRebaseBase(ctx, dir, first, second, second)
	if err != nil || got != second {
		t.Fatalf("fast-forward base: got=%s err=%v", got, err)
	}
	if _, err := agreeLiveRebaseBase(ctx, dir, first, first, second); err == nil {
		t.Fatal("expected disagreeing AGS/Forgejo bases to fail")
	}
	if _, err := agreeLiveRebaseBase(ctx, dir, second, first, first); err == nil {
		t.Fatal("expected rewritten/older live base to fail")
	}
}
