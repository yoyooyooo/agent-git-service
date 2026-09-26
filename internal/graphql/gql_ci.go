package graphql

import (
	"context"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
	"sort"
	"strings"
)

func pendingCICheck(name string) map[string]any {
	return map[string]any{"__typename": "CheckRun", "name": name, "status": "QUEUED", "conclusion": "", "isRequired": true, "detailsUrl": "", "checkSuite": map[string]any{"workflowRun": map[string]any{"workflow": map[string]any{"name": ""}}}}
}
func (s *Server) statusCheckRollupGQL(ctx context.Context, pr db.PullRequest) any {
	if pr.HeadSHA == "" {
		return nil
	}
	selected, err := s.Svc.CISelection(pr.Repository.FullName)
	if err != nil {
		addResponseError(ctx, service.CIErrorMessage(err))
		return ciRollupConnection(pr.HeadSHA, []any{})
	}
	if selected.Name == "native" {
		return s.nativeStatusCheckRollupGQL(ctx, pr)
	}
	observed, err := s.Svc.ReadCIChecks(ctx, pr)
	if err != nil {
		addResponseError(ctx, service.CIErrorMessage(err))
		return ciRollupConnection(pr.HeadSHA, []any{})
	}
	if !observed.RequiredKnown && queryRequiresCIPolicy(ctx) {
		addResponseError(ctx, "Required CI policy is unknown; configure required_checks explicitly")
	}
	sort.Slice(observed.Checks, func(i, j int) bool { return observed.Checks[i].Name < observed.Checks[j].Name })
	nodes := make([]any, 0, len(observed.Checks))
	for _, check := range observed.Checks {
		var required any = check.Required
		if !observed.RequiredKnown {
			required = nil
		}
		nodes = append(nodes, map[string]any{"__typename": "CheckRun", "id": check.ID, "name": check.Name, "status": strings.ToUpper(check.Status), "conclusion": strings.ToUpper(check.Conclusion), "isRequired": required, "detailsUrl": check.URL, "startedAt": check.StartedAt, "completedAt": check.CompletedAt, "checkSuite": map[string]any{"workflowRun": map[string]any{"event": check.Event, "workflow": map[string]any{"name": check.Workflow}}}})
	}
	return ciRollupConnection(pr.HeadSHA, nodes)
}
