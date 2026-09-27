package service

import (
	"context"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
)

func TestPRCreationWithNoIntegrationsHasNoProjectionDependencies(t *testing.T) {
	// No DB/Git/provider is needed for the disabled integration hook. Native PR
	// creation has already committed; this optional hook must be a true no-op.
	for _, svc := range []*Service{nil, {}} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := svc.EnqueuePullRequestCreatedIntegrations(ctx, db.PullRequest{ID: 42}); err != nil {
			t.Fatal(err)
		}
	}
}
