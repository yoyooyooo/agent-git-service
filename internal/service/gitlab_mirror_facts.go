package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
	"github.com/ngaut/agent-git-service/internal/gitstore"
)

// GitLabMirrorFacts describes the current instance's configuration, never an
// observed GitLab ref or permission to perform a provider write.
type GitLabMirrorFacts struct {
	Schema              string `json:"schema"`
	Repository          string `json:"repository"`
	ConfigurationSource string `json:"configuration_source"`
	GitLabRefReadback   string `json:"gitlab_ref_readback"`
	gitlabintegration.MirrorFacts
}

func (s *Service) GetGitLabMirrorFacts(ctx context.Context, repoFullName, ref string) (GitLabMirrorFacts, error) {
	repo, err := s.projectionRepository(ctx, repoFullName)
	if err != nil {
		return GitLabMirrorFacts{}, wrapErr(err)
	}
	if err := s.requireRepoPermission(ctx, repo.ID, RepoPermissionRead); err != nil {
		return GitLabMirrorFacts{}, err
	}
	if !strings.HasPrefix(ref, "refs/") || !gitstore.IsValidRefName(ref) {
		return GitLabMirrorFacts{}, fmt.Errorf("%w: ref must be a full Git ref name", ErrValidation)
	}
	facts, err := s.GitLabIntegration.MirrorFacts(repo.FullName, ref)
	if err != nil {
		return GitLabMirrorFacts{}, err
	}
	return GitLabMirrorFacts{
		Schema: "ags.gitlab-mirror-facts.v1", Repository: repo.FullName,
		ConfigurationSource: "running_integration", GitLabRefReadback: "not_claimed", MirrorFacts: facts,
	}, nil
}
