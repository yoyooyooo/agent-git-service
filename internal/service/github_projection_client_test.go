package service_test

import (
	"context"

	"github.com/ngaut/agent-git-service/internal/githubintegration"
)

func (c *projectionCloseGitHubClient) ObservePullRequest(ctx context.Context, repo string, number int, head, base string) (githubintegration.PullRequest, bool, error) {
	c.closeSource = head
	c.closeTarget = base
	return githubintegration.PullRequest{Number: 8, State: "open"}, true, nil
}

func (c *projectionCloseGitHubClient) ClosePullRequest(ctx context.Context, repo string, number int) (githubintegration.PullRequest, error) {
	return githubintegration.PullRequest{Number: number, State: "closed"}, nil
}
