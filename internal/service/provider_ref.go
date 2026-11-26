package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/gitstore"
)

type ProviderGitRef struct {
	Schema       string    `json:"schema"`
	Repository   string    `json:"repository"`
	Provider     string    `json:"provider"`
	ExternalRepo string    `json:"external_repo"`
	Ref          string    `json:"ref"`
	SHA          string    `json:"sha"`
	ObservedAt   time.Time `json:"observed_at"`
}

func (s *Service) ReadProviderGitRef(ctx context.Context, repoFullName, ref string) (ProviderGitRef, error) {
	repo, target, err := s.providerObservationRepository(ctx, repoFullName)
	if err != nil {
		return ProviderGitRef{}, err
	}
	if !strings.HasPrefix(ref, "refs/heads/") || !gitstore.IsValidRefName(ref) {
		return ProviderGitRef{}, fmt.Errorf("%w: ref must be a full refs/heads/ branch name", ErrValidation)
	}
	sha, found, err := s.ForgejoIntegration.ExactBranchSHA(ctx, repo.FullName, target, strings.TrimPrefix(ref, "refs/heads/"))
	if err != nil {
		return ProviderGitRef{}, fmt.Errorf("%w: %w", ErrProviderInventoryUnavailable, err)
	}
	if !found {
		return ProviderGitRef{}, ErrNotFound
	}
	return ProviderGitRef{
		Schema: "ags.provider-git-ref.v1", Repository: repo.FullName, Provider: ProjectionProviderForgejo,
		ExternalRepo: target, Ref: ref, SHA: sha, ObservedAt: time.Now().UTC(),
	}, nil
}
