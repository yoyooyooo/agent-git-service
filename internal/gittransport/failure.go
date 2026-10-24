package gittransport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Failure is the allowlisted evidence, never the provider response. A pattern
// mentioning 403 is not a measured HTTP status. RetryAdvice grants no replay.
type Failure struct {
	Category        string `json:"category"`
	ReasonCode      string `json:"reason_code"`
	EvidenceSource  string `json:"evidence_source"`
	HTTPStatus      *int   `json:"http_status"`
	ExitCode        *int   `json:"exit_code"`
	StderrTruncated bool   `json:"stderr_truncated"`
	RetryAdvice     string `json:"retry_advice"`
}

// TransportError retains only safe evidence and context identity. In particular
// neither a url.Error nor a PathError (which may embed secrets) is retained.
type TransportError struct {
	failure Failure
	cause   error
}

func (e *TransportError) Error() string    { return "provider Git " + failureSummary(e.failure) }
func (e *TransportError) Unwrap() error    { return e.cause }
func (e *TransportError) Failure() Failure { return e.failure }

func failure(category, reason, source string) Failure {
	return Failure{Category: category, ReasonCode: reason, EvidenceSource: source, RetryAdvice: "inspect_before_new_plan"}
}

// In priority order: specific reasons must win over generic rejection text.
var failurePatterns = [...]struct{ text, category, reason string }{
	{"stale info", "cas_conflict", "expected_old_mismatch"},
	{"but expected", "cas_conflict", "expected_old_mismatch"},
	{"reference already exists", "cas_conflict", "expected_absent_mismatch"},
	{"protected branch", "protection_policy", "protected_branch"},
	{"protected_branch", "protection_policy", "protected_branch"},
	{"403", "http_forbidden", "http_403_pattern"},
	{"forbidden", "http_forbidden", "forbidden_pattern"},
	{".lock': file exists", "ref_lock_contention", "ref_lock_exists"},
	{".lock: file exists", "ref_lock_contention", "ref_lock_exists"},
	{"unable to create", "unknown", "unknown"}, // paired with .lock below
	{"authentication failed", "auth_failed", "authentication_failed"},
	{"401", "auth_failed", "http_401_pattern"},
	{"repository not found", "target_missing_or_inaccessible", "repository_not_found"},
	{"non-fast-forward", "non_fast_forward", "non_fast_forward"},
	{"fetch first", "non_fast_forward", "fetch_first"},
	{"pre-receive hook declined", "hook_rejected", "hook_declined"},
	{"update hook declined", "hook_rejected", "hook_declined"},
	{"could not resolve host", "network", "dns_failed"},
	{"connection refused", "network", "connect_failed"},
	{"connection reset", "network", "connection_reset"},
	{"certificate problem", "network", "tls_failed"},
	{"tls handshake", "network", "tls_failed"},
	{"timed out", "network", "timeout"},
	{"redirect", "network", "redirect_refused"},
}

// scanFailure records fixed-pattern hits even beyond the retained output cap.
// The small overlap supports patterns split across Write calls. Raw bytes never
// leave the bounded process-local buffers or become a digest/log field.
type failureScan struct {
	hits [len(failurePatterns)]bool
	tail string
	lock bool
}

func (s *failureScan) Write(p []byte) {
	text := s.tail + strings.ToLower(string(p))
	for i, pattern := range failurePatterns {
		s.hits[i] = s.hits[i] || strings.Contains(text, pattern.text)
	}
	s.lock = s.lock || strings.Contains(text, "unable to create") && strings.Contains(text, ".lock")
	if len(text) > 256 {
		text = text[len(text)-256:]
	}
	s.tail = text
}
func (s *failureScan) Failure() Failure {
	for i, pattern := range failurePatterns {
		if s.hits[i] && pattern.category != "unknown" {
			return failure(pattern.category, pattern.reason, "stderr_pattern")
		}
		if pattern.category == "unknown" && s.lock {
			return failure("ref_lock_contention", "ref_lock_exists", "stderr_pattern")
		}
	}
	return failure("unknown", "response_withheld", "none")
}

// Describe prefers typed transport evidence. The text fallback is for legacy
// in-memory errors only and returns fixed enums, not sanitized arbitrary text.
func Describe(err error) Failure {
	var typed *TransportError
	if errors.As(err, &typed) {
		return typed.Failure()
	}
	if errors.Is(err, context.Canceled) {
		return failure("network", "context_canceled", "context")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return failure("network", "context_deadline", "context")
	}
	var network net.Error
	if errors.As(err, &network) {
		return failure("network", "transport_failed", "net_error")
	}
	var scan failureScan
	if err != nil {
		scan.Write([]byte(err.Error()))
	}
	return scan.Failure()
}

// SafeError does not preserve arbitrary cause strings; errors.Is(context...) is
// deliberately preserved through any number of caller wraps.
func SafeError(err error) *TransportError {
	f := Describe(err)
	var cause error
	if errors.Is(err, context.Canceled) {
		cause = context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		cause = context.DeadlineExceeded
	}
	return &TransportError{failure: f, cause: cause}
}
func failureSummary(f Failure) string {
	switch f.Category {
	case "protection_policy":
		return "protected branch rejected"
	case "http_forbidden":
		return "403 forbidden"
	case "non_fast_forward":
		return "non-fast-forward"
	case "auth_failed":
		return "authentication failed"
	case "target_missing_or_inaccessible":
		return "repository missing or inaccessible"
	case "unknown":
		return "operation failed (response withheld)"
	}
	return fmt.Sprintf("%s (%s)", f.Category, f.ReasonCode)
}
