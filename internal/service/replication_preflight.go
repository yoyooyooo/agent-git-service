package service

import (
	"context"
	"errors"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

// ReplicationSourcePath resolves an operator-registered, already-provisioned
// identity for startup checks. It grants no user access, creates nothing and
// does not replace the per-request original-user checks in PrimaryReadAuthority.
func (s *Service) ReplicationSourcePath(ctx context.Context, expected edgeprotocol.RepositoryIdentity) (string, error) {
	if expected.Validate() != nil || expected.Kind != "repo" || s.Git == nil {
		return "", errors.New("invalid replication store")
	}
	var repo db.Repository
	if err := s.DB.WithContext(ctx).Select("id", "full_name", "disabled", "git_storage_id").First(&repo, expected.RepositoryID).Error; err != nil {
		return "", errors.New("registered replication repository is unavailable")
	}
	identity, err := replicationIdentity(repo, expected.AuthorityID, "repo")
	if err != nil || identity != expected || repo.Disabled {
		return "", errors.New("registered replication identity is stale or disabled")
	}
	if !s.Git.Exists(ctx, repo.FullName) {
		return "", errors.New("registered replication Git storage is unavailable")
	}
	return s.Git.GetRepoPath(ctx, repo.FullName)
}
