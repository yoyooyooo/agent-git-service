package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/ngaut/agent-git-service/internal/artifactretirement"
	"github.com/ngaut/agent-git-service/internal/db"
)

type retirementProvidersCheckpoint struct {
	Schema       string                     `json:"schema"`
	IntentSHA256 string                     `json:"intent_sha256"`
	RepositoryID uint                       `json:"repository_id"`
	Required     map[string]map[string]bool `json:"required"`
}

// Persist the complete provider requirement before any provider moves. DB
// coordinates may be old or new on recovery and are not remote-write receipts.
func (s *Service) retirementProviderCheckpoint(ctx context.Context, repo db.Repository, plan artifactretirement.Plan, stateRoot string, mayCreate bool) (map[string]map[string]bool, error) {
	path := filepath.Join(stateRoot, "providers.json")
	var saved retirementProvidersCheckpoint
	err := artifactretirement.LoadCheckpoint(path, &saved)
	if os.IsNotExist(err) && mayCreate {
		required, err := s.artifactRetirementProviderRequirements(ctx, repo, plan)
		if err != nil {
			return nil, err
		}
		saved = retirementProvidersCheckpoint{Schema: "ags.artifact-retirement.providers.v1", IntentSHA256: plan.IntentSHA256, RepositoryID: repo.ID, Required: required}
		if err := artifactretirement.SaveCheckpoint(path, saved); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if saved.Schema != "ags.artifact-retirement.providers.v1" || saved.IntentSHA256 != plan.IntentSHA256 || saved.RepositoryID != repo.ID || saved.Required == nil {
		return nil, errors.New("artifact retirement provider journal does not match this operation")
	}
	return saved.Required, nil
}

func sameRetirementRefs(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for ref, oid := range a {
		if b[ref] != oid {
			return false
		}
	}
	return true
}
