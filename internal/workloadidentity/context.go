// Package workloadidentity contains the secret-free provenance shape retained
// by Access Grant transport Sessions. Workload assertion verification and
// assertion-to-Session exchange are retired.
package workloadidentity

const (
	DefaultIssuer         = "multica"
	WorkloadContextSchema = "workload.context.v1"
)

// WorkloadContext is provenance only. It never grants an AGS operation.
type WorkloadContext struct {
	Schema           string `json:"schema"`
	IssuerInstanceID string `json:"issuer_instance_id"`
	Subject          string `json:"subject"`
	CorrelationID    string `json:"correlation_id"`
	WorkspaceID      string `json:"workspace_id"`
	AgentID          string `json:"agent_id"`
	SquadID          string `json:"squad_id,omitempty"`
	IssueID          string `json:"issue_id,omitempty"`
	IssueKey         string `json:"issue_key,omitempty"`
	TaskID           string `json:"task_id"`
	RunID            string `json:"run_id"`
	TriggerID        string `json:"trigger_id,omitempty"`
	RuntimeID        string `json:"runtime_id,omitempty"`
	Role             string `json:"role,omitempty"`
}
