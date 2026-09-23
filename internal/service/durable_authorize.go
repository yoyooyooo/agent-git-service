package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/operationconstraints"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

// DurableOperationAuthorizationResult is the response for a successful durable
// operation authorization check, matching the ags-cli DurableOperationAuthorizationResult.
type DurableOperationAuthorizationResult struct {
	Authorized         bool                                   `json:"authorized"`
	PrincipalID        uint                                   `json:"principal_id"`
	ContractRevision   string                                 `json:"contract_revision"`
	Resource           DurableOperationAuthorizationResource  `json:"resource"`
	Operation          DurableOperationAuthorizationOperation `json:"operation"`
	AuthorizationBasis DurableOperationAuthorizationBasis     `json:"authorization_basis"`
}

type DurableOperationAuthorizationResource struct {
	Service    string `json:"service"`
	Repository string `json:"repository"`
}

type DurableOperationAuthorizationOperation struct {
	Name        string         `json:"name"`
	Constraints map[string]any `json:"constraints"`
}

type DurableOperationAuthorizationBasis struct {
	NativeGrantRevision    string `json:"native_grant_revision"`
	ResourcePolicyRevision string `json:"resource_policy_revision"`
	PolicyClass            string `json:"policy_class,omitempty"`
	PolicyClassRevision    string `json:"policy_class_revision,omitempty"`

	// Canonical team identity is an internal durable-workflow snapshot. The
	// generic authorization receipt does not expose these target-local fields.
	TeamIdentityID      string `json:"-"`
	MembershipEpoch     int64  `json:"-"`
	TeamBindingRevision string `json:"-"`
}

var (
	durableOperationConstraintKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

	// Constraints are receipt scope, not arbitrary caller metadata. Each
	// operation admits only keys whose meaning is implemented by its client
	// contract; every other key fails before it can be serialized.
	durableOperationConstraintKeys = map[string]map[string]struct{}{
		// Default-operation schemas are owned by operationconstraints. Only the
		// four explicitly deferred operations remain on this compatibility map.
		"pr.merge": {
			"pull_request_number": {}, "forgejo_pull_request_number": {}, "merge_method": {}, "expected_head_sha": {},
		},
		"review.submit": {
			"pull_request_number": {}, "forgejo_pull_request_number": {}, "exact_head": {},
		},
	}
)

const nativeHumanAuthorizationRevision = "native-human-repository-permission-v1"

// AuthorizeDurableOperation authorizes an already authenticated durable
// caller. Human operators are governed by their current native repository
// permission; they do not need a PrincipalSession binding or resource policy.
// Durable agents retain the team-authority-v4 evaluator, while dynamic agents
// use the separate Access Grant path. The principal-session-v2 revision is
// accepted only by the explicit legacy input compatibility path.
func (s *Service) AuthorizeDurableOperation(
	ctx context.Context,
	viewer db.User,
	serviceName, repository, operationName string,
	operationConstraints map[string]any,
) (DurableOperationAuthorizationResult, error) {
	cleanService := strings.ToLower(strings.TrimSpace(serviceName))
	cleanRepo := strings.Trim(strings.TrimSpace(repository), "/")
	cleanOperation := strings.ToLower(strings.TrimSpace(operationName))
	if sessionauthority.IsSecretShapedValue(cleanService) || sessionauthority.IsSecretShapedValue(cleanRepo) || sessionauthority.IsSecretShapedValue(cleanOperation) {
		return DurableOperationAuthorizationResult{}, fmt.Errorf("%w: requested authority scope is invalid", ErrValidation)
	}
	if cleanService != "ags" || cleanRepo == "" || strings.Count(cleanRepo, "/") != 1 || cleanOperation == "" {
		return DurableOperationAuthorizationResult{}, fmt.Errorf("%w: requested authority scope is invalid", ErrValidation)
	}
	constraints, err := normalizeDurableOperationConstraints(cleanOperation, operationConstraints)
	if err != nil {
		return DurableOperationAuthorizationResult{}, err
	}

	// Resolve the DB resource before evaluating policy so a genuinely absent
	// repository retains the API's existing not-found contract.
	var repo db.Repository
	if err := s.DBForCtx(ctx).Where("full_name = ?", cleanRepo).First(&repo).Error; err != nil {
		return DurableOperationAuthorizationResult{},
			fmt.Errorf("%w: requested repository is unavailable", ErrNotFound)
	}
	if repo.Disabled {
		return DurableOperationAuthorizationResult{},
			fmt.Errorf("%w: requested repository is unavailable", ErrForbidden)
	}

	operation, supported := sessionauthority.LookupOperation(cleanOperation)
	if !supported {
		return DurableOperationAuthorizationResult{},
			fmt.Errorf("%w: requested operation is unsupported", ErrValidation)
	}
	permission, err := s.currentRepoAccess(ctx, repo.ID, viewer.ID)
	if err != nil {
		return DurableOperationAuthorizationResult{}, err
	}
	requiredPermission := repoPermissionForSessionAuthority(operation.RequiredPermission)
	if !permission.AtLeast(requiredPermission) {
		return DurableOperationAuthorizationResult{},
			fmt.Errorf("%w: principal does not have sufficient permission for requested operation", ErrForbidden)
	}

	// A machine credential identifies the human; native AGS repository access
	// is the complete authorization fact. Association/profile policy is not a
	// second permission system and must not block an otherwise valid operation.
	if viewer.Type == db.TypeUser && viewer.UserKind == db.UserKindHuman && isUserStatusActive(viewer.Status) {
		return DurableOperationAuthorizationResult{
			Authorized:       true,
			PrincipalID:      viewer.ID,
			ContractRevision: sessionauthority.ContractRevision,
			Resource: DurableOperationAuthorizationResource{
				Service:    cleanService,
				Repository: cleanRepo,
			},
			Operation: DurableOperationAuthorizationOperation{
				Name:        operation.Name,
				Constraints: constraints,
			},
			AuthorizationBasis: DurableOperationAuthorizationBasis{
				NativeGrantRevision:    nativeGrantRevision(viewer.ID, repo.ID, permission),
				ResourcePolicyRevision: nativeHumanAuthorizationRevision,
			},
		}, nil
	}

	if s.PrincipalSessions == nil {
		return DurableOperationAuthorizationResult{}, fmt.Errorf("%w: principal session authority is not configured", ErrForbidden)
	}
	principalAuthority, err := s.PrincipalSessions.DurablePrincipalAuthority(viewer.ID)
	if err != nil {
		return DurableOperationAuthorizationResult{}, fmt.Errorf("%w: immutable principal policy binding is unavailable", ErrForbidden)
	}
	policyClass := principalAuthority.PolicyClass.ID
	scope, err := s.PrincipalSessions.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
		Service: cleanService, Repository: cleanRepo, Operation: cleanOperation, PolicyClass: policyClass,
	})
	if errors.Is(err, sessionauthority.ErrOperationNotSupported) {
		return DurableOperationAuthorizationResult{},
			fmt.Errorf("%w: requested operation is unsupported", ErrValidation)
	}
	if err != nil {
		return DurableOperationAuthorizationResult{},
			fmt.Errorf("%w: resource policy does not authorize the requested operation", ErrForbidden)
	}

	return DurableOperationAuthorizationResult{
		Authorized:       true,
		PrincipalID:      viewer.ID,
		ContractRevision: sessionauthority.ContractRevision,
		Resource: DurableOperationAuthorizationResource{
			Service:    scope.Resource.Service,
			Repository: scope.Resource.Repository,
		},
		Operation: DurableOperationAuthorizationOperation{
			Name:        scope.Operation.Name,
			Constraints: constraints,
		},
		AuthorizationBasis: DurableOperationAuthorizationBasis{
			NativeGrantRevision: nativeGrantRevision(viewer.ID, repo.ID, permission), ResourcePolicyRevision: scope.Resource.PolicyRevision,
			PolicyClass: policyClass, PolicyClassRevision: principalAuthority.PolicyClass.PolicyRevision,
			TeamIdentityID: principalAuthority.TeamBinding.TeamIdentityID, MembershipEpoch: principalAuthority.TeamBinding.EpochFloor,
			TeamBindingRevision: principalAuthority.TeamBinding.BindingRevision,
		},
	}, nil
}

