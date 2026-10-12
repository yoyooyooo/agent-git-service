package service

import (
	"testing"

	"github.com/ngaut/agent-git-service/internal/cibackend"
)

// Same-named required checks from distinct workflow files must not permit a
// successful sibling to hide a failed or pending required check.
func TestAstraSameNameRequiredChecksCannotMaskFailure(t *testing.T) {
	for _, bad := range []struct{ label, status, conclusion string }{
		{"failure", "completed", "failure"},
		{"pending", "in_progress", ""},
	} {
		t.Run(bad.label, func(t *testing.T) {
			s, ctx, _, pr, backend, _ := ciFixture(t)
			first := backend.runs[0]
			second := first
			second.Key, second.Workflow, second.Name = "18", "app-ci.yml", "App CI"
			backend.runs = append(backend.runs, second)
			backend.jobs = []cibackend.Job{
				{Key: "41", Run: "17", Name: "unit", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "success"},
				{Key: "42", Run: "17", Name: "lint", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "success"},
				{Key: "43", Run: "18", Name: "unit", HeadSHA: pr.HeadSHA, Status: bad.status, Conclusion: bad.conclusion},
			}
			checks, err := s.ReadCIChecks(ctx, pr)
			if err != nil {
				t.Fatalf("distinct workflows should remain readable: %v", err)
			}
			if len(checks.Checks) != 3 {
				t.Fatalf("want 3 checks, got %#v", checks.Checks)
			}
			for _, check := range checks.Checks {
				if !check.Required {
					t.Fatalf("expected required check: %#v", check)
				}
			}
			if err := s.enforceConfiguredCIForMerge(ctx, pr); err == nil {
				t.Fatalf("merge gate accepted required app-ci.yml/unit=%s because ci.yml/unit succeeded", bad.label)
			}
			states, err := s.latestStatusChecksForPR(ctx, pr)
			if err != nil {
				t.Fatal(err)
			}
			if states["unit"].Passed || (bad.label == "pending" && states["unit"].Completed) {
				t.Fatalf("branch protection fold hid app-ci.yml/unit=%s: %+v", bad.label, states["unit"])
			}
			if !states["lint"].Completed || !states["lint"].Passed {
				t.Fatalf("unrelated required check lint changed: %+v", states["lint"])
			}
			if err := s.enforceRequiredStatusChecks(ctx, pr, branchProtectionRequiredStatusChecks{Contexts: []string{"unit", "lint"}}); err == nil {
				t.Fatalf("branch protection accepted required unit while app-ci.yml/unit=%s", bad.label)
			}
		})
	}
}

func TestSameNameRequiredChecksPassOnlyWhenEveryMatchPasses(t *testing.T) {
	s, ctx, _, pr, backend, _ := ciFixture(t)
	first := backend.runs[0]
	second := first
	second.Key, second.Workflow, second.Name = "18", "app-ci.yml", "App CI"
	backend.runs = append(backend.runs, second)
	backend.jobs = []cibackend.Job{
		{Key: "41", Run: "17", Name: "unit", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "success"},
		{Key: "42", Run: "17", Name: "lint", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "success"},
		{Key: "43", Run: "18", Name: "unit", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "success"},
	}
	if err := s.enforceConfiguredCIForMerge(ctx, pr); err != nil {
		t.Fatal(err)
	}
	states, err := s.latestStatusChecksForPR(ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	if !states["unit"].Completed || !states["unit"].Passed {
		t.Fatalf("both unit observations passed but fold rejected them: %+v", states["unit"])
	}
	if err := s.enforceRequiredStatusChecks(ctx, pr, branchProtectionRequiredStatusChecks{Contexts: []string{"unit", "lint"}}); err != nil {
		t.Fatal(err)
	}
}
