package gitstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var ErrMergePrecondition = errors.New("merge reference precondition failed")
var mergeSHA = regexp.MustCompile(`^[a-f0-9]{40}$`)

func verifyMergePreconditions(ctx context.Context, clone string, opts tempCloneOptions) error {
	if !mergeSHA.MatchString(opts.expectedHead) || !mergeSHA.MatchString(opts.expectedBase) || opts.headBranch == opts.pushBranch || !IsValidRefName("refs/heads/"+opts.headBranch) || !IsValidRefName("refs/heads/"+opts.pushBranch) {
		return ErrMergePrecondition
	}
	for ref, want := range map[string]string{opts.headBranch: opts.expectedHead, opts.pushBranch: opts.expectedBase} {
		out, e := exec.CommandContext(ctx, "git", "-C", clone, "rev-parse", "--verify", "refs/remotes/origin/"+ref).Output()
		if e != nil || strings.TrimSpace(string(out)) != want {
			return ErrMergePrecondition
		}
	}
	return nil
}

// publishCheckedMerge imports immutable objects, then uses ONE Git ref transaction
// to verify the source head and compare/swap the destination base. The verification
// takes the source ref lock too, including against concurrent receive-pack. A Go
// mutex or a preflight read alone would not provide this cross-process guarantee.
func publishCheckedMerge(ctx context.Context, bare, clone, newSHA string, opts tempCloneOptions) error {
	if !mergeSHA.MatchString(newSHA) || !mergeSHA.MatchString(opts.expectedHead) || !mergeSHA.MatchString(opts.expectedBase) || opts.headBranch == opts.pushBranch || !IsValidRefName("refs/heads/"+opts.headBranch) || !IsValidRefName("refs/heads/"+opts.pushBranch) {
		return ErrMergePrecondition
	}
	if _, e := exec.CommandContext(ctx, "git", "--git-dir", bare, "fetch", "--no-tags", "--no-write-fetch-head", "--no-auto-maintenance", clone, newSHA).CombinedOutput(); e != nil {
		return fmt.Errorf("import merge objects: %w", e)
	}
	ref := "refs/heads/" + opts.pushBranch
	update := fmt.Sprintf("%s %s %s\n", opts.expectedBase, newSHA, ref)
	// Preserve repository-owned receive policy; a direct reference transaction
	// is not permission to bypass hooks used by the ordinary Git push path.
	if err := checkedMergeHook(ctx, bare, "pre-receive", update); err != nil {
		return err
	}
	if err := checkedMergeHook(ctx, bare, "update", "", ref, opts.expectedBase, newSHA); err != nil {
		return err
	}
	script := fmt.Sprintf("start\nverify refs/heads/%s %s\nupdate refs/heads/%s %s %s\nprepare\ncommit\n", opts.headBranch, opts.expectedHead, opts.pushBranch, newSHA, opts.expectedBase)
	cmd := exec.CommandContext(ctx, "git", "--git-dir", bare, "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(script)
	if _, e := cmd.CombinedOutput(); e != nil {
		return ErrMergePrecondition
	}
	// These hooks run after the ref transaction, like receive-pack. A failure
	// is an observation, never a reason to repeat or roll back a confirmed merge.
	if err := checkedMergeHook(ctx, bare, "post-receive", update); err != nil {
		slog.WarnContext(ctx, "merge post-receive hook failed")
	}
	if err := checkedMergeHook(ctx, bare, "post-update", "", ref); err != nil {
		slog.WarnContext(ctx, "merge post-update hook failed")
	}
	return nil
}

func checkedMergeHook(ctx context.Context, bare, name, input string, args ...string) error {
	hooks := filepath.Join(bare, "hooks")
	configured, configErr := exec.CommandContext(ctx, "git", "--git-dir", bare, "config", "--path", "--get", "core.hooksPath").Output()
	if configErr == nil {
		path := strings.TrimSpace(string(configured))
		if path == "" || strings.ContainsAny(path, "\x00\r\n") {
			return fmt.Errorf("invalid configured merge hook directory")
		}
		if path == os.DevNull {
			return nil
		}
		if filepath.IsAbs(path) {
			hooks = path
		} else {
			hooks = filepath.Join(bare, path)
		}
	} else {
		var status *exec.ExitError
		if !errors.As(configErr, &status) || status.ExitCode() != 1 {
			return fmt.Errorf("merge hook configuration unavailable")
		}
	}
	location := filepath.Join(hooks, name)
	info, err := os.Lstat(location)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("merge hook unavailable")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("merge hook must be a regular file")
	}
	if info.Mode().Perm()&0111 == 0 {
		return nil
	}
	command := exec.CommandContext(ctx, location, args...)
	command.Dir = bare
	command.Env = append(os.Environ(), "GIT_DIR="+bare)
	command.Stdin = strings.NewReader(input)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return fmt.Errorf("merge rejected by %s hook", name)
	}
	return nil
}
