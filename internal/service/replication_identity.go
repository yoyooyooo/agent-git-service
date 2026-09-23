package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/tenant"
)

// ProvisionReplicationIdentity is an operator capability, not a public read
// side effect. Native repo-admin permission is required. Allocation shares the
// exclusive capture barrier so delete/recreate cannot replace the row between
// authorization and the CAS. Only DB metadata changes here, never source Git.
// Peers must explicitly register the result; names do not imply copy authority.
func (s *Service) ProvisionReplicationIdentity(ctx context.Context, authority, fullName, kind string) (edgeprotocol.RepositoryIdentity, error) {
	if err := s.requireReplicationAdministrator(ctx); err != nil {
		return edgeprotocol.RepositoryIdentity{}, err
	}
	probe := edgeprotocol.RepositoryIdentity{AuthorityID: authority, StoreID: "provision", RepositoryID: 1, Kind: kind}
	if probe.Validate() != nil || s.Git == nil {
		return edgeprotocol.RepositoryIdentity{}, ErrValidation
	}
	var result edgeprotocol.RepositoryIdentity
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
		if err := s.allocateReplicationStore(ctx, &repo); err != nil {
			return err
		}
		result, err = replicationIdentity(repo, authority, kind)
		return err
	})
	return result, err
}

// All operator paths share the same single-DB, native-admin boundary. A
// transport Session or tenant context cannot bootstrap replica privileges.
func (s *Service) requireReplicationAdministrator(ctx context.Context) error {
	if _, ok := UserFromContext(ctx); !ok {
		return ErrUnauthorized
	}
	if _, ok := DelegatedSessionIDFromContext(ctx); ok {
		return ErrForbidden
	}
	if _, ok := tenant.FromContext(ctx); ok {
		return ErrForbidden
	}
	if database, ok := DBFromContext(ctx); ok && database != nil && database != s.DB {
		return ErrForbidden
	}
	if s.Git == nil || s.DB == nil {
		return ErrValidation
	}
	return nil
}

// Caller holds the capture barrier and has just checked native repo-admin.
func (s *Service) allocateReplicationStore(ctx context.Context, repo *db.Repository) error {
	if repo.GitStorageID != nil {
		return nil
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	id := hex.EncodeToString(random[:])
	if err := s.DBForCtx(ctx).WithContext(ctx).Model(&db.Repository{}).
		Where("id = ? AND git_storage_id IS NULL", repo.ID).Update("git_storage_id", id).Error; err != nil {
		return err
	}
	if err := s.DBForCtx(ctx).WithContext(ctx).Select("id", "git_storage_id").First(repo, repo.ID).Error; err != nil {
		return wrapErr(err)
	}
	return nil
}

func replicationIdentity(repo db.Repository, authority, kind string) (edgeprotocol.RepositoryIdentity, error) {
	if repo.GitStorageID == nil || len(*repo.GitStorageID) != 32 {
		return edgeprotocol.RepositoryIdentity{}, errors.New("repository storage is not provisioned for replication")
	}
	if _, err := hex.DecodeString(*repo.GitStorageID); err != nil {
		return edgeprotocol.RepositoryIdentity{}, ErrValidation
	}
	identity := edgeprotocol.RepositoryIdentity{AuthorityID: authority, RepositoryID: uint64(repo.ID), StoreID: *repo.GitStorageID + "-" + kind, Kind: kind}
	if err := identity.Validate(); err != nil {
		return edgeprotocol.RepositoryIdentity{}, err
	}
	return identity, nil
}
