// Package snapshotstore owns verified, immutable, self-contained Git views.
// It has no AGS database or authorization. Callers must separately authenticate
// the peer, authorize the user, choose the exact manifest and coordinate capture
// with all source mutations/GC. None of those proofs follows from a valid hash.
package snapshotstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

const maxGitOutput = 64 << 20

// Git ignores ambient credentials, replace refs, remote helpers, hooks, global
// config and tracing. Source local configuration is never copied to a view.
func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	prefix := []string{"--no-replace-objects", "-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false",
		"-c", "protocol.allow=never", "-c", "pack.threads=1", "-c", "pack.windowMemory=32m"}
	cmd := exec.CommandContext(ctx, "git", append(prefix, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C", "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull, "GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1"}
	return cmd
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("Git output exceeds limit")
	}
	return b.Buffer.Write(p)
}

func runGit(ctx context.Context, dir string, input io.Reader, args ...string) ([]byte, error) {
	cmd := gitCommand(ctx, dir, args...)
	out := &boundedBuffer{limit: maxGitOutput}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, out, io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("snapshot Git %s failed: %w", args[0], err)
	}
	return out.Bytes(), nil
}

// ObserveManifest reads a QUIESCED source under a caller-owned capture/GC
// guard. It does not acquire that guard or claim that two scans prove an atomic
// multi-ref snapshot. Prefixes are an explicit primary-owned export policy;
// hidden-ref policy/permission resolution must happen before this function.
func ObserveManifest(ctx context.Context, source string, identity edgeprotocol.RepositoryIdentity, policy string, prefixes []string) (edgeprotocol.Manifest, error) {
	if len(prefixes) == 0 {
		return edgeprotocol.Manifest{}, errors.New("explicit export prefixes required")
	}
	for _, prefix := range prefixes {
		if !strings.HasPrefix(prefix, "refs/") || !strings.HasSuffix(prefix, "/") || strings.ContainsAny(prefix, "\x00\r\n") {
			return edgeprotocol.Manifest{}, errors.New("invalid export prefix")
		}
	}
	bare, err := runGit(ctx, source, nil, "rev-parse", "--is-bare-repository")
	if err != nil || strings.TrimSpace(string(bare)) != "true" {
		return edgeprotocol.Manifest{}, errors.New("source must be a bare repository")
	}
	if _, err := os.Stat(filepath.Join(source, "shallow")); !errors.Is(err, os.ErrNotExist) {
		return edgeprotocol.Manifest{}, errors.New("shallow sources are not supported")
	}
	if _, err := os.Stat(filepath.Join(source, "info", "grafts")); !errors.Is(err, os.ErrNotExist) {
		return edgeprotocol.Manifest{}, errors.New("grafted sources are not supported")
	}
	if partial, err := runGit(ctx, source, nil, "config", "--get", "extensions.partialclone"); err == nil && len(bytes.TrimSpace(partial)) != 0 {
		return edgeprotocol.Manifest{}, errors.New("partial sources are not supported")
	}
	// Prefix selection is not a hidden-ref policy evaluator. Until the live
	// primary adapter supplies that policy, reject such sources rather than
	// accidentally exporting a ref that native upload-pack would hide.
	for _, key := range []string{"uploadpack.hideRefs", "transfer.hideRefs"} {
		if _, err := runGit(ctx, source, nil, "config", "--get-all", key); err == nil {
			return edgeprotocol.Manifest{}, errors.New("source hidden-ref policy requires an explicit export adapter")
		}
	}
	format, err := runGit(ctx, source, nil, "rev-parse", "--show-object-format")
	if err != nil {
		return edgeprotocol.Manifest{}, err
	}
	data, err := runGit(ctx, source, nil, "for-each-ref", "--format=%(refname)%09%(objectname)%09%(symref)")
	if err != nil {
		return edgeprotocol.Manifest{}, err
	}
	refs := make([]edgeprotocol.Ref, 0)
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			return edgeprotocol.Manifest{}, errors.New("invalid source reference output")
		}
		included := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(parts[0], prefix) {
				included = true
				break
			}
		}
		if !included {
			continue
		}
		if parts[2] != "" {
			return edgeprotocol.Manifest{}, errors.New("symbolic export refs other than HEAD are unsupported")
		}
		refs = append(refs, edgeprotocol.Ref{Name: parts[0], OID: parts[1]})
	}
	var head edgeprotocol.Head
	if symbolic, err := runGit(ctx, source, nil, "symbolic-ref", "-q", "HEAD"); err == nil {
		head.SymbolicRef = strings.TrimSpace(string(symbolic))
	}
	if oid, err := runGit(ctx, source, nil, "rev-parse", "--verify", "HEAD"); err == nil {
		head.OID = strings.TrimSpace(string(oid))
	} else if head.SymbolicRef != "" {
		head.Unborn = true
	} else {
		return edgeprotocol.Manifest{}, errors.New("source has invalid HEAD")
	}
	return edgeprotocol.NewManifest(identity, strings.TrimSpace(string(format)), head, policy, refs)
}

