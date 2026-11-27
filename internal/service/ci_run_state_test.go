package service

import (
	"errors"
	"testing"

	"github.com/ngaut/agent-git-service/internal/cibackend"
)

func TestCIChecksRunAndJobMayShareRequiredName(t *testing.T) {
	s, ctx, repo, pr, backend, config := ciFixture(t)
	selected := config.Backends["builder"]
	selected.Kind = "forgejo"
	config.Backends["builder"] = selected
	required := []string{"unit"}
	binding := config.Repositories[repo.FullName]
	binding.RequiredChecks = &required
	config.Repositories[repo.FullName] = binding
	backend.runs[0].Name = "unit"
	backend.runs[0].Status, backend.runs[0].Conclusion = "queued", ""
	registry, err := cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
	if err != nil {
		t.Fatal(err)
	}
	s.CI = registry

	observed, err := s.ReadCIChecks(ctx, pr)
	if err != nil || len(observed.Checks) != 2 {
		t.Fatalf("same-name run/job rejected or hidden: %+v %v", observed, err)
	}
	seen := map[string]bool{}
	for _, check := range observed.Checks {
		if check.Name != "unit" || !check.Required || seen[check.ID] {
			t.Fatalf("run guard renamed or aliased required evidence: %+v", observed.Checks)
		}
		seen[check.ID] = true
	}
	if err := s.enforceConfiguredCIForMerge(ctx, pr); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("pending same-name run allowed required CI: %v", err)
	}
	states, err := s.latestStatusChecksForPR(ctx, pr)
	if err != nil || states["unit"].Completed || states["unit"].Passed {
		t.Fatalf("merge state discarded pending same-name evidence: %+v %v", states, err)
	}

	backend.runs[0].Status, backend.runs[0].Conclusion = "completed", "success"
	observed, err = s.ReadCIChecks(ctx, pr)
	if err != nil || len(observed.Checks) != 1 || observed.Checks[0].Name != "unit" || !seen[observed.Checks[0].ID] || observed.Checks[0].Conclusion != "success" {
		t.Fatalf("completed workflow changed real job identity: %+v %v", observed, err)
	}
	if err := s.enforceConfiguredCIForMerge(ctx, pr); err != nil {
		t.Fatalf("completed required job remained blocked: %v", err)
	}

	backend.jobs = append(backend.jobs, cibackend.Job{Key: "42", Run: "17", Name: "unit", HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: "success"})
	if _, err := s.ReadCIChecks(ctx, pr); !errors.Is(err, cibackend.ErrInvalid) {
		t.Fatalf("duplicate real job name became valid: %v", err)
	}
}

func TestCIChecksRunGuardDoesNotInventRequiredPolicy(t *testing.T) {
	s, ctx, repo, pr, backend, config := ciFixture(t)
	binding := config.Repositories[repo.FullName]
	binding.RequiredChecks = nil
	config.Repositories[repo.FullName] = binding
	backend.runs[0].Status, backend.runs[0].Conclusion = "in_progress", ""
	registry, err := cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
	if err != nil {
		t.Fatal(err)
	}
	s.CI = registry
	observed, err := s.ReadCIChecks(ctx, pr)
	if err != nil || observed.RequiredKnown || len(observed.Checks) != 2 {
		t.Fatalf("run guard invented policy or hid progress: %+v %v", observed, err)
	}
	for _, check := range observed.Checks {
		if check.Required {
			t.Fatalf("run guard promoted an optional check: %+v", check)
		}
	}
}
