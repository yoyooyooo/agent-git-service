package service

import (
	"context"

	"github.com/ngaut/agent-git-service/internal/db"
)

// ForgejoPullRequestBrowserURL reads the durable projection identity. Missing,
// pending, inaccessible, invalid or ambiguous mappings fall back to the configured
// repository's PR list; the AGS number is never used as a provider number.
func (s *Service) ForgejoPullRequestBrowserURL(ctx context.Context, repoFullName string, number int) (string, bool) {
	fallback, ok := s.ForgejoIntegration.BrowserURL(repoFullName, "/pulls")
	if !ok {
		return "", false
	}
	pr, err := s.GetPR(ctx, repoFullName, number)
	if err != nil {
		return fallback, true
	}
	rows, err := s.ListPullRequestProjections(ctx, pr.ID)
	if err != nil {
		return fallback, true
	}
	var matches []db.PullRequestProjection
	for _, row := range rows {
		if row.Provider == ProjectionProviderForgejo {
			matches = append(matches, row)
		}
	}
	if len(matches) != 1 {
		return fallback, true
	}
	mapping := matches[0]
	if mapping.RepositoryID != pr.RepositoryID || mapping.ExternalNumber <= 0 ||
		(mapping.State != ProjectionStateOpen && mapping.State != ProjectionStateClosed && mapping.State != ProjectionStateMerged) {
		return fallback, true
	}
	location, ok := s.ForgejoIntegration.PullRequestBrowserURL(repoFullName, mapping.ExternalRepo, mapping.ExternalNumber)
	if !ok {
		return fallback, true
	}
	return location, true
}
