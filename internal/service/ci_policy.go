package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/ngaut/agent-git-service/internal/db"
)

// CIRequiredChecks combines current branch protection and explicit backend
// policy. A configured backend can add requirements, never erase branch policy.
func (s *Service) CIRequiredChecks(ctx context.Context, pr db.PullRequest) (map[string]bool, bool, error) {
	sel, e := s.CISelection(pr.Repository.FullName)
	if e != nil {
		return nil, false, e
	}
	result := map[string]bool{}
	known := sel.Name == "native" || sel.Name == "none" || sel.Binding.RequiredChecks != nil
	if sel.Binding.RequiredChecks != nil {
		for _, name := range *sel.Binding.RequiredChecks {
			result[name] = true
		}
	}
	bp, e := s.GetBranchProtection(ctx, pr.RepositoryID, pr.BaseRef)
	if errors.Is(e, ErrNotFound) {
		return result, known, nil
	}
	if e != nil {
		return nil, false, e
	}
	policy, e := decodeBranchProtectionRequiredStatusChecks(bp.RequiredStatusChecksJSON)
	if e != nil {
		return nil, false, ErrInvalidState
	}
	for _, name := range collectRequiredStatusCheckContexts(policy) {
		result[name] = true
	}
	return result, true, nil
}
func (s *Service) enforceConfiguredCIForMerge(ctx context.Context, pr db.PullRequest) error {
	sel, e := s.CISelection(pr.Repository.FullName)
	if e != nil {
		return e
	}
	if sel.Name == "native" {
		if sel.Binding.RequiredChecks == nil || len(*sel.Binding.RequiredChecks) == 0 {
			return nil
		}
		states, err := s.latestStatusChecksForPR(ctx, pr)
		if err != nil {
			return err
		}
		for _, name := range *sel.Binding.RequiredChecks {
			state, ok := states[name]
			if !ok || !state.Completed || !state.Passed {
				return fmt.Errorf("%w: current head has pending or failed required native CI checks", ErrInvalidState)
			}
		}
		return nil
	}
	required, known, e := s.CIRequiredChecks(ctx, pr)
	if e != nil {
		return e
	}
	if !known || len(required) == 0 {
		return nil
	}
	checks, e := s.ReadCIChecks(ctx, pr)
	if e != nil {
		return e
	}
	for _, check := range checks.Checks {
		if required[check.Name] && check.Status == "completed" && (check.Conclusion == "success" || check.Conclusion == "neutral" || check.Conclusion == "skipped") {
			delete(required, check.Name)
		}
	}
	if len(required) > 0 {
		return fmt.Errorf("%w: current head has pending or failed required CI checks", ErrInvalidState)
	}
	return nil
}
