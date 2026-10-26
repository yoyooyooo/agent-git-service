package forgejointegration

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ngaut/agent-git-service/internal/gittransport"
)

const (
	ProjectionFailureRefLock                      = "ref_lock_contention"
	ProjectionFailureHookRejected                 = "hook_rejected"
	ProjectionFailureNetwork                      = "transport_error"
	ProjectionFailureUnknown                      = "unknown_projection_error"
	ProjectionFailureNonFastForward               = "non_fast_forward_blocked"
	ProjectionFailureProtectedBranch              = "protected_branch_blocked"
	ProjectionFailureAuthFailed                   = "auth_failed"
	ProjectionFailurePermissionDenied             = "permission_denied"
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
	Cause        error `json:"-"`
	// Diagnostic is additive; existing API/DB fields retain their shapes. It
	// is immediate evidence, not a diagnostics table or an execution permit.
	Diagnostic *gittransport.Failure `json:"diagnostic,omitempty"`
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
	f := projectionFailure(err)
	pe := ProjectionError{Type: ProjectionFailureUnknown, Ref: strings.TrimSpace(ref), ExpectedSHA: strings.TrimSpace(expectedSHA), Cause: err, Diagnostic: &f}
	switch f.Category {
	case "cas_conflict", "non_fast_forward":
		pe.Type = ProjectionFailureNonFastForward // legacy public type
	case "protection_policy":
		pe.Type = ProjectionFailureProtectedBranch
	case "http_forbidden":
		pe.Type = ProjectionFailurePermissionDenied
	case "auth_failed":
		pe.Type = ProjectionFailureAuthFailed
	case "target_missing_or_inaccessible":
		pe.Type = ProjectionFailureRepoMissing
	case "ref_lock_contention":
		pe.Type = ProjectionFailureRefLock
	case "hook_rejected":
		pe.Type = ProjectionFailureHookRejected
	case "network":
		pe.Type = ProjectionFailureNetwork
	}
	// Legacy errors have no typed evidence. Keep the known SHA/translation
	// compatibility cases, but never return their arbitrary response text.
	var transport *gittransport.TransportError
	var provider *ProviderHTTPError
	if !errors.As(err, &transport) && !errors.As(err, &provider) && err != nil && f.Category == "unknown" {
		msg := strings.ToLower(err.Error())
		switch {
		case strings.Contains(msg, "head sha mismatch"), strings.Contains(msg, "did not reach expected head"):
			pe.Type = ProjectionFailureSHADrift
		case strings.Contains(msg, "更新被拒绝"):
			pe.Type = ProjectionFailureNonFastForward
		case strings.Contains(msg, "凭据不正确"), strings.Contains(msg, "requires authentication"):
			pe.Type = ProjectionFailureAuthFailed
		}
	}
	pe.ErrorSummary = fmt.Sprintf("projection failure: %s (%s; %s)", pe.Type, f.ReasonCode, f.EvidenceSource)
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
	var transport *gittransport.TransportError
	var provider *ProviderHTTPError
	if errors.As(err, &transport) || errors.As(err, &provider) {
		f := projectionFailure(err)
		return fmt.Sprintf("projection transport failure: %s (%s; %s)", f.Category, f.ReasonCode, f.EvidenceSource)
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
	if max <= 0 {
		return ""
	}
	// Summaries are display text: replace each run of invalid UTF-8 bytes
	// before measuring the byte budget, since U+FFFD can expand the input.
	s = strings.ToValidUTF8(s, "\ufffd")
	if len(s) <= max {
		return s
	}
	end, suffix := max, ""
	if max > 3 {
		end, suffix = max-3, "..."
	}
	// s is valid UTF-8 and end < len(s); back up to a complete rune boundary.
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + suffix
}