// normalizeDurableOperationConstraints is the service authority boundary for
// caller-provided receipt fields. It accepts the scalar JSON shape that the
// durable authorization protocol supports and rejects credentials before they
// can reach a serialized receipt, audit, or error path.
func normalizeDurableOperationConstraints(operation string, input map[string]any) (map[string]any, error) {
	if operationconstraints.IsDefaultOperation(operation) {
		normalized, err := operationconstraints.NormalizeJSON(operation, input)
		if err != nil {
			return nil, fmt.Errorf("%w: operation constraints are invalid", ErrValidation)
		}
		out, err := operationconstraints.ToJSON(operation, normalized)
		if err != nil {
			return nil, fmt.Errorf("%w: operation constraints are invalid", ErrValidation)
		}
		return out, nil
	}

	out := make(map[string]any, len(input))
	allowedKeys := durableOperationConstraintKeys[operation]
	for key, value := range input {
		if !durableOperationConstraintKeyRE.MatchString(key) {
			return nil, fmt.Errorf("%w: operation constraints are invalid", ErrValidation)
		}
		if _, allowed := allowedKeys[key]; !allowed {
			return nil, fmt.Errorf("%w: operation constraints are invalid", ErrValidation)
		}
		switch typed := value.(type) {
		case string:
			typed = strings.TrimSpace(typed)
			if typed == "" || strings.ContainsAny(typed, "\r\n\x00") || sessionauthority.IsSecretShapedValue(typed) {
				return nil, fmt.Errorf("%w: operation constraints are invalid", ErrValidation)
			}
			out[key] = typed
		case json.Number, bool:
			out[key] = typed
		default:
			return nil, fmt.Errorf("%w: operation constraints are invalid", ErrValidation)
		}
	}
	if operation == "pr.merge" {
		if len(out) != 4 || !positiveConstraintNumber(out["pull_request_number"]) ||
			!positiveConstraintNumber(out["forgejo_pull_request_number"]) ||
			!canonicalMergeMethod(out["merge_method"]) || !canonicalActionSHA(out["expected_head_sha"]) {
			return nil, fmt.Errorf("%w: operation constraints are invalid", ErrValidation)
		}
	}
	return out, nil
}

const maxActionPullRequestNumber int64 = operationconstraints.MaxJSONSafePositiveInteger

func positiveConstraintNumber(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	return operationconstraints.IsJSONSafePositiveInteger(number.String())
}

func canonicalActionSHA(value any) bool {
	text, ok := value.(string)
	return ok && operationconstraints.IsCanonicalSHA(text)
}

func canonicalMergeMethod(value any) bool {
	method, ok := value.(string)
	if !ok {
		return false
	}
	switch method {
	case "merge", "rebase", "rebase-merge", "squash", "fast-forward-only":
		return true
	default:
		return false
	}
}

func durablePolicyClassRevision(authority *sessionauthority.Set, policyClass string) string {
	if policyClass == "" {
		return ""
	}
	for _, class := range authority.Config().PolicyClasses {
		if class.ID == policyClass {
			return class.PolicyRevision
		}
	}
	return ""
}