// WritePack materializes ONLY roots in the supplied manifest. Keep the source
// capture/GC guard until this returns; the resulting exported view will own its
// objects independently and will not hold a source lock across a WAN transfer.
// Native pack-objects produces a non-thin pack; no hooks/config/reflogs or extra
// source refs are copied. Empty/unborn repositories still produce a valid pack.
func WritePack(ctx context.Context, source string, manifest edgeprotocol.Manifest, dst io.Writer) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	input := strings.Join(manifest.Roots(), "\n")
	if input != "" {
		input += "\n"
	}
	cmd := gitCommand(ctx, source, "pack-objects", "--stdout", "--revs", "--no-reuse-delta", "--no-sparse", "--delta-base-offset", "--threads=1", "--window-memory=32m", "-q")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(input), dst, io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("export exact snapshot objects: %w", err)
	}
	return nil
}

func initRepo(ctx context.Context, root string, manifest edgeprotocol.Manifest) (string, error) {
	repo := filepath.Join(root, "view", "repo.git")
	if err := os.MkdirAll(filepath.Dir(repo), 0700); err != nil {
		return "", err
	}
	if _, err := runGit(ctx, root, nil, "init", "--bare", "--template=", "--object-format="+manifest.Snapshot.ObjectFormat, repo); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(repo, "config"), []byte(snapshotConfig(manifest)), 0600); err != nil {
		return "", err
	}
	return repo, nil
}

func snapshotConfig(manifest edgeprotocol.Manifest) string {
	// Fixed owner-only config, not inherited from the primary or a template.
	cfg := "[core]\n\trepositoryformatversion = 0\n\tbare = true\n\tlogallrefupdates = false\n\thooksPath = " + os.DevNull + "\n[http]\n\treceivepack = false\n[uploadpack]\n\tallowAnySHA1InWant = false\n\tallowTipSHA1InWant = false\n\tallowReachableSHA1InWant = false\n\tallowFilter = false\n[gc]\n\tauto = 0\n[maintenance]\n\tauto = false\n"
	if manifest.Snapshot.ObjectFormat == "sha256" {
		cfg = strings.Replace(cfg, "repositoryformatversion = 0", "repositoryformatversion = 1", 1) + "[extensions]\n\tobjectFormat = sha256\n"
	}
	return cfg
}

func installRefs(ctx context.Context, repo string, m edgeprotocol.Manifest) error {
	var commands strings.Builder
	commands.WriteString("start\n")
	for _, ref := range m.Refs {
		fmt.Fprintf(&commands, "create %s %s\n", ref.Name, ref.OID)
	}
	commands.WriteString("prepare\ncommit\n")
	if _, err := runGit(ctx, repo, strings.NewReader(commands.String()), "update-ref", "--stdin"); err != nil {
		return err
	}
	head := m.Snapshot.HEAD.OID + "\n"
	if m.Snapshot.HEAD.SymbolicRef != "" {
		head = "ref: " + m.Snapshot.HEAD.SymbolicRef + "\n"
	}
	return os.WriteFile(filepath.Join(repo, "HEAD"), []byte(head), 0600)
}

func verifyRepo(ctx context.Context, repo string, m edgeprotocol.Manifest) error {
	// Neither borrowed objects, shallow boundaries nor local symlinks are valid
	// in a published self-contained view. A modified cache fails on reopen.
	if err := filepath.WalkDir(repo, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in snapshot")
		}
		return nil
	}); err != nil {
		return err
	}
	for _, name := range []string{"objects/info/alternates", "objects/info/http-alternates", "info/grafts", "shallow"} {
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
			return errors.New("snapshot depends on external or shallow data")
		}
	}
	config, err := os.ReadFile(filepath.Join(repo, "config"))
	if err != nil {
		return err
	}
	if string(config) != snapshotConfig(m) {
		return errors.New("snapshot configuration changed")
	}
	observed, err := ObserveManifest(ctx, repo, m.Snapshot.Identity, m.Snapshot.ExportPolicyRevision, []string{"refs/"})
	if err != nil {
		return err
	}
	if observed.Snapshot != m.Snapshot {
		return errors.New("snapshot refs or HEAD mismatch")
	}
	if _, err := runGit(ctx, repo, nil, "fsck", "--strict", "--full", "--no-reflogs", "--no-dangling"); err != nil {
		return err
	}
	// Reject extra hidden/unreachable objects in a supplied pack, not merely
	// missing objects. Borrowing a superset object database is NOT supported.
	roots := strings.Join(m.Roots(), "\n")
	if roots != "" {
		roots += "\n"
	}
	reachable, err := runGit(ctx, repo, strings.NewReader(roots), "rev-list", "--objects", "--no-object-names", "--stdin")
	if err != nil {
		return err
	}
	all, err := runGit(ctx, repo, nil, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
	if err != nil {
		return err
	}
	want, got := strings.Fields(string(reachable)), strings.Fields(string(all))
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(want, "\n") != strings.Join(got, "\n") {
		return errors.New("snapshot contains objects outside exported reachability")
	}
	return nil
}
