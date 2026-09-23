package gitstore

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// Init creates a new bare git repository, optionally seeding a README commit.
func (s *Store) Init(ctx context.Context, fullName, defaultBranch string, seed bool) error {
	ctx, release, err := s.BeginMutation(ctx)
	if err != nil {
		return err
	}
	defer release()
	dir, err := s.repoPath(ctx, fullName)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err == nil {
		return s.EnsureReceivePolicy(ctx, fullName)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("gitstore: mkdir %s: %w", dir, err)
	}

	stg := filesystem.NewStorage(osfs.New(dir), nil)

	repo, err := git.InitWithOptions(stg, nil, git.InitOptions{
		DefaultBranch: plumbing.NewBranchReferenceName(defaultBranch),
	})
	if err != nil {
		return fmt.Errorf("gitstore: git init: %w", err)
	}

	// Install a pre-receive hook that protects the default branch from
	// deletion and non-fast-forward rewrites while preserving rebase flows on
	// ordinary work branches. Non-standard ref namespaces also require CAS.
	if err := installNonFFRejectHook(dir); err != nil {
		return fmt.Errorf("gitstore: install pre-receive hook: %w", err)
	}

	if seed {
		if err := seedReadme(ctx, repo, defaultBranch); err != nil {
			return fmt.Errorf("gitstore: seed: %w", err)
		}
	}
	return nil
}

// Fork creates a copy of the repository by duplicating its directory structure.
func (s *Store) Fork(ctx context.Context, srcFullName, targetFullName string) error {
	ctx, release, err := s.BeginMutation(ctx)
	if err != nil {
		return err
	}
	defer release()
	srcPath, err := s.repoPath(ctx, srcFullName)
	if err != nil {
		return err
	}
	targetPath, err := s.repoPath(ctx, targetFullName)
	if err != nil {
		return err
	}

	if _, err := os.Stat(srcPath); err != nil {
		return fmt.Errorf("source repo does not exist: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		return err
	}

	// Remove targetPath if it exists (e.g. from CreateRepo's InitBare)
	_ = os.RemoveAll(targetPath)

	cmd := exec.CommandContext(ctx, "cp", "-a", srcPath, targetPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gitstore cp: %w (output: %s)", err, out)
	}

	// cp -a preserves the source's hooks/ directory, but re-install the
	// pre-receive hook defensively so a forked repo gets the current
	// policy even if the source was created before this behaviour landed.
	if err := installNonFFRejectHook(targetPath); err != nil {
		return fmt.Errorf("gitstore fork: %w", err)
	}

	return nil
}

// EnsureReceivePolicy refreshes the server-owned pre-receive dispatcher and
// authority guard for an existing repository. Git HTTP calls this before
// serving so upgraded repositories receive both delegated-session and durable
// default-branch invariants without replacing repo-local policies.
func (s *Store) EnsureReceivePolicy(ctx context.Context, fullName string) error {
	ctx, release, err := s.BeginMutation(ctx)
	if err != nil {
		return err
	}
	defer release()
	dir, err := s.repoPath(ctx, fullName)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("gitstore: repository path: %w", err)
	}
	if err := installNonFFRejectHook(dir); err != nil {
		return fmt.Errorf("gitstore: install receive policy hook: %w", err)
	}
	return nil
}

