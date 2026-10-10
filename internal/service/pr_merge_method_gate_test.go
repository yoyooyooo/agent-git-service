package service_test

import (
	"errors"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestStandardMergeRejectsDisabledMethodOnRESTAndGraphQL(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	pr, ctx, _ := setupPRWithRealBranches(t, svc, "gate-user", "gate-repo")
	full := pr.Repository.FullName
	if full == "" {
		full = "gate-user/gate-repo"
	}
	if err := svc.DB.Model(&db.Repository{}).Where("full_name = ?", full).Updates(map[string]any{
		"allow_squash_merge": false,
		"allow_merge_commit": true,
		"allow_rebase_merge": true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	before, err := svc.Git.HeadSHA(ctx, full, "main")
	if err != nil {
		t.Fatal(err)
	}

	_, restErr := svc.MergeStandardPR(ctx, full, pr.Number, "squash", "", pr.HeadSHA)
	if !errors.Is(restErr, service.ErrConflict) {
		t.Fatalf("REST squash: %v", restErr)
	}
	_, gqlErr := svc.MergeStandardPRByID(ctx, pr.ID, "squash", "", pr.HeadSHA)
	if !errors.Is(gqlErr, service.ErrConflict) {
		t.Fatalf("GraphQL squash: %v", gqlErr)
	}
	after, err := svc.Git.HeadSHA(ctx, full, "main")
	if err != nil || after != before {
		t.Fatalf("disabled method moved base: before=%s after=%s err=%v", before, after, err)
	}

	merged, err := svc.MergeStandardPR(ctx, full, pr.Number, "merge", "", pr.HeadSHA)
	if err != nil || !merged.Merged || merged.MergeCommitSHA == "" {
		t.Fatalf("allowed merge did not land: merged=%v sha=%s err=%v", merged.Merged, merged.MergeCommitSHA, err)
	}
}
