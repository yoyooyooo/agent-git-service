package graphql

import (
	"context"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
)

func TestOrdinaryPRSelectionDoesNotRequireCIObservation(t *testing.T) {
	// No service is deliberately provided: a simple PR mutation/view response
	// must not invoke repository policy or a CI provider merely to build its shape.
	server := &Server{}
	for _, query := range []string{
		`query { repository(owner:"team",name:"project") { pullRequest(number:1) { number title } } }`,
		`mutation { createPullRequest(input:{repositoryId:"fixture",headRefName:"feature",baseRefName:"main"}) { pullRequest { number url } } }`,
	} {
		state := &responseErrors{query: query}
		ctx := context.WithValue(context.Background(), responseErrorsKey{}, state)
		result := server.statusCheckRollupGQL(ctx, db.PullRequest{HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
		if result == nil || len(state.items) != 0 {
			t.Fatalf("ordinary PR selection was coupled to CI: %#v", result)
		}
	}
}
