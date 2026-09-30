package artifactretirement

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const ReceiptSchema = "ags.artifact-retirement.receipt.v1"

type Receipt struct {
	Schema             string    `json:"schema"`
	OperationID        string    `json:"operation_id"`
	IntentSHA256       string    `json:"intent_sha256"`
	Repository         string    `json:"repository"`
	StartedAt          time.Time `json:"started_at"`
	FinishedAt         time.Time `json:"finished_at"`
	Status             string    `json:"status"`
	Phase              string    `json:"phase"`
	OriginalHead       string    `json:"original_head"`
	CleanHead          string    `json:"clean_head"`
	ChangedRefs        int       `json:"changed_refs"`
	ChangedCommits     int       `json:"changed_commits"`
	SignatureRemovals  int       `json:"signature_removals"`
	RetiredBlobs       []string  `json:"retired_blobs"`
	BeforeDiskKiB      int64     `json:"before_disk_kib"`
	AfterDiskKiB       int64     `json:"after_disk_kib"`
	RecoveryArchive    string    `json:"recovery_archive"`
	RecoveryArchiveSHA string    `json:"recovery_archive_sha256"`
}

type BranchUpdate struct {
	Branch string `json:"branch"`
	Old    string `json:"old"`
	New    string `json:"new"`
}

func BranchUpdates(plan Plan) []BranchUpdate {
	var result []BranchUpdate
	for _, update := range plan.RefUpdates {
		if !strings.HasPrefix(update.Ref, "refs/heads/") {
			continue
		}
		result = append(result, BranchUpdate{
			Branch: strings.TrimPrefix(update.Ref, "refs/heads/"),
			Old:    update.Old,
			New:    update.New,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Branch < result[j].Branch })
	return result
}

// Checkpoints carry service-owned orchestration schemas but share the same
// private-file, bounded JSON and durable-write rules as Git plans.
func SaveCheckpoint(path string, value any) error  { return writePrivateJSON(path, value) }
func LoadCheckpoint(path string, target any) error { return readPrivateJSON(path, target) }

func SavePlan(path string, plan Plan) error {
	return writePrivateJSON(path, plan)
}

func LoadPlan(path string) (Plan, error) {
	var plan Plan
	if err := readPrivateJSON(path, &plan); err != nil {
		return plan, err
	}
	if !safeID(plan.OperationID) || !fullDigest(plan.IntentSHA256) || !safeRepo(plan.Repository) || !safeBranch(plan.DefaultBranch) ||
		!fullOID(plan.OriginalHead) || !fullOID(plan.CleanHead) || !fullOID(plan.DefaultTree) ||
		len(plan.CommitMap) == 0 || len(plan.OriginalRefs) == 0 || len(plan.CleanRefs) == 0 ||
		len(plan.RetiredBlobs) == 0 || strings.TrimSpace(plan.StagingGitDir) == "" {
		return plan, errors.New("invalid artifact retirement plan")
	}
	return plan, nil
}

func SaveReceipt(path string, receipt Receipt) error {
	return writePrivateJSON(path, receipt)
}

func LoadReceipt(path string) (Receipt, error) {
	var receipt Receipt
	if err := readPrivateJSON(path, &receipt); err != nil {
		return receipt, err
	}
	if receipt.Schema != ReceiptSchema || !safeID(receipt.OperationID) || !fullDigest(receipt.IntentSHA256) || !safeRepo(receipt.Repository) ||
		receipt.StartedAt.IsZero() || receipt.FinishedAt.Before(receipt.StartedAt) || receipt.Status != "completed" {
		return receipt, errors.New("invalid artifact retirement receipt")
	}
	return receipt, nil
}

func readPrivateJSON(path string, target any) error {
	if _, err := validatePrivateStateFile(path, 64*1024*1024); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return errors.New("invalid artifact retirement state JSON")
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("invalid trailing artifact retirement state")
	}
	return nil
}

func writePrivateJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > 64*1024*1024 {
		return errors.New("artifact retirement state exceeds budget")
	}
	dir := filepath.Dir(path)
	if err := EnsurePrivateStateDirectory(dir); err != nil {
		return err
	}
	if st, err := os.Lstat(path); err == nil {
		if !st.Mode().IsRegular() || !ownedStateFile(st) || st.Mode().Perm()&0077 != 0 {
			return errors.New("unsafe retirement state destination")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".artifact-retirement-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncStateDirectory(dir)
}

func syncStateDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func CurrentHead(ctx context.Context, gitDir, branch string) (string, error) {
	if !safeBranch(branch) {
		return "", errors.New("invalid artifact retirement branch")
	}
	out, err := (gitRunner{dir: gitDir}).run(ctx, nil, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", err
	}
	oid := strings.TrimSpace(string(out))
	if !fullOID(oid) {
		return "", errors.New("invalid artifact retirement branch head")
	}
	return oid, nil
}

func VerifyRetiredAbsent(ctx context.Context, gitDir string, blobs []BlobSpec) error {
	for _, spec := range blobs {
		exists, err := objectPhysicallyExists(ctx, gitDir, spec.OID)
		if err != nil {
			return fmt.Errorf("unable to verify retired object absence: %w", err)
		}
		if exists {
			return fmt.Errorf("retired blob %s remains in the active repository", spec.OID)
		}
	}
	return nil
}

func VerifyCleanRefs(ctx context.Context, activeGitDir string, expected map[string]string) error {
	actual, err := refMap(ctx, gitRunner{dir: activeGitDir}, false)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return errors.New("artifact retirement cleaned refs do not match the plan")
	}
	for ref, want := range expected {
		if actual[ref] != want {
			return fmt.Errorf("artifact retirement cleaned ref drift: %s", ref)
		}
	}
	return nil
}

func FinalizePublished(ctx context.Context, activeGitDir string, plan Plan) error {
	active := gitRunner{dir: activeGitDir}
	if err := cleanupImportRefs(ctx, active, "refs/retirement-import/"+plan.OperationID+"/"); err != nil {
		return err
	}
	if err := VerifyCleanRefs(ctx, activeGitDir, plan.CleanRefs); err != nil {
		return err
	}
	for old, newOID := range plan.CommitMap {
		if old == newOID {
			continue
		}
		out, err := active.run(ctx, nil, "rev-parse", "--verify", "refs/replace/"+old)
		if err != nil || strings.TrimSpace(string(out)) != newOID {
			return errors.New("artifact retirement replacement map is incomplete")
		}
	}
	if _, err := active.run(ctx, nil, "reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all"); err != nil {
		return err
	}
	if _, err := active.run(ctx, nil, "gc", "--prune=now"); err != nil {
		return err
	}
	if err := VerifyRetiredAbsent(ctx, activeGitDir, plan.RetiredBlobs); err != nil {
		return err
	}
	if _, err := active.run(ctx, nil, "fsck", "--full", "--no-dangling"); err != nil {
		return err
	}
	return InstallRetiredBlobPolicy(activeGitDir, plan.RetiredBlobs)
}

func VerifyCurrentRefs(ctx context.Context, activeGitDir string, expected map[string]string) error {
	actual, err := refMap(ctx, gitRunner{dir: activeGitDir}, false)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return errors.New("artifact retirement repository refs drifted after planning")
	}
	for ref, want := range expected {
		if actual[ref] != want {
			return fmt.Errorf("artifact retirement ref drift: %s", ref)
		}
	}
	return nil
}

func cleanupImportRefs(ctx context.Context, active gitRunner, prefix string) error {
	importRefs, err := active.run(ctx, nil, "for-each-ref", "--format=%(refname)%00%(objectname)", prefix)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(importRefs)) == "" {
		return nil
	}
	var cleanup strings.Builder
	cleanup.WriteString("start\n")
	for _, line := range strings.Split(strings.TrimSpace(string(importRefs)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x00")
		if len(parts) != 2 || !strings.HasPrefix(parts[0], prefix) {
			return errors.New("invalid artifact retirement import ref")
		}
		fmt.Fprintf(&cleanup, "delete %s %s\n", parts[0], parts[1])
	}
	cleanup.WriteString("prepare\ncommit\n")
	if _, err := active.run(ctx, []byte(cleanup.String()), "update-ref", "--stdin"); err != nil {
		return errors.New("artifact retirement import ref cleanup failed")
	}
	return nil
}

func refMap(ctx context.Context, g gitRunner, includeReplace bool) (map[string]string, error) {
	out, err := g.run(ctx, nil, "for-each-ref", "--format=%(refname)%00%(objectname)")
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x00")
		if len(parts) != 2 {
			return nil, errors.New("invalid ref inventory")
		}
		if strings.HasPrefix(parts[0], "refs/retirement-import/") {
			continue
		}
		if !includeReplace && strings.HasPrefix(parts[0], "refs/replace/") {
			continue
		}
		result[parts[0]] = parts[1]
	}
	return result, nil
}