// seedReadme creates an initial commit with README.md on defaultBranch.
func seedReadme(ctx context.Context, repo *git.Repository, defaultBranch string) error {
	stg := repo.Storer
	readmeContent := ""

	// Create blob.
	blobEnc := stg.NewEncodedObject()
	blobEnc.SetType(plumbing.BlobObject)
	w, err := blobEnc.Writer()
	if err != nil {
		return err
	}
	if _, err := fmt.Fprint(w, readmeContent); err != nil {
		_ = w.Close()
		return err
	}
	_ = w.Close()
	blobHash, err := stg.SetEncodedObject(blobEnc)
	if err != nil {
		return err
	}

	// Create tree.
	treeEnc := stg.NewEncodedObject()
	treeEnc.SetType(plumbing.TreeObject)
	tree := object.Tree{
		Entries: []object.TreeEntry{
			{Name: "README.md", Mode: filemode.Regular, Hash: blobHash},
		},
	}
	if err := tree.Encode(treeEnc); err != nil {
		return err
	}
	treeHash, err := stg.SetEncodedObject(treeEnc)
	if err != nil {
		return err
	}

	// Create commit.
	now := time.Now()
	sig := &object.Signature{Name: defaultCommitName, Email: defaultCommitEmail, When: now}
	commitEnc := stg.NewEncodedObject()
	commitEnc.SetType(plumbing.CommitObject)
	commit := object.Commit{
		Author:       *sig,
		Committer:    *sig,
		Message:      "Initial commit\n",
		TreeHash:     treeHash,
		ParentHashes: nil,
	}
	if err := commit.Encode(commitEnc); err != nil {
		return err
	}
	commitHash, err := stg.SetEncodedObject(commitEnc)
	if err != nil {
		return err
	}

	// Point HEAD → branch → commit.
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(defaultBranch))
	if err := stg.SetReference(headRef); err != nil {
		return err
	}
	return stg.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(defaultBranch), commitHash))
}

const (
	preReceiveDispatcherMarker = "# gh-server: managed pre-receive dispatcher"
	legacyAuthorityHookMarker  = "# gh-server: preserve the repository authority line"
	preservedLocalHookName     = "10-local-preserved"
	managedAuthorityHookName   = "50-gh-server-authority"
)

