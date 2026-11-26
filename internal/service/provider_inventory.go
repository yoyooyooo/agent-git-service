package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

const (
	ProviderPullRequestInventorySchema = "ags.provider-pull-request-inventory.v1"
	ProviderPullRequestReadSchema      = "ags.provider-pull-request.v1"
	ProviderInventoryMaxPage           = 10000
	ProviderInventoryMaxPerPage        = 100
)

var (
	ErrProviderInventoryNotConfigured = errors.New("provider inventory is not configured")
	ErrProviderInventoryUnavailable   = errors.New("provider inventory is unavailable")
)

// ProviderInventoryPullRequest correlates a live provider PR with its current
// AGS mapping only. A missing mapping is not evidence of historical ownership.
type ProviderInventoryPullRequest struct {
	ProviderProjectionObservation
	Title         string `json:"title"`
	UpdatedAt     string `json:"updated_at"`
	AGSPRNumber   *int   `json:"ags_pr_number"`
	MappingStatus string `json:"mapping_status"`
}

type ProviderPullRequestInventory struct {
	Schema       string                         `json:"schema"`
	Repository   string                         `json:"repository"`
	Provider     string                         `json:"provider"`
	ExternalRepo string                         `json:"external_repo"`
	State        string                         `json:"state"`
	Page         int                            `json:"page"`
	PerPage      int                            `json:"per_page"`
	NextPage     *int                           `json:"next_page"`
	PullRequests []ProviderInventoryPullRequest `json:"pull_requests"`
	ObservedAt   time.Time                      `json:"observed_at"`
}

type ProviderPullRequestRead struct {
	Schema       string                       `json:"schema"`
	Repository   string                       `json:"repository"`
	Provider     string                       `json:"provider"`
	ExternalRepo string                       `json:"external_repo"`
	PullRequest  ProviderInventoryPullRequest `json:"pull_request"`
	ObservedAt   time.Time                    `json:"observed_at"`
}

func (s *Service) providerObservationRepository(ctx context.Context, repoFullName string) (db.Repository, string, error) {
	// These are repository-wide observations, never an extension of a PR-scoped
	// delegated capability. Check here as well as at the HTTP surface.
	if _, delegated := DelegatedSessionIDFromContext(ctx); delegated {
		return db.Repository{}, "", ErrForbidden
	}
	user, ok := UserFromContext(ctx)
	if !ok || user.ID == 0 {
		return db.Repository{}, "", ErrUnauthorized
	}
	repo, err := s.projectionRepository(ctx, repoFullName)
	if err != nil {
		return db.Repository{}, "", wrapErr(err)
	}
	// HasRepoAccess includes a legacy compatibility backfill. This observation
	// must read the native grant without repairing or creating any authority.
	permission, err := s.currentRepoAccess(ctx, repo.ID, user.ID)
	if err != nil {
		return db.Repository{}, "", err
	}
	if !permission.AtLeast(RepoPermissionRead) && repo.Private {
		return db.Repository{}, "", ErrNotFound
	}
	target, err := s.ForgejoIntegration.ConfiguredTargetFullName(repo.FullName)
	if err != nil {
		return db.Repository{}, "", ErrProviderInventoryNotConfigured
	}
	return repo, target, nil
}

func (s *Service) ListProviderPullRequests(ctx context.Context, repoFullName, state string, page, perPage int) (ProviderPullRequestInventory, error) {
	repo, target, err := s.providerObservationRepository(ctx, repoFullName)
	if err != nil {
		return ProviderPullRequestInventory{}, err
	}
	if state != "open" && state != "closed" && state != "all" {
		return ProviderPullRequestInventory{}, fmt.Errorf("%w: state must be open, closed or all", ErrValidation)
	}
	if page < 1 || page > ProviderInventoryMaxPage || perPage < 1 || perPage > ProviderInventoryMaxPerPage {
		return ProviderPullRequestInventory{}, fmt.Errorf("%w: page must be 1..10000 and per_page must be 1..100", ErrValidation)
	}
	observed, err := s.ForgejoIntegration.PullRequestInventoryPage(ctx, repo.FullName, target, state, page, perPage)
	if err != nil {
		return ProviderPullRequestInventory{}, fmt.Errorf("%w: %w", ErrProviderInventoryUnavailable, err)
	}
	if len(observed) > perPage || (page == ProviderInventoryMaxPage && len(observed) > 0) {
		return ProviderPullRequestInventory{}, ErrProviderInventoryUnavailable
	}
	rows, err := s.providerInventoryRows(ctx, repo, target, observed)
	if err != nil {
		return ProviderPullRequestInventory{}, err
	}
	for _, row := range rows {
		if state != "all" && row.State != state {
			return ProviderPullRequestInventory{}, ErrProviderInventoryUnavailable
		}
	}
	var next *int
	if len(rows) > 0 {
		n := page + 1
		next = &n
	}
	return ProviderPullRequestInventory{
		Schema: ProviderPullRequestInventorySchema, Repository: repo.FullName,
		Provider: ProjectionProviderForgejo, ExternalRepo: target, State: state,
		Page: page, PerPage: perPage, NextPage: next, PullRequests: rows, ObservedAt: time.Now().UTC(),
	}, nil
}

