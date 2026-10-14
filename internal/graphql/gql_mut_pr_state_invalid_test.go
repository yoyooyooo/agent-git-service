package graphql

import (
	"context"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
)

func TestPRStateMutationRejectsInvalidIDWithoutInventingSuccess(t *testing.T) {
	for _, mutation := range []struct{ name, state string }{{"reopenPullRequest", db.StateOpen}, {"closePullRequest", db.StateClosed}} {
		for _, id := range []string{"", "not-a-node", "Issue_1", "PullRequest_no-number"} {
			t.Run(mutation.name+"/"+id, func(t *testing.T) {
				result := (&Server{}).doSetPRState(context.Background(), gqlRequest{Variables: map[string]any{"input": map[string]any{"pullRequestId": id}}}, mutation.state, mutation.name)
				if result["errors"] == nil || result["data"] != nil {
					t.Fatalf("invalid ID returned success: %#v", result)
				}
			})
		}
	}
}
