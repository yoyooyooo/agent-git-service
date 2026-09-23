// Package operationconstraints owns the canonical constraint schemas shared by
// Access Grant authorization/effects, durable authorization, persisted transport
// Sessions, and use-time checks.
package operationconstraints

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

const MaxJSONSafePositiveInteger int64 = 9007199254740991

var (
	ErrInvalid   = errors.New("operation constraints are invalid")
	shaRE        = regexp.MustCompile(`^[a-f0-9]{40}$`)
	sha256RE     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	repositoryRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,99})/[A-Za-z0-9](?:[A-Za-z0-9._-]{0,99})$`)
)

// IsDefaultOperation reports whether operation has an implemented canonical
// operation constraint contract. Other registered operations remain deferred.
func IsDefaultOperation(operation string) bool {
	switch strings.TrimSpace(operation) {
	case "repo.read", "repo.create", "repo.delete", "repo.admin",
		"git.read", "git.push", "git.force_push", "protected_ref.write", "ref.delete",
		"pr.create", "pr.comment", "pr.edit", "pr.close", "pr.reopen", "pr.rebase", "pr.read", "pr.merge",
		"review.read", "review.write", "review.submit", "review.dismiss",
		"ci.read", "branch_protection.write", "webhook.write":
		return true
	default:
		return false
	}
}

// NormalizeJSON validates a JSON-shaped constraint object and returns the
// canonical persisted string representation. Pull-request numbers must be JSON
// numbers here; accepting their string form is reserved for persisted rows and
// concrete use-time facts.
func NormalizeJSON(operation string, input map[string]any) (map[string]string, error) {
	values := make(map[string]string, len(input))
	for key, raw := range input {
		if sessionauthority.IsSecretShapedValue(key) {
			return nil, ErrInvalid
		}
		switch value := raw.(type) {
		case string:
			if isNumberConstraintKey(key) || !canonicalScalar(value) {
				return nil, ErrInvalid
			}
			values[key] = value
		case json.Number:
			number, ok := positiveJSONNumber(value.String())
			if !ok {
				return nil, ErrInvalid
			}
			values[key] = strconv.FormatInt(number, 10)
		case float64:
			if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value || value <= 0 || value > float64(MaxJSONSafePositiveInteger) {
				return nil, ErrInvalid
			}
			values[key] = strconv.FormatInt(int64(value), 10)
		case int:
			if value <= 0 || int64(value) > MaxJSONSafePositiveInteger {
				return nil, ErrInvalid
			}
			values[key] = strconv.FormatInt(int64(value), 10)
		case int64:
			if value <= 0 || value > MaxJSONSafePositiveInteger {
				return nil, ErrInvalid
			}
			values[key] = strconv.FormatInt(value, 10)
		default:
			return nil, ErrInvalid
		}
	}
	return normalize(operation, values)
}

// NormalizeStored validates the string representation persisted on a Session
// or built from concrete endpoint facts.
func NormalizeStored(operation string, input map[string]string) (map[string]string, error) {
	values := make(map[string]string, len(input))
	for key, value := range input {
		if sessionauthority.IsSecretShapedValue(key) || !canonicalScalar(value) {
			return nil, ErrInvalid
		}
		values[key] = value
	}
	return normalize(operation, values)
}

// Match validates both persisted and actual facts through the same schema and
// then compares them. Branch refs compare by one shared refs/heads canonical
// form while receipts preserve the caller's original canonical spelling.
func Match(operation string, expected, actual map[string]string) bool {
	left, err := NormalizeStored(operation, expected)
	if err != nil {
		return false
	}
	right, err := NormalizeStored(operation, actual)
	if err != nil || len(left) != len(right) {
		return false
	}
	for key, expectedValue := range left {
		actualValue, ok := right[key]
		if !ok {
			return false
		}
		if key == "base_ref" || key == "head_ref" {
			if canonicalBranchRef(expectedValue) != canonicalBranchRef(actualValue) {
				return false
			}
			continue
		}
		if expectedValue != actualValue {
			return false
		}
	}
	return true
}

// ToJSON restores JSON numbers for receipt responses while preserving refs and
// SHAs as strings.
func ToJSON(operation string, normalized map[string]string) (map[string]any, error) {
	values, err := NormalizeStored(operation, normalized)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(values))
	for key, value := range values {
		if isNumberConstraintKey(key) {
			out[key] = json.Number(value)
		} else {
			out[key] = value
		}
	}
	return out, nil
}

func normalize(operation string, values map[string]string) (map[string]string, error) {
	operation = strings.TrimSpace(operation)
	requireKeys := func(keys ...string) bool {
		if len(values) != len(keys) {
			return false
		}
		for _, key := range keys {
			if _, ok := values[key]; !ok {
				return false
			}
		}
		return true
	}
	requireNumber := func(key string) bool {
		number, ok := positiveJSONNumber(values[key])
		if ok {
			values[key] = strconv.FormatInt(number, 10)
		}
		return ok
	}
	requireRef := func(key string) bool { return canonicalBranchRef(values[key]) != "" }
	requireSHA := func(key string) bool { return shaRE.MatchString(values[key]) }
	requireSHA256 := func(key string) bool { return sha256RE.MatchString(values[key]) }
	requireRepository := func(key string) bool { return repositoryRE.MatchString(values[key]) }

	valid := false
	switch operation {
	case "repo.read", "git.read", "git.push", "git.force_push", "protected_ref.write", "ref.delete",
		"repo.delete", "branch_protection.write", "webhook.write":
		valid = len(values) == 0
	case "repo.create":
		valid = requireKeys("target_repository", "base_ref", "source_base_sha", "source_ref_digest", "import_mode", "visibility") &&
			requireRepository("target_repository") && requireRef("base_ref") && requireSHA("source_base_sha") &&
			requireSHA256("source_ref_digest") && (values["import_mode"] == "ags-only" || values["import_mode"] == "ags-forgejo") &&
			(values["visibility"] == "private" || values["visibility"] == "public" || values["visibility"] == "internal")
	case "repo.admin":
		valid = requireKeys("target_repository", "base_ref", "source_base_sha", "source_ref_digest", "action") &&
			requireRepository("target_repository") && requireRef("base_ref") && requireSHA("source_base_sha") &&
			requireSHA256("source_ref_digest") && values["action"] == "forgejo_onboard"
	case "pr.create":
		valid = requireKeys("base_ref", "head_ref") && requireRef("base_ref") && requireRef("head_ref")
	case "pr.comment", "pr.edit", "pr.close", "pr.reopen":
		valid = requireKeys("pull_request_number") && requireNumber("pull_request_number")
	case "pr.rebase":
		valid = requireKeys("pull_request_number", "forgejo_pull_request_number", "expected_head_sha", "expected_base_sha") &&
			requireNumber("pull_request_number") && requireNumber("forgejo_pull_request_number") &&
			requireSHA("expected_head_sha") && requireSHA("expected_base_sha")
	case "pr.read":
		valid = len(values) == 0 ||
			(requireKeys("pull_request_number") && requireNumber("pull_request_number")) ||
			(requireKeys("head_ref") && requireRef("head_ref"))
	case "pr.merge":
		valid = requireKeys("pull_request_number", "forgejo_pull_request_number", "expected_head_sha", "merge_method") &&
			requireNumber("pull_request_number") && requireNumber("forgejo_pull_request_number") &&
			requireSHA("expected_head_sha") && canonicalMergeMethod(values["merge_method"])
	case "review.read", "review.dismiss":
		valid = requireKeys("pull_request_number", "forgejo_pull_request_number") &&
			requireNumber("pull_request_number") && requireNumber("forgejo_pull_request_number")
	case "review.write", "review.submit":
		// Ordinary gh pr review may only know the AGS PR number. Provider numbers
		// remain optional and are filled by the engine/server when available.
		switch {
		case requireKeys("pull_request_number", "review_action"):
			valid = requireNumber("pull_request_number") &&
				(values["review_action"] == "approve" || values["review_action"] == "request_changes" || values["review_action"] == "comment")
		case requireKeys("pull_request_number", "forgejo_pull_request_number", "review_action"):
			valid = requireNumber("pull_request_number") && requireNumber("forgejo_pull_request_number") &&
				(values["review_action"] == "approve" || values["review_action"] == "request_changes" || values["review_action"] == "comment")
		}
	case "ci.read":
		switch {
		case len(values) == 0:
			valid = true
		case requireKeys("run_id"):
			valid = requireNumber("run_id")
		case requireKeys("pull_request_number", "forgejo_pull_request_number"):
			valid = requireNumber("pull_request_number") && requireNumber("forgejo_pull_request_number")
		case requireKeys("pull_request_number", "forgejo_pull_request_number", "head_sha"):
			valid = requireNumber("pull_request_number") && requireNumber("forgejo_pull_request_number") &&
				requireSHA("head_sha") && values["head_sha"] == strings.ToLower(values["head_sha"])
		}
	default:
		return nil, ErrInvalid
	}
	if !valid {
		return nil, ErrInvalid
	}
	return values, nil
}

func canonicalMergeMethod(value string) bool {
	switch value {
	case "merge", "rebase", "rebase-merge", "squash", "fast-forward-only":
		return true
	default:
		return false
	}
}

func canonicalScalar(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 2048 &&
		!strings.ContainsAny(value, "\r\n\x00") && !sessionauthority.IsSecretShapedValue(value)
}

// IsJSONSafePositiveInteger reports whether raw is the canonical decimal form
// accepted by every default-operation integer consumer.
func IsJSONSafePositiveInteger(raw string) bool {
	_, ok := positiveJSONNumber(raw)
	return ok
}

// IsCanonicalSHA reports whether raw is a full lowercase Git object ID used by
// the default operation contracts.
func IsCanonicalSHA(raw string) bool { return shaRE.MatchString(raw) }

func positiveJSONNumber(raw string) (int64, bool) {
	if raw == "" || (len(raw) > 1 && raw[0] == '0') {
		return 0, false
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	return value, err == nil && value > 0 && value <= MaxJSONSafePositiveInteger
}

func isNumberConstraintKey(key string) bool {
	return key == "run_id" || key == "pull_request_number" || key == "forgejo_pull_request_number"
}

func canonicalBranchRef(raw string) string {
	if raw == "" || raw != strings.TrimSpace(raw) || strings.HasPrefix(raw, "/") || strings.HasSuffix(raw, "/") {
		return ""
	}
	branch := raw
	if strings.HasPrefix(branch, "refs/") {
		if !strings.HasPrefix(branch, "refs/heads/") {
			return ""
		}
		branch = strings.TrimPrefix(branch, "refs/heads/")
	}
	if branch == "" || branch == "@" || strings.HasPrefix(branch, ".") || strings.HasSuffix(branch, ".") ||
		strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.Contains(branch, "@{") ||
		strings.ContainsAny(branch, " \\~^:?*[") {
		return ""
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return ""
		}
		for _, char := range part {
			if char < 0x20 || char == 0x7f {
				return ""
			}
		}
	}
	return "refs/heads/" + branch
}
