package routebstage0

type ProcessIdentity struct {
	PID              int    `json:"pid"`
	StartTicks       uint64 `json:"start_ticks"`
	ParentPID        int    `json:"parent_pid"`
	UID              uint32 `json:"uid"`
	GID              uint32 `json:"gid"`
	ExecutableSHA256 string `json:"executable_sha256"`
}

type Context struct {
	Schema                     string          `json:"schema"`
	RegistrationID             string          `json:"registration_id"`
	RegistrationSHA256         string          `json:"registration_sha256"`
	Root                       string          `json:"root"`
	Operation                  string          `json:"operation"`
	Phase                      string          `json:"phase"`
	InvocationID               string          `json:"invocation_id"`
	AttemptID                  string          `json:"attempt_id"`
	AuthorityEpoch             uint64          `json:"authority_epoch"`
	AuthorizationID            string          `json:"authorization_id"`
	BootID                     string          `json:"boot_id"`
	Supervisor                 ProcessIdentity `json:"supervisor"`
	Child                      ProcessIdentity `json:"child"`
	InterpreterAuthoritySHA256 string          `json:"interpreter_authority_sha256"`
	ToolRevision               string          `json:"tool_revision"`
	ToolTree                   string          `json:"tool_tree"`
	ToolManifestSHA256         string          `json:"tool_manifest_sha256"`
	RecoveryRegistrationSHA256 string          `json:"recovery_registration_sha256"`
	ScopeGrantSHA256           string          `json:"scope_grant_sha256"`
}

type RuntimeIdentity struct {
	Context     Context           `json:"context"`
	PIDs        []ProcessIdentity `json:"pids"`
	ManagerUnit string            `json:"manager_unit"`
}

type ExitRecord struct {
	Schema             string          `json:"schema"`
	RegistrationSHA256 string          `json:"registration_sha256"`
	InvocationID       string          `json:"invocation_id"`
	Phase              string          `json:"phase"`
	BootID             string          `json:"boot_id"`
	Supervisor         ProcessIdentity `json:"supervisor"`
	Child              ProcessIdentity `json:"child"`
	ChildExitCode      int             `json:"child_exit_code"`
	SignalForwarded    bool            `json:"signal_forwarded"`
	ForcedKill         bool            `json:"forced_kill"`
	ContextVerified    bool            `json:"context_verified"`
	AuthorityRechecked bool            `json:"authority_rechecked"`
	NormalExit         bool            `json:"normal_exit"`
}

type Request struct {
	Schema       string `json:"schema"`
	Method       string `json:"method"`
	InvocationID string `json:"invocation_id,omitempty"`
	Generation   string `json:"generation,omitempty"`
	ResultSHA256 string `json:"result_sha256,omitempty"`
}

type Response struct {
	Schema            string             `json:"schema"`
	OK                bool               `json:"ok"`
	Error             string             `json:"error,omitempty"`
	Context           *Context           `json:"context,omitempty"`
	Runtime           *RuntimeIdentity   `json:"runtime,omitempty"`
	Exit              *ExitRecord        `json:"exit,omitempty"`
	ManagerCompletion *ManagerCompletion `json:"manager_completion,omitempty"`
	Action            string             `json:"action,omitempty"`
	Writers           *WriterResult      `json:"writers,omitempty"`
	Restore           *RestoreCompletion `json:"restore,omitempty"`
}

type Writer struct {
	Kind   string `json:"kind"`
	Object string `json:"object,omitempty"`
	Count  int    `json:"count"`
}
type WriterResult struct {
	Generation string   `json:"generation"`
	Closed     bool     `json:"closed"`
	Writers    []Writer `json:"writers"`
	Unknown    []string `json:"unknown"`
}
