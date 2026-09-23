package forgejointegration

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	ProjectionFailureUnknown                      = "unknown_projection_error"
	ProjectionFailureNonFastForward               = "non_fast_forward_blocked"
	ProjectionFailureProtectedBranch              = "protected_branch_blocked"
	ProjectionFailureAuthFailed                   = "auth_failed"
	ProjectionFailureRepoMissing                  = "forgejo_repo_missing"
	ProjectionFailureSHADrift                     = "sha_drift"
	ProjectionFailureSourceBranchCleanupPending   = "source_branch_cleanup_pending"
	ProjectionFailureMergedContentMissing         = "merged_content_missing"
	ProjectionFailurePullRequestProjectionMissing = "pull_request_projection_missing"
	ProjectionFailurePullRequestAuthorityMissing  = "pull_request_authority_missing"
	ProjectionFailurePullRequestStateDrift        = "pull_request_state_drift"
)

// ProjectionError is a structured, redacted Forgejo projection failure.
// It is safe to persist and expose through operator APIs.
type ProjectionError struct {
	Type         string
	Repo         string
	TargetRepo   string
	Ref          string
	Branch       string
	ExpectedSHA  string
	ActualSHA    string
	ErrorSummary string
	Cause        error
}

func (e *ProjectionError) Error() string {
	if e == nil {
		return "forgejo projection error"
	}
	msg := e.Type
	if e.Ref != "" {
		msg += " " + e.Ref
	}
	if e.ErrorSummary != "" {
		msg += ": " + e.ErrorSummary
	}
	return msg
}

func (e *ProjectionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ClassifyProjectionError turns a raw Forgejo/Git failure into a stable failure type.
func ClassifyProjectionError(ref, expectedSHA string, err error) ProjectionError {
	var structured *ProjectionError
	if errors.As(err, &structured) && structured != nil {
		pe := *structured
		if strings.TrimSpace(pe.Type) == "" {
			pe.Type = ProjectionFailureUnknown
		}
		if strings.TrimSpace(pe.Ref) == "" {
			pe.Ref = strings.TrimSpace(ref)
		}
		if strings.TrimSpace(pe.ExpectedSHA) == "" {
			pe.ExpectedSHA = strings.TrimSpace(expectedSHA)
		}
		if strings.TrimSpace(pe.ErrorSummary) == "" {
			pe.ErrorSummary = redactProjectionError(err)
		}
		if pe.Cause == nil {
			pe.Cause = err
		}
		return pe
	}
	pe := ProjectionError{
		Type:         ProjectionFailureUnknown,
		Ref:          strings.TrimSpace(ref),
		ExpectedSHA:  strings.TrimSpace(expectedSHA),
		ErrorSummary: redactProjectionError(err),
		Cause:        err,
	}
	msg := strings.ToLower(pe.ErrorSummary)
	switch {
	case strings.Contains(msg, "non-fast-forward") || strings.Contains(msg, "fetch first") || strings.Contains(msg, "updates were rejected") || strings.Contains(msg, "更新被拒绝") || (strings.Contains(msg, "cannot lock ref") && strings.Contains(msg, "reference already exists")):
		pe.Type = ProjectionFailureNonFastForward
	case strings.Contains(msg, "not allowed to push to protected branch") || strings.Contains(msg, "protected branch") || strings.Contains(msg, "pre-receive hook declined"):
		pe.Type = ProjectionFailureProtectedBranch
	case strings.Contains(msg, "authentication failed") || strings.Contains(msg, "凭据不正确") || strings.Contains(msg, "requires authentication") || strings.Contains(msg, "401"):
		pe.Type = ProjectionFailureAuthFailed
	case strings.Contains(msg, "repository not found") || strings.Contains(msg, "not found") || strings.Contains(msg, "404"):
		pe.Type = ProjectionFailureRepoMissing
	case strings.Contains(msg, "head sha mismatch") || strings.Contains(msg, "did not reach expected head"):
		pe.Type = ProjectionFailureSHADrift
	}
	return pe
}

var credentialURLPattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s'"<>]+`)

// SanitizeProjectionErrorSummary removes credential-bearing URLs and bounds a
// summary before it is persisted or included in an operator notification.
func SanitizeProjectionErrorSummary(summary string) string {
	return redactProjectionError(errors.New(summary))
}

func redactProjectionError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	msg = credentialURLPattern.ReplaceAllStringFunc(msg, func(raw string) string {
		return redactURLCredential(raw)
	})
	return truncateProjectionError(msg, 1000)
}

func redactURLCredential(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		return raw[:i+3] + "<redacted>"
	}
	return raw
}

func truncateProjectionError(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return fmt.Sprintf("%s...", s[:max-3])
}
