package service

import (
	"context"
	"errors"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

var ErrReplicationRegistrationConflict = errors.New("replication registration observation changed")

// ObserveReplicationRegistration is side-effect-free. Authority comes from the
// owning primary configuration, not an HTTP request or a peer certificate.
func (s *Service) ObserveReplicationRegistration(ctx context.Context, authority, fullName string) (edgeprotocol.Registration, error) {
	return s.replicationRegistration(ctx, authority, fullName, nil)
}

// RegisterReplicationRepository is idempotent for the exact observed row. It
// only allocates internal storage identity; it never adds a peer or modifies
// its grant set, starts a listener, changes refs or enables local Edge reads.
func (s *Service) RegisterReplicationRepository(ctx context.Context, authority, fullName string, expected edgeprotocol.RegisterRepository) (edgeprotocol.Registration, error) {
	if expected.Validate() != nil {
		return edgeprotocol.Registration{}, ErrValidation
	}
	return s.replicationRegistration(ctx, authority, fullName, &expected)
}

func (s *Service) replicationRegistration(ctx context.Context, authority, fullName string, expected *edgeprotocol.RegisterRepository) (edgeprotocol.Registration, error) {
	if err := s.requireReplicationAdministrator(ctx); err != nil {
		return edgeprotocol.Registration{}, err
	}
	probe := edgeprotocol.RepositoryIdentity{AuthorityID: authority, StoreID: "registration", RepositoryID: 1, Kind: "repo"}
	if probe.Validate() != nil || !edgeprotocol.ValidRepositoryLocator(fullName) {
		return edgeprotocol.Registration{}, ErrValidation
	}
	var result edgeprotocol.Registration
	err := s.Git.WithSnapshotCapture(ctx, func(ctx context.Context) error {
		ctx = ContextWithRepoCache(ctx)
		repo, err := s.LookupRepoIdentity(ctx, fullName)
		if err != nil {
			return err
		}
		if err := s.RequireRepoPermission(ctx, repo.ID, RepoPermissionAdmin); err != nil {
			return err
		}
		if repo.Disabled {
			return ErrForbidden
		}
		// Identity lookup intentionally omits creation metadata. Load the row's
		// creation fact under this same barrier, after native authorization.
		if err := s.DBForCtx(ctx).WithContext(ctx).Select("id", "created_at").First(&repo, repo.ID).Error; err != nil {
			return wrapErr(err)
		}
		if expected != nil {
			if expected.ExpectedAuthorityID != authority || expected.ExpectedRepositoryID != uint64(repo.ID) || !expected.ExpectedCreatedAt.Equal(repo.CreatedAt) {
				return ErrReplicationRegistrationConflict
			}
			if err := s.allocateReplicationStore(ctx, &repo); err != nil {
				return err
			}
		}
		result = edgeprotocol.Registration{Version: edgeprotocol.RegistrationVersion, AuthorityID: authority, Repository: repo.FullName, RepositoryID: uint64(repo.ID), CreatedAt: repo.CreatedAt.UTC()}
		if repo.GitStorageID != nil {
			identity, err := replicationIdentity(repo, authority, "repo")
			if err != nil {
				return err
			}
			result.Identity = &identity
		}
		return result.Validate()
	})
	return result, err
}
