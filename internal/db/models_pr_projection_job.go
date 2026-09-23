package db

import "time"

// PullRequestProjectionJob tracks durable asynchronous external PR projection work.
// AGS PR rows remain authoritative; this table records projection worker phase,
// attempts, and safe error details until the final PullRequestProjection row exists.
type PullRequestProjectionJob struct {
	ID             uint        `gorm:"primaryKey;autoIncrement"`
	PullRequestID  uint        `gorm:"not null;uniqueIndex:idx_pr_projection_job_pr_provider;index"`
	PullRequest    PullRequest `gorm:"foreignKey:PullRequestID"`
	RepositoryID   uint        `gorm:"not null;index:idx_pr_projection_job_provider_phase_next,priority:2"`
	Repository     Repository  `gorm:"foreignKey:RepositoryID"`
	AgentSessionID *string     `gorm:"type:char(36);index"`
	// ActionIntentID binds an action-rebase generation to its exact durable
	// authority record. It remains nullable for historical and generic projection
	// jobs; action workers fail closed when the binding is absent.
	ActionIntentID            *string `gorm:"size:64;uniqueIndex:idx_pr_projection_job_action_intent"`
	Provider                  string  `gorm:"size:32;not null;uniqueIndex:idx_pr_projection_job_pr_provider;index:idx_pr_projection_job_provider_phase_next,priority:1"`
	Trigger                   string  `gorm:"size:64;not null;default:'pull_request_projection';index"`
	ActionGeneration          uint    `gorm:"not null;default:0"`
	CorrelationID             string  `gorm:"size:255;index"`
	RepoFullName              string  `gorm:"size:512;not null;index"`
	AGSPRNumber               int     `gorm:"column:ags_pr_number;not null;index"`
	HeadRef                   string  `gorm:"size:255;index"`
	BaseRef                   string  `gorm:"size:255;index"`
	HeadSHA                   string  `gorm:"size:40;index"`
	PreflightAGSHeadSHA       string  `gorm:"size:40;index"`
	PreflightBaseSHA          string  `gorm:"size:40;index"`
	ExpectedForgejoOldHeadSHA string  `gorm:"size:40;index"`
	DesiredAGSHeadSHA         string  `gorm:"size:40;index"`
	ObservedForgejoHeadSHA    string  `gorm:"size:40;index"`
	Phase                     string  `gorm:"size:32;not null;default:'queued';index:idx_pr_projection_job_provider_phase_next,priority:3"`
	Attempt                   int     `gorm:"not null;default:0"`
	LastErrorType             string  `gorm:"size:96"`
	LastError                 string  `gorm:"type:text"`
	ExternalRepo              string  `gorm:"size:512"`
	ExternalNumber            int
	ExternalURL               string     `gorm:"size:2048"`
	RemoteRef                 string     `gorm:"size:512"`
	RemoteSHA                 string     `gorm:"size:40"`
	NextRunAt                 *time.Time `gorm:"index:idx_pr_projection_job_provider_phase_next,priority:4"`
	StartedAt                 *time.Time
	FinishedAt                *time.Time
	SuccessCommentDispatchID  string `gorm:"size:255;index"`
	SuccessCommentClaimToken  string `gorm:"size:64"`
	SuccessCommentClaimedAt   *time.Time
	SuccessCommentedAt        *time.Time
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
	Attempts                  []PullRequestProjectionJobAttempt `gorm:"foreignKey:JobID"`
}

func (PullRequestProjectionJob) TableName() string { return "pull_request_projection_jobs" }

// PullRequestProjectionJobAttempt is the durable per-generation history for a
// projection job. Later success or retry must not delete or rewrite a prior
// attempt's error.
type PullRequestProjectionJobAttempt struct {
	ID           uint   `gorm:"primaryKey;autoIncrement"`
	JobID        uint   `gorm:"not null;uniqueIndex:idx_pr_projection_job_attempt,priority:1;index"`
	Attempt      int    `gorm:"not null;uniqueIndex:idx_pr_projection_job_attempt,priority:2"`
	Phase        string `gorm:"size:32;not null"`
	Status       string `gorm:"size:32;not null"`
	ErrorType    string `gorm:"size:96"`
	ErrorSummary string `gorm:"column:error_summary;type:text"`
	RemoteSHA    string `gorm:"size:40"`
	StartedAt    *time.Time
	FinishedAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (PullRequestProjectionJobAttempt) TableName() string {
	return "pull_request_projection_job_attempts"
}
