package artifactretirement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetirementPreservesAvailableDatabaseOnlyCommit(t *testing.T) {
	ctx := context.Background()
	bare, head, historical, blobOID, blob := testFixture(t)
	tree := testGit(t, bare, nil, "rev-parse", head+"^{tree}")
	raw := "tree " + tree + "\nparent " + head + "\nauthor Fixture <fixture@example.test> 1700000000 +0000\ncommitter Fixture <fixture@example.test> 1700000000 +0000\n\ndatabase-only audit commit\n"
	orphan := testGit(t, bare, []byte(raw), "hash-object", "-w", "-t", "commit", "--stdin")
	initialRefs := testGit(t, bare, nil, "show-ref")
	sum := sha256.Sum256(blob)
	intent := Intent{Schema: IntentSchema, OperationID: "orphan-preservation", Repository: "owner/repo", DefaultBranch: "main", ExpectedAncestor: historical, RecoveryArchive: "off-host://fixture/recovery", RecoveryArchiveSHA256: strings.Repeat("a", 64), AllowSignatureRemoval: true, RetiredBlobs: []BlobSpec{{OID: blobOID, Bytes: int64(len(blob)), SHA256: hex.EncodeToString(sum[:])}}}
	plan, err := PrepareWithApplicationRoots(ctx, bare, filepath.Join(t.TempDir(), "state"), intent, []string{orphan})
	if err != nil {
		t.Fatal(err)
	}
	if testGit(t, bare, nil, "show-ref") != initialRefs {
		t.Fatal("preparation mutated source references")
	}
	mapped := plan.CommitMap[orphan]
	if mapped == "" || mapped == orphan || plan.CleanRefs["refs/ags/retention/"+mapped] != mapped {
		t.Fatalf("database-only commit not preserved: %s", mapped)
	}
	if _, ok := plan.OriginalRefs["refs/ags/retention/"+orphan]; ok {
		t.Fatal("synthetic staging ref was mistaken for an original source ref")
	}
	if err := Publish(ctx, bare, plan); err != nil {
		t.Fatal(err)
	}
	if testGit(t, bare, nil, "show", "-s", "--format=%s", orphan) != "database-only audit commit" {
		t.Fatal("historical audit object content lost")
	}
	if err := VerifyRetiredAbsent(ctx, bare, intent.RetiredBlobs); err != nil {
		t.Fatal(err)
	}
}
