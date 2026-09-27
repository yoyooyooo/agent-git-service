package graphql

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
)

func TestPendingChecksHaveNullableTimes(t *testing.T) {
	node := jobToCheckNode(db.WorkflowRunJob{Name: "unit", Status: "queued"}, db.Workflow{}, db.WorkflowRun{}, "https://ags.example.test", "team/repo")
	if node["startedAt"] != nil || node["completedAt"] != nil {
		t.Fatal("pending check manufactured an event time")
	}
	data, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	var client struct {
		StartedAt   *time.Time
		CompletedAt *time.Time
	}
	if err = json.Unmarshal(data, &client); err != nil {
		t.Fatal("official-client timestamp shape", err)
	}
	if client.StartedAt != nil || client.CompletedAt != nil {
		t.Fatal("unset times are not null")
	}
	now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	if nullableCheckTime(now) != now.Format(time.RFC3339) {
		t.Fatal("real timestamp lost")
	}
}
