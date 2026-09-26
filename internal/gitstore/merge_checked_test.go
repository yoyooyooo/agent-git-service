package gitstore

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func checkedFixture(t *testing.T) (*Store, string, string, string, string) {
	t.Helper()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	repo := "fixture/checked"
	if e = s.Init(ctx, repo, "main", true); e != nil {
		t.Fatal(e)
	}
	base, e := s.HeadSHA(ctx, repo, "main")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.CreateBranch(ctx, repo, "feature", "main"); e != nil {
		t.Fatal(e)
	}
	head, e := s.WriteFile(ctx, repo, "feature", "feature.txt", "feature", []byte("feature\n"))
	if e != nil {
		t.Fatal(e)
	}
	directory, e := s.GetRepoPath(ctx, repo)
	if e != nil {
		t.Fatal(e)
	}
	return s, repo, directory, base, head
}
func TestCheckedMergePublishesOnlyTheObservedHeadAndBase(t *testing.T) {
	s, repo, _, base, head := checkedFixture(t)
	ctx := context.Background()
	result, e := s.Merge(ctx, MergeOptions{FullName: repo, BaseBranch: "main", HeadBranch: "feature", ExpectedHeadSHA: head, ExpectedBaseSHA: base, Committer: "Fixture", Email: "fixture@example.test", MergeMessage: "checked fixture merge"})
	if e != nil {
		t.Fatal(e)
	}
	actual, e := s.HeadSHA(ctx, repo, "main")
	if e != nil || actual != result || result == base {
		t.Fatal("merge not published", e)
	}
	if next, e := s.HeadSHA(ctx, repo, "feature"); e != nil || next != head {
		t.Fatal("source branch changed", e)
	}
	if _, e = s.Merge(ctx, MergeOptions{FullName: repo, BaseBranch: "main", HeadBranch: "feature", ExpectedHeadSHA: head, ExpectedBaseSHA: base}); !errors.Is(e, ErrMergePrecondition) {
		t.Fatal("stale base allowed", e)
	}
}
func TestCheckedMergeRejectsAConcurrentGitHeadChangeWithoutRollingItBack(t *testing.T) {
	s, repo, bare, base, head := checkedFixture(t)
	ctx := context.Background()
	if e := s.CreateBranch(ctx, repo, "next", "feature"); e != nil {
		t.Fatal(e)
	}
	next, e := s.WriteFile(ctx, repo, "next", "next.txt", "next", []byte("next\n"))
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.withTempClone(ctx, repo, tempCloneOptions{opName: "checked-test", prefix: "ags-checked-test-*", committer: "Fixture", email: "fixture@example.test", pushBranch: "main", headBranch: "feature", expectedHead: head, expectedBase: base}, func(clone string) error {
		for _, args := range [][]string{{"checkout", "-B", "main", "origin/main"}, {"merge", "--no-ff", "--no-edit", "origin/feature"}} {
			command := append([]string{"-C", clone}, args...)
			if out, e := exec.CommandContext(ctx, "git", command...).CombinedOutput(); e != nil {
				t.Fatalf("fixture merge %v: %s", e, out)
			}
		}
		// Simulate another Git process that is not coordinated by this Store mutex.
		if out, e := exec.CommandContext(ctx, "git", "--git-dir", bare, "update-ref", "refs/heads/feature", next, head).CombinedOutput(); e != nil {
			t.Fatalf("concurrent update %v: %s", e, out)
		}
		return nil
	})
	if !errors.Is(e, ErrMergePrecondition) {
		t.Fatal("head verification was only a preflight", e)
	}
	if got, e := s.HeadSHA(ctx, repo, "main"); e != nil || got != base {
		t.Fatal("failed transaction changed base", e)
	}
	if got, e := s.HeadSHA(ctx, repo, "feature"); e != nil || got != next {
		t.Fatal("failed merge rolled back another writer", e)
	}
}
