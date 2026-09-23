// Package operationcatalog is the single AGS owner of known collaboration
// operations and their standard/privileged risk class (AGS-T022 Stage 2).
//
// Route handlers, Git receive classification, Access Grant evaluation, and
// tests must derive risk from this catalog. Agent Kit registries and request
// environment variables are not authority for operation risk.
package operationcatalog

import (
	"sort"
	"strings"
)

// Risk is the authorization risk class for one registered operation.
type Risk string

const (
	// RiskStandard operations are available to every trusted Multica Agent on
	// an onboarded repository when the instance standard executor has the
	// required live native permission.
	RiskStandard Risk = "standard"
	// RiskPrivileged operations are reserved for explicitly supported role
	// intents and operation-specific checks.
	RiskPrivileged Risk = "privileged"
)

// AccessRoleMaintainer is the established Agent-side merge role.
const AccessRoleMaintainer = "maintainer"

// AccessRoleAdmin is accepted as the explicit admin spelling for the same
// single-purpose workload merge role. It does not authorize other operations.
const AccessRoleAdmin = "admin"

func IsPRMergeRole(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case AccessRoleMaintainer, AccessRoleAdmin:
		return true
	default:
		return false
	}
}

var catalog = map[string]Risk{
	// Standard collaboration.
	"repo.read":    RiskStandard,
	"git.read":     RiskStandard,
	"git.push":     RiskStandard,
	"pr.read":      RiskStandard,
	"pr.create":    RiskStandard,
	"pr.comment":   RiskStandard,
	"pr.edit":      RiskStandard,
	"pr.close":     RiskStandard,
	"pr.reopen":    RiskStandard,
	"pr.rebase":    RiskStandard,
	"review.read":  RiskStandard,
	"review.write": RiskStandard,
	"ci.read":      RiskStandard,

	// Privileged.
	"pr.merge":                RiskPrivileged,
	"review.dismiss":          RiskPrivileged,
	"git.force_push":          RiskPrivileged,
	"protected_ref.write":     RiskPrivileged,
	"ref.delete":              RiskPrivileged,
	"repo.create":             RiskPrivileged,
	"repo.delete":             RiskPrivileged,
	"repo.admin":              RiskPrivileged,
	"branch_protection.write": RiskPrivileged,
	"webhook.write":           RiskPrivileged,
	// Legacy name hard-cut to review.write for ordinary review submission.
	// Kept as privileged-unknown for fail-closed routing until callers migrate.
	"review.submit": RiskStandard,
}

// RiskOf returns the risk class for a known operation.
func RiskOf(operation string) (Risk, bool) {
	risk, ok := catalog[strings.TrimSpace(operation)]
	return risk, ok
}

// IsKnown reports whether operation is registered in the catalog.
func IsKnown(operation string) bool {
	_, ok := RiskOf(operation)
	return ok
}

// IsStandard reports whether operation is a standard collaboration operation.
func IsStandard(operation string) bool {
	risk, ok := RiskOf(operation)
	return ok && risk == RiskStandard
}

// IsPrivileged reports whether operation is privileged.
func IsPrivileged(operation string) bool {
	risk, ok := RiskOf(operation)
	return ok && risk == RiskPrivileged
}

// CanonicalName hard-cuts legacy aliases onto the catalog name used for
// authorization and audit. Unknown names are returned trimmed unchanged.
func CanonicalName(operation string) string {
	op := strings.TrimSpace(operation)
	switch op {
	case "review.submit":
		return "review.write"
	default:
		return op
	}
}

// StandardOperations returns the sorted standard operation set.
func StandardOperations() []string {
	return operationsWithRisk(RiskStandard)
}

// PrivilegedOperations returns the sorted privileged operation set.
func PrivilegedOperations() []string {
	return operationsWithRisk(RiskPrivileged)
}

// AllOperations returns every registered operation name, sorted.
func AllOperations() []string {
	out := make([]string, 0, len(catalog))
	for name := range catalog {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func operationsWithRisk(risk Risk) []string {
	out := make([]string, 0, len(catalog))
	for name, class := range catalog {
		if class != risk {
			continue
		}
		// Do not advertise the legacy alias in the public standard set.
		if name == "review.submit" {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
