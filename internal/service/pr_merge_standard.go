package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
)

type mergePreconditionKey struct{}
type mergePrecondition struct{ Head, Base string }

// MergeStandardPR owns standard REST/GraphQL merge semantics. It freezes the
// current source/base refs, enforces the repository's existing policy and selects
// an explicitly configured merge authority independently of the CI backend.
func (s *Service) MergeStandardPR(ctx context.Context, repository string, number int, method, message, expected string) (db.PullRequest, error) {
	actor, e := s.GetCurrentUser(ctx)
	if e != nil {
		return db.PullRequest{}, e
	}
	if _, delegated := DelegatedSessionIDFromContext(ctx); delegated {
		return db.PullRequest{}, ErrForbidden
	}
	pr, e := s.GetPR(ctx, repository, number)
	if e != nil {
		return pr, e
	}
	if pr.Merged || pr.State != db.StateOpen || pr.Draft {
		return pr, fmt.Errorf("%w: pull request is not open and ready", ErrInvalidState)
	}
	method = strings.ToLower(strings.TrimSpace(method))
	if method == "" {
		method = "merge"
	}
	switch method {
	case "merge", "rebase", "squash":
	default:
		return pr, fmt.Errorf("%w: unsupported standard merge method", ErrValidation)
	}
	expected = strings.ToLower(strings.TrimSpace(expected))
	if expected != "" && !canonicalActionSHA(expected) {
		return pr, fmt.Errorf("%w: invalid expected head SHA", ErrValidation)
	}
	if s.Git == nil {
		return pr, fmt.Errorf("%w: Git storage is unavailable", ErrInvalidState)
	}
	// Do not silently apply a same-repo ref transaction to a fork's unrelated head.
	// Existing provider-owned fork merges may still be handled by their authority.
	providerMethod, providerOwned := s.ForgejoIntegration.ConfiguredMergeMethod(repository)
	if !providerOwned && pr.HeadRepositoryID != 0 && pr.HeadRepositoryID != pr.RepositoryID {
		return pr, fmt.Errorf("%w: atomic cross-repository merge is not available", ErrInvalidState)
	}
	headRepository := repository
	if pr.HeadRepositoryID != 0 && pr.HeadRepositoryID != pr.RepositoryID {
		headRepo, e := s.GetRepoByID(ctx, strconv.FormatUint(uint64(pr.HeadRepositoryID), 10))
		if e != nil {
			return pr, e
		}
		headRepository = headRepo.FullName
	}
	head, e := s.Git.HeadSHA(ctx, headRepository, pr.HeadRef)
	if e != nil {
		return pr, e
	}
	if expected != "" && head != expected {
		return pr, fmt.Errorf("%w: head changed since it was observed", ErrConflict)
	}
	if pr.HeadSHA != head {
		return pr, fmt.Errorf("%w: pull request head projection has not caught up", ErrConflict)
	}
	expected = head
	// Each owning execution path below performs enforceMergePolicy once,
	// including selected CI. Repeating that remote observation here adds latency
	// but cannot strengthen the eventual atomic head/base check.
	if providerOwned {
		// A configured fast-forward-only authority is a stricter rebase path: it
		// accepts only already-linear input, without manufacturing a merge commit.
		if method == "rebase" && providerMethod == "fast-forward-only" {
			method = providerMethod
		}
		if method != providerMethod {
			return pr, fmt.Errorf("%w: method differs from configured merge authority", ErrConflict)
		}
		receipt, e := s.executeNativeProviderMerge(ctx, repository, number, HumanProviderMergeInput{ExpectedHeadSHA: expected, MergeMethod: method}, false)
		if e != nil {
			return pr, e
		}
		if !receipt.ProviderMerged && receipt.Outcome != providerMergeOutcomeSourceSuccess {
			return pr, fmt.Errorf("%w: provider merge outcome not confirmed", ErrConflict)
		}
		// Signed provider reconciliation remains the terminal AGS fact owner. Wait a
		// bounded interval; never claim merged merely because a provider POST returned.
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			current, e := s.GetPR(ctx, repository, number)
			if e != nil {
				return pr, e
			}
			if current.Merged {
				return current, nil
			}
			select {
			case <-ctx.Done():
				return pr, ctx.Err()
			case <-deadline.C:
				return pr, fmt.Errorf("%w: provider confirmed merge; AGS projection pending, observe this PR before retrying", ErrConflict)
			case <-tick.C:
			}
		}
	}
	base, e := s.Git.HeadSHA(ctx, repository, pr.BaseRef)
	if e != nil {
		return pr, e
	}
	ctx = context.WithValue(ctx, mergePreconditionKey{}, mergePrecondition{Head: expected, Base: base})
	if e = s.mergePRRecordWithUser(ctx, actor, &pr, method, message); e != nil {
		return pr, e
	}
	return pr, nil
}

// MergeStandardPRByID resolves the GraphQL node without leaking private state.
func (s *Service) MergeStandardPRByID(ctx context.Context, id uint, method, message, expected string) (db.PullRequest, error) {
	if _, e := s.GetCurrentUser(ctx); e != nil {
		return db.PullRequest{}, e
	}
	pr, e := s.loadPRForMergeByID(ctx, id)
	if e != nil {
		return pr, e
	}
	return s.MergeStandardPR(ctx, pr.Repository.FullName, pr.Number, method, message, expected)
}