// installNonFFRejectHook installs the AGS authority guard behind a dispatcher.
// An existing repo-local pre-receive policy is migrated into pre-receive.d and
// retained across later refreshes; AGS never silently replaces a stronger
// repository-specific policy.
func installNonFFRejectHook(bareRepoDir string) error {
	hooksDir := filepath.Join(bareRepoDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o750); err != nil {
		return fmt.Errorf("mkdir hooks: %w", err)
	}
	partsDir := filepath.Join(hooksDir, "pre-receive.d")
	if err := os.MkdirAll(partsDir, 0o750); err != nil {
		return fmt.Errorf("mkdir pre-receive.d: %w", err)
	}

	path := filepath.Join(hooksDir, "pre-receive")
	if existing, err := os.ReadFile(path); err == nil {
		body := string(existing)
		isManaged := strings.Contains(body, preReceiveDispatcherMarker) || strings.Contains(body, legacyAuthorityHookMarker)
		if !isManaged {
			preservedPath := filepath.Join(partsDir, preservedLocalHookName)
			if preserved, readErr := os.ReadFile(preservedPath); readErr == nil {
				if !bytes.Equal(preserved, existing) {
					return fmt.Errorf("preserve existing pre-receive hook: %s already contains a different policy", preservedPath)
				}
			} else if !os.IsNotExist(readErr) {
				return fmt.Errorf("read preserved pre-receive hook: %w", readErr)
			} else if err := os.WriteFile(preservedPath, existing, 0o755); err != nil {
				return fmt.Errorf("preserve existing pre-receive hook: %w", err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read pre-receive hook: %w", err)
	}

	authorityScript := fmt.Sprintf(`#!/bin/sh
# gh-server: preserve the repository authority line and CAS ref namespaces.
default_ref=$(git symbolic-ref HEAD 2>/dev/null || true)
zero="%s"
exit_code=0
while read -r oldrev newrev refname; do
    if [ "${AGS_GIT_HTTP_RECEIVE_PACK:-}" = "1" ]; then
        case ":${AGS_GIT_HTTP_PROTECTED_REFS:-}:" in
            *":$refname:"*)
                echo "error: direct Git HTTP push to protected branch $refname rejected" >&2
                echo "hint: merge through the authoritative pull-request workflow." >&2
                exit_code=1
                continue
                ;;
        esac
    fi

    if [ "${AGS_DELEGATED_SESSION:-}" = "1" ]; then
        case "$refname" in
            refs/heads/*)
                ;;
            *)
                echo "error: delegated session may update branch refs only: $refname" >&2
                exit_code=1
                continue
                ;;
        esac
        if [ "$newrev" = "$zero" ]; then
            echo "error: delegated session may not delete branch $refname" >&2
            exit_code=1
            continue
        fi
        case ":${AGS_DELEGATED_PROTECTED_REFS:-}:" in
            *":$refname:"*)
                echo "error: delegated session may not push directly to protected branch $refname" >&2
                exit_code=1
                continue
                ;;
        esac
        if [ "$oldrev" != "$zero" ] && ! git merge-base --is-ancestor "$oldrev" "$newrev"; then
            echo "error: delegated session non-fast-forward push to $refname rejected" >&2
            exit_code=1
        fi
    fi

    # Synthetic Wiki has its own catalog/repair transaction, not the PR base
    # authority. Only the primary's verified Wiki HTTP adapter sets this marker;
    # delegated requests and ordinary repositories never receive the exception.
    if [ "${AGS_SYNTHETIC_WIKI_WRITE:-}" = "1" ] && [ "${AGS_GIT_HTTP_RECEIVE_PACK:-}" = "1" ] && [ "${AGS_DELEGATED_SESSION:-}" != "1" ] && [ "$refname" = "refs/heads/master" ]; then
        continue
    fi
    if [ -n "$default_ref" ] && [ "$refname" = "$default_ref" ]; then
        if [ "$newrev" = "$zero" ]; then
            echo "error: deletion of default branch $refname rejected" >&2
            exit_code=1
            continue
        fi
        if [ "$oldrev" != "$zero" ] && ! git merge-base --is-ancestor "$oldrev" "$newrev"; then
            echo "error: non-fast-forward push to default branch $refname rejected" >&2
            echo "hint: merge through the authoritative pull-request workflow instead of rewriting the base history." >&2
            exit_code=1
        fi
        continue
    fi
    case "$refname" in
        refs/heads/*|refs/tags/*)
            continue
            ;;
    esac
    # Custom ref creation and deletion remain allowed. Updates require CAS.
    if [ "$oldrev" = "$zero" ] || [ "$newrev" = "$zero" ]; then
        continue
    fi
    if ! git merge-base --is-ancestor "$oldrev" "$newrev"; then
        echo "error: non-fast-forward push to $refname rejected" >&2
        echo "hint: custom ref namespaces require a fast-forward update;" >&2
        echo "hint: use --force-with-lease=$refname:<current-sha> to retry intentionally." >&2
        exit_code=1
    fi
done
exit "$exit_code"
`, ZeroSHA)
	if err := writeExecutableAtomically(filepath.Join(partsDir, managedAuthorityHookName), []byte(authorityScript)); err != nil {
		return fmt.Errorf("write managed authority hook: %w", err)
	}

	dispatcher := `#!/bin/sh
# gh-server: managed pre-receive dispatcher. Repo-local policies live in pre-receive.d.
hooks_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)/pre-receive.d
updates=$(cat)
exit_code=0
for hook in "$hooks_dir"/*; do
    [ -f "$hook" ] && [ -x "$hook" ] || continue
    if ! printf '%s\n' "$updates" | "$hook"; then
        exit_code=1
    fi
done
exit "$exit_code"
`
	if err := writeExecutableAtomically(path, []byte(dispatcher)); err != nil {
		return fmt.Errorf("write pre-receive dispatcher: %w", err)
	}
	return nil
}

func writeExecutableAtomically(path string, body []byte) error {
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, body) {
		return os.Chmod(path, 0o755)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pre-receive-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// Delete removes the on-disk git repository.
func (s *Store) Delete(ctx context.Context, fullName string) error {
	ctx, release, err := s.BeginMutation(ctx)
	if err != nil {
		return err
	}
	defer release()
	path, err := s.repoPath(ctx, fullName)
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}
