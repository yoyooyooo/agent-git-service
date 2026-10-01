package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/ngaut/agent-git-service/internal/artifactretirement"
	"github.com/ngaut/agent-git-service/internal/db"
)

const retirementProviderCheckpointSchema = "ags.artifact-retirement.providers.v2"

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
	legacy := err == nil && saved.Schema == "ags.artifact-retirement.providers.v1"
	if (os.IsNotExist(err) || legacy) && mayCreate {
		// v1 included ProjectionRefState drift rows as provider-write authority.
		// Before any publication, recompute under the current policy and replace
		// that legacy journal. Once publication can have started, never shrink a
		// saved write set: doing so could hide a partially moved provider branch.
		required, queryErr := s.artifactRetirementProviderRequirements(ctx, repo, plan)
		if queryErr != nil {
			return nil, queryErr
		}
		saved = retirementProvidersCheckpoint{Schema: retirementProviderCheckpointSchema, IntentSHA256: plan.IntentSHA256, RepositoryID: repo.ID, Required: required}
		if saveErr := artifactretirement.SaveCheckpoint(path, saved); saveErr != nil {
			return nil, saveErr
		}
		err = nil
	} else if err != nil {
		return nil, err
	} else if legacy {
		return nil, errors.New("artifact retirement legacy provider journal cannot resume after publication")
	}
	if saved.Schema != retirementProviderCheckpointSchema || saved.IntentSHA256 != plan.IntentSHA256 || saved.RepositoryID != repo.ID || saved.Required == nil {
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
