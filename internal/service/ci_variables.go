package service

import (
	"context"
	"errors"

	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/db"
)

// CIRepositoryVariables reads only repository definitions from the selected
// provider. Values require a durable authenticated repository administrator.
func (s *Service) CIRepositoryVariables(ctx context.Context, repository string, page, perPage int) (cibackend.Variables, error) {
	reader, boundRepository, err := s.repositoryVariableReader(ctx, repository)
	if err != nil {
		return cibackend.Variables{}, err
	}
	maxInt := int(^uint(0) >> 1)
	if page < 1 || perPage < 1 || perPage > 100 || page-1 > (maxInt-perPage)/perPage {
		return cibackend.Variables{}, ErrValidation
	}
	variables, err := reader.RepositoryVariables(ctx, boundRepository, page, perPage)
	return variables, variableProviderError(err)
}

// CIRepositoryVariable distinguishes a missing definition from an empty value.
func (s *Service) CIRepositoryVariable(ctx context.Context, repository, name string) (cibackend.Variable, error) {
	reader, boundRepository, err := s.repositoryVariableReader(ctx, repository)
	if err != nil {
		return cibackend.Variable{}, err
	}
	if !cibackend.ValidVariableName(name) {
		return cibackend.Variable{}, ErrValidation
	}
	variable, err := reader.RepositoryVariable(ctx, boundRepository, name)
	return variable, variableProviderError(err)
}

func (s *Service) repositoryVariableReader(ctx context.Context, repository string) (cibackend.RepositoryVariableReader, string, error) {
	// The general permission helper permits trusted internal calls without a
	// user. Provider values must never inherit that bypass or public-repo reads.
	if viewer, ok := UserFromContext(ctx); !ok || viewer.ID == 0 {
		return nil, "", ErrUnauthorized
	}
	repo, err := s.GetRepo(ctx, repository)
	if err != nil {
		return nil, "", err
	}
	// Admin has no delegated capability mapping. Existing review/CI sessions
	// cannot acquire variable values through their durable principal's grants.
	if err := s.RequireRepoPermission(ctx, repo.ID, RepoPermissionAdmin); err != nil {
		return nil, "", err
	}
	selected, err := s.CISelection(repo.FullName)
	if err != nil {
		return nil, "", err
	}
	reader, supported := selected.Backend.(cibackend.RepositoryVariableReader)
	if selected.Kind != "forgejo" || !supported {
		return nil, "", cibackend.ErrUnsupported
	}
	return reader, selected.Binding.Repository, nil
}

func variableProviderError(err error) error {
	if err == nil {
		return nil
	}
	for _, kind := range []error{cibackend.ErrNotFound, cibackend.ErrUnsupported, cibackend.ErrInvalid, cibackend.ErrUnavailable} {
		if errors.Is(err, kind) {
			return kind
		}
	}
	// Provider credentials and transport failures belong to AGS, not the
	// caller. Preserve neither provider bodies nor a misleading caller denial.
	return cibackend.ErrUnavailable
}

// requireNativeVariableScope protects every native storage entry point, so
// slug routes, numeric repository routes and direct service callers agree.
func (s *Service) requireNativeVariableScope(ctx context.Context, repoID *uint) error {
	if s.CI.NativeVariablesOnly() {
		return nil
	}
	if repoID == nil {
		// Organization routes have no repository binding with which to choose
		// a provider. A mixed instance cannot claim native organization truth.
		return cibackend.ErrUnsupported
	}
	var repo db.Repository
	if err := s.DBForCtx(ctx).Select("id", "full_name").First(&repo, "id = ?", *repoID).Error; err != nil {
		return wrapErr(err)
	}
	selected, err := s.CISelection(repo.FullName)
	if err != nil {
		return err
	}
	if selected.Name != "native" {
		return cibackend.ErrUnsupported
	}
	return nil
}