// ReadProviderPullRequest accepts a provider number, including an old or orphan
// projection which is no longer reachable through an AGS PR's current mapping.
func (s *Service) ReadProviderPullRequest(ctx context.Context, repoFullName string, number int) (ProviderPullRequestRead, error) {
	repo, target, err := s.providerObservationRepository(ctx, repoFullName)
	if err != nil {
		return ProviderPullRequestRead{}, err
	}
	if number <= 0 {
		return ProviderPullRequestRead{}, fmt.Errorf("%w: provider number must be positive", ErrValidation)
	}
	observed, found, err := s.ForgejoIntegration.ExactPullRequestSnapshot(ctx, repo.FullName, target, number)
	if err != nil {
		return ProviderPullRequestRead{}, fmt.Errorf("%w: %w", ErrProviderInventoryUnavailable, err)
	}
	if !found {
		return ProviderPullRequestRead{}, ErrNotFound
	}
	if observed.Number != number {
		return ProviderPullRequestRead{}, ErrProviderInventoryUnavailable
	}
	rows, err := s.providerInventoryRows(ctx, repo, target, []forgejointegration.PullRequestSnapshot{observed})
	if err != nil {
		return ProviderPullRequestRead{}, err
	}
	return ProviderPullRequestRead{
		Schema: ProviderPullRequestReadSchema, Repository: repo.FullName,
		Provider: ProjectionProviderForgejo, ExternalRepo: target, PullRequest: rows[0], ObservedAt: time.Now().UTC(),
	}, nil
}

func (s *Service) providerInventoryRows(ctx context.Context, repo db.Repository, target string, observed []forgejointegration.PullRequestSnapshot) ([]ProviderInventoryPullRequest, error) {
	rows := make([]ProviderInventoryPullRequest, 0, len(observed))
	numbers := make([]int, 0, len(observed))
	seen := make(map[int]bool, len(observed))
	for _, pull := range observed {
		if pull.Number <= 0 || seen[pull.Number] || (pull.State != "open" && pull.State != "closed") || len(pull.Title) > 8192 || len(pull.UpdatedAt) > 128 {
			return nil, ErrProviderInventoryUnavailable
		}
		observation, err := providerProjectionObservation(pull)
		if err != nil {
			return nil, ErrProviderInventoryUnavailable
		}
		seen[pull.Number] = true
		numbers = append(numbers, pull.Number)
		rows = append(rows, ProviderInventoryPullRequest{
			ProviderProjectionObservation: observation, Title: pull.Title, UpdatedAt: pull.UpdatedAt, MappingStatus: "unmapped",
		})
	}
	if len(numbers) == 0 {
		return rows, nil
	}
	var projections []db.PullRequestProjection
	if err := s.DBForCtx(ctx).Preload("PullRequest").
		Where("repository_id = ? AND provider = ? AND external_repo = ? AND external_number IN ?", repo.ID, ProjectionProviderForgejo, target, numbers).
		Find(&projections).Error; err != nil {
		return nil, wrapErr(err)
	}
	mapping := make(map[int]int, len(projections))
	for _, projection := range projections {
		pr := projection.PullRequest
		if pr.ID == 0 || pr.RepositoryID != repo.ID || pr.Number <= 0 || mapping[projection.ExternalNumber] != 0 {
			return nil, ErrProviderInventoryUnavailable
		}
		mapping[projection.ExternalNumber] = pr.Number
	}
	for n := range rows {
		if number, ok := mapping[rows[n].Number]; ok {
			rows[n].AGSPRNumber = &number
			rows[n].MappingStatus = "current"
		}
	}
	return rows, nil
}
