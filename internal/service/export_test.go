package service

import (
	"context"

	"github.com/ngaut/agent-git-service/internal/db"
)

func SetTestSyncPRHeadAfterTipRead(fn func()) {
	testSyncPRHeadAfterTipRead = fn
}

func SetTestEnqueueForgejoPullRequestProjection(fn func(*Service, context.Context, db.PullRequest) (db.PullRequestProjectionJob, error)) {
	testEnqueueForgejoPullRequestProjection = fn
}
