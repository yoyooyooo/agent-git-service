package providerlogprotocol

import "time"

const (
	StepLogsSchema       = "ags.internal-provider-step-logs.v1"
	UnattributedStepName = "AGS: output outside recorded steps"
	// Leave room for the existing binding fields within the envelope margin.
	StepLogMetadataMaxBytes = 48 << 10
)

// ByteRange addresses the decoded text in the same verified log response. End
// is exclusive. Forgejo's logical line indexes never cross this wire boundary.
type ByteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type StepLog struct {
	Name        string      `json:"name"`
	Number      int         `json:"number"`
	Status      string      `json:"status"`
	Conclusion  string      `json:"conclusion"`
	StartedAt   *time.Time  `json:"started_at"`
	CompletedAt *time.Time  `json:"completed_at"`
	Ranges      []ByteRange `json:"ranges,omitempty"`
	// "unattributed" is an AGS diagnostic bucket, not a provider execution
	// step. Its conclusion is the enclosing job's conclusion; it does not
	// assert that the unassigned output itself caused that conclusion.
	Diagnostic string `json:"diagnostic,omitempty"`
}

type StepLogs struct {
	Schema     string    `json:"schema"`
	ByteLength int64     `json:"byte_length"`
	Steps      []StepLog `json:"steps"`
}
