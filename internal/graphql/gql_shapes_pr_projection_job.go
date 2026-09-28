package graphql

import (
	"context"

	"github.com/ngaut/agent-git-service/internal/db"
)

// projectionJobGQL exposes the existing durable projection job, not a new
// provider action or a synchronous provider lookup. In particular a queued or
// failed projection is not a failed AGS PR creation. There is no guessed URL.
func (s *Server) projectionJobGQL(ctx context.Context, pr db.PullRequest, query string) any {
	if !queryHasAny(query, "projectionJob") {
		return nil
	}
	job, present, err := s.Svc.PullRequestProjectionJob(ctx, pr.ID)
	if err != nil {
		logErr(ctx, "projectionJob GraphQL", err)
		return map[string]any{"provider": "forgejo", "status": "unavailable", "phase": "unavailable"}
	}
	if !present {
		return nil
	}
	return map[string]any{
		"jobId":            job.JobID,
		"provider":         job.Provider,
		"trigger":          job.Trigger,
		"actionGeneration": job.ActionGeneration,
		"nextRepairAction": job.NextRepairAction,
		"status":           job.Status,
		"phase":            job.Phase,
		"attempt":          job.Attempt,
		"headRef":          job.HeadRef,
		"baseRef":          job.BaseRef,
		"remoteSha":        job.RemoteSHA,
		"externalRepo":     job.ExternalRepo,
		"externalNumber":   job.ExternalNumber,
		"externalUrl":      job.ExternalURL,
		"lastErrorType":    job.LastErrorType,
		"updatedAt":        job.UpdatedAt,
	}
}
