package multicafailures

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/service"
)

type fakeRunner struct {
	calls [][]string
	outs  map[string]string
}

func (r *fakeRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	key := strings.Join(args, " ")
	out, ok := r.outs[key]
	if !ok {
		return nil, &missingCallError{key: key}
	}
	return []byte(out), nil
}

type missingCallError struct{ key string }

func (e *missingCallError) Error() string { return "missing fake output for " + e.key }

type fakeRecorder struct {
	inputs []service.MulticaTaskFailedInput
}

func (r *fakeRecorder) RecordMulticaTaskFailure(ctx context.Context, in service.MulticaTaskFailedInput) (service.MulticaIncidentResult, error) {
	r.inputs = append(r.inputs, in)
	return service.MulticaIncidentResult{EventAccepted: true}, nil
}

func TestWatcherOmitsProfileWhenConfiguredAsDash(t *testing.T) {
	runner := &fakeRunner{outs: map[string]string{
		"--workspace-id ws-1 issue list --limit 50 --output json": `[]`,
	}}
	watcher := New(Config{Enabled: true, Command: "multica", Profile: "-", WorkspaceID: "ws-1"}, &fakeRecorder{}, WithRunner(runner))
	if _, err := watcher.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	wantCalls := [][]string{{"--workspace-id", "ws-1", "issue", "list", "--limit", "50", "--output", "json"}}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		b, _ := json.MarshalIndent(runner.calls, "", "  ")
		t.Fatalf("calls mismatch:\n%s", b)
	}
}

func TestWatcherPollOnceRecordsFailedRunsForAGSMappedIssues(t *testing.T) {
	completedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	runner := &fakeRunner{outs: map[string]string{
		"--profile p --workspace-id ws-1 issue list --limit 2 --output json": `{
		  "issues": [
		    {"id":"issue-id-1","identifier":"HUM-1","title":"Fix checkout","workspace_id":"ws-1","metadata":{"ags_repo":"octo/repo","ags_pr_url":"http://ags/pr/1"}},
		    {"id":"issue-id-2","identifier":"HUM-2","title":"No repo","metadata":{}}
		  ]
		}`,
		"--profile p --workspace-id ws-1 issue runs HUM-1 --output json": `[
		  {"id":"task-ok","status":"completed","agent_id":"agent-a"},
		  {"id":"task-failed","status":"failed","agent_id":"agent-b","agent_name":"lane-b","failure_reason":"timeout","error":"runtime timed out","runtime_provider":"codex","runtime_id":"rt-1","attempt":2,"max_attempts":3,"completed_at":"` + completedAt.Format(time.RFC3339) + `"}
		]`,
	}}
	recorder := &fakeRecorder{}
	watcher := New(Config{Enabled: true, Command: "multica", Profile: "p", Workspace: "workspace-alpha", WorkspaceID: "ws-1", IssueLimit: 2}, recorder, WithRunner(runner))
	res, err := watcher.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if res.IssuesScanned != 2 || res.IssuesSkipped != 1 || res.RunsScanned != 2 || res.FailedRuns != 1 || res.Accepted != 1 {
		t.Fatalf("unexpected result: %#v", res)
	}
	if len(recorder.inputs) != 1 {
		t.Fatalf("inputs=%d", len(recorder.inputs))
	}
	got := recorder.inputs[0]
	if got.RepoFullName != "octo/repo" || got.MulticaIssueKey != "HUM-1" || got.TaskID != "task-failed" || got.AgentName != "lane-b" || got.FailureReason != "timeout" || got.RuntimeProvider != "codex" || got.AGSPrURL != "http://ags/pr/1" {
		t.Fatalf("unexpected input: %#v", got)
	}
	wantTime := completedAt
	if !got.OccurredAt.Equal(wantTime) {
		t.Fatalf("occurred_at=%s", got.OccurredAt)
	}
	wantCalls := [][]string{
		{"--profile", "p", "--workspace-id", "ws-1", "issue", "list", "--limit", "2", "--output", "json"},
		{"--profile", "p", "--workspace-id", "ws-1", "issue", "runs", "HUM-1", "--output", "json"},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		b, _ := json.MarshalIndent(runner.calls, "", "  ")
		t.Fatalf("calls mismatch:\n%s", b)
	}
}