// Publish imports only the already-verified cleaned graph into the live bare
// repository, atomically CAS-updates every original ref, installs compatibility
// replace refs for changed commits, then removes old unreachable object bytes.
// Callers must stop all service listeners/workers before invoking this function.
func Publish(ctx context.Context, activeGitDir string, plan Plan) error {
	if err := VerifyCurrentRefs(ctx, activeGitDir, plan.OriginalRefs); err != nil {
		return err
	}
	active := gitRunner{dir: activeGitDir}
	stage := gitRunner{dir: plan.StagingGitDir}
	cleanRefs, err := refMap(ctx, stage, true)
	if err != nil {
		return err
	}
	for ref, want := range plan.CleanRefs {
		if cleanRefs[ref] != want {
			return errors.New("artifact retirement staging refs drifted")
		}
	}
	for old, mapped := range plan.CommitMap {
		if old != mapped && cleanRefs["refs/replace/"+old] != mapped {
			return errors.New("artifact retirement staging replacement map drifted")
		}
	}
	if err := VerifyRetiredAbsent(ctx, plan.StagingGitDir, plan.RetiredBlobs); err != nil {
		return err
	}
	importPrefix := "refs/retirement-import/" + plan.OperationID + "/"
	refspec := "+refs/*:" + importPrefix + "*"
	if _, err := runGit(ctx, nil, "-c", "protocol.file.allow=always", "--git-dir="+activeGitDir,
		"-c", "gc.auto=0", "-c", "maintenance.auto=false", "fetch", "--no-tags", plan.StagingGitDir, refspec); err != nil {
		return errors.New("artifact retirement object import failed")
	}

	var tx strings.Builder
	tx.WriteString("start\n")
	refSet := map[string]bool{}
	for ref := range plan.OriginalRefs {
		refSet[ref] = true
	}
	for ref := range plan.CleanRefs {
		refSet[ref] = true
	}
	refs := make([]string, 0, len(refSet))
	for ref := range refSet {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		old, hadOld := plan.OriginalRefs[ref]
		newOID, hasNew := plan.CleanRefs[ref]
		switch {
		case hadOld && hasNew && old == newOID:
			fmt.Fprintf(&tx, "verify %s %s\n", ref, old)
		case hadOld && hasNew:
			fmt.Fprintf(&tx, "update %s %s %s\n", ref, newOID, old)
		case hadOld && !hasNew:
			fmt.Fprintf(&tx, "delete %s %s\n", ref, old)
		case !hadOld && hasNew:
			fmt.Fprintf(&tx, "create %s %s\n", ref, newOID)
		default:
			return errors.New("artifact retirement invalid ref transition")
		}
	}
	changedCommits := make([]string, 0)
	for old, newOID := range plan.CommitMap {
		if old != newOID {
			changedCommits = append(changedCommits, old)
		}
	}
	sort.Strings(changedCommits)
	for _, old := range changedCommits {
		fmt.Fprintf(&tx, "create refs/replace/%s %s\n", old, plan.CommitMap[old])
	}
	tx.WriteString("prepare\ncommit\n")
	if _, err := active.run(ctx, []byte(tx.String()), "update-ref", "--stdin"); err != nil {
		return errors.New("artifact retirement ref publication failed")
	}

	if err := cleanupImportRefs(ctx, active, importPrefix); err != nil {
		return err
	}
	if err := FinalizePublished(ctx, activeGitDir, plan); err != nil {
		return err
	}
	head, err := active.run(ctx, nil, "rev-parse", "--verify", "refs/heads/"+plan.DefaultBranch+"^{commit}")
	if err != nil || strings.TrimSpace(string(head)) != plan.CleanHead {
		return errors.New("artifact retirement default branch publication did not converge")
	}
	tree, err := active.run(ctx, nil, "rev-parse", plan.CleanHead+"^{tree}")
	if err != nil || strings.TrimSpace(string(tree)) != plan.DefaultTree {
		return errors.New("artifact retirement changed the current default tree")
	}
	if _, err := active.run(ctx, nil, "fsck", "--full", "--no-dangling"); err != nil {
		return err
	}
	return nil
}
