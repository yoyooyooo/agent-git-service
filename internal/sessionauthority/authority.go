// Package sessionauthority owns the secret-free principal/session authority
// snapshot. Canonical team-v4 identity is selected by verified issuer/key plus
// workspace/team/policy-class/epoch and one exact resource/operation scope;
// subject is provenance. Typed subject binding is executable only under the
// explicit legacy rollback contract.
package sessionauthority

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	CurrentVersion                 = 2
	ContractRevision               = "2026-07-24.team-authority-v4"
	LegacyContractRevision         = "2026-07-19.principal-session-v2"
	LegacySubjectCompatibilityMode = "legacy-subject-v2"
	MaximumSessionTTL              = 2 * time.Hour

	// DefaultDynamicPolicyClass is the bounded authority class for dynamically
	// attested workspace members. Higher-privilege operations require a
	// separately named, explicitly reviewed immutable-principal policy class.
	DefaultDynamicPolicyClass = "multica.workspace.default.v1"
)

var (
	ErrAuthorityNotConfigured = errors.New("principal session authority is not configured")
	ErrIssuerNotFound         = errors.New("trusted issuer instance not found")
	ErrIssuerInactive         = errors.New("trusted issuer instance is inactive")
	ErrBindingNotFound        = errors.New("principal binding not found")
	ErrBindingInactive        = errors.New("principal binding is inactive")
	ErrResourceNotFound       = errors.New("session resource policy not found")
	ErrResourceInactive       = errors.New("session resource policy is inactive")
	ErrResourceAmbiguous      = errors.New("session resource policy is ambiguous")
	ErrOperationNotSupported  = errors.New("session operation is not supported")
	ErrPolicyClassNotFound    = errors.New("team policy class not found")
	ErrPolicyClassInactive    = errors.New("team policy class is inactive")
	ErrTeamBindingNotFound    = errors.New("team authority binding not found")
	ErrTeamBindingInactive    = errors.New("team authority binding is inactive")
	ErrTeamBindingAmbiguous   = errors.New("team authority binding is ambiguous")
)

type Permission string

const (
	PermissionRead  Permission = "read"
	PermissionWrite Permission = "write"
	PermissionAdmin Permission = "admin"
)

// Config is projected under integrations.yaml's top-level team_authority
// key (AGS-T022 Stage 5; principal_sessions is retired). Legacy delegation v1/v2 is deliberately not part of this model.
type Config struct {
	Version          int             `yaml:"version" json:"version"`
	ContractRevision string          `yaml:"contract_revision" json:"contract_revision"`
	TrustedIssuers   []TrustedIssuer `yaml:"trusted_issuers,omitempty" json:"trusted_issuers,omitempty"`
	// Bindings is the typed legacy subject-binding compatibility surface. A
	// canonical team authority configuration has no entries here.
	LegacyCompatibilityMode string             `yaml:"legacy_compatibility_mode,omitempty" json:"legacy_compatibility_mode,omitempty"`
	Bindings                []PrincipalBinding `yaml:"bindings,omitempty" json:"bindings,omitempty"`
	TeamBindings            []TeamBinding      `yaml:"team_bindings,omitempty" json:"team_bindings,omitempty"`
	PolicyClasses           []PolicyClass      `yaml:"policy_classes,omitempty" json:"policy_classes,omitempty"`
	// ResourceDefaults are target/service-level ceilings for repositories that
	// already exist in AGS and are authorized by the resolved principal's
	// native grant. They are not wildcard grants. Exact Resources remain
	// optional exception overrides and can explicitly disable one repository.
	ResourceDefaults []ResourceDefault `yaml:"resource_defaults,omitempty" json:"resource_defaults,omitempty"`
	Resources        []ResourcePolicy  `yaml:"resources,omitempty" json:"resources,omitempty"`
}

// TrustedIssuer names one concrete assertion issuer instance. Multiple
// instances may share a provider kind, but each ID is independently disabled.
type TrustedIssuer struct {
	ID            string   `yaml:"id" json:"id"`
	Issuer        string   `yaml:"issuer" json:"issuer"`
	KeyIDs        []string `yaml:"key_ids" json:"key_ids"`
	Status        string   `yaml:"status" json:"status"`
	TrustRevision string   `yaml:"trust_revision" json:"trust_revision"`
}

// PrincipalBinding maps one immutable issuer-local subject to one immutable
// AGS principal ID. It contains no repository, operation, role, or task facts.
type PrincipalBinding struct {
	ID               string `yaml:"id" json:"id"`
	IssuerInstanceID string `yaml:"issuer_instance_id" json:"issuer_instance_id"`
	Subject          string `yaml:"subject" json:"subject"`
	PrincipalID      uint   `yaml:"principal_id" json:"principal_id"`
	Status           string `yaml:"status" json:"status"`
	BindingRevision  string `yaml:"binding_revision" json:"binding_revision"`
	ExpiresAt        string `yaml:"expires_at,omitempty" json:"expires_at,omitempty"`
	RevokedAt        string `yaml:"revoked_at,omitempty" json:"revoked_at,omitempty"`
	expiresAt        *time.Time
}

// ResourcePolicy contributes only exact target/resource state and the maximum
// credential lifetime. Principal native grants decide which operations are
// authorized; this model intentionally has no capability allowlist.
// TeamBinding is keyed exclusively by the trusted issuer instance, immutable
// team identity, and issuer-assigned policy class. Its WorkspaceID is the
// target-local immutable workspace binding that a canonical assertion must
// match exactly. Dynamic agents and squads are assertion provenance only and
// can never select this binding.
type TeamBinding struct {
	ID               string `yaml:"id" json:"id"`
	IssuerInstanceID string `yaml:"issuer_instance_id" json:"issuer_instance_id"`
	WorkspaceID      string `yaml:"workspace_id" json:"workspace_id"`
	TeamIdentityID   string `yaml:"team_identity_id" json:"team_identity_id"`
	PolicyClass      string `yaml:"policy_class" json:"policy_class"`
	PrincipalID      uint   `yaml:"principal_id" json:"principal_id"`
	Status           string `yaml:"status" json:"status"`
	BindingRevision  string `yaml:"binding_revision" json:"binding_revision"`
	EpochFloor       int64  `yaml:"epoch_floor" json:"epoch_floor"`
}

// PolicyClass is an immutable exact operation set. It narrows, never widens,
// the native grant and resource policy evaluator.
type PolicyClass struct {
	ID             string   `yaml:"id" json:"id"`
	Status         string   `yaml:"status" json:"status"`
	PolicyRevision string   `yaml:"policy_revision" json:"policy_revision"`
	Operations     []string `yaml:"operations" json:"operations"`
}

type ResourceDefault struct {
	ID             string `yaml:"id" json:"id"`
	Target         string `yaml:"target" json:"target"`
	Service        string `yaml:"service" json:"service"`
	Status         string `yaml:"status" json:"status"`
	MaxSessionTTL  string `yaml:"max_session_ttl" json:"max_session_ttl"`
	PolicyRevision string `yaml:"policy_revision" json:"policy_revision"`
	maxSessionTTL  time.Duration
}

type ResourcePolicy struct {
	ID             string `yaml:"id" json:"id"`
	Target         string `yaml:"target" json:"target"`
	Service        string `yaml:"service" json:"service"`
	Repository     string `yaml:"repository" json:"repository"`
	Status         string `yaml:"status" json:"status"`
	MaxSessionTTL  string `yaml:"max_session_ttl" json:"max_session_ttl"`
	PolicyRevision string `yaml:"policy_revision" json:"policy_revision"`
	maxSessionTTL  time.Duration
}

// Request contains only verified identity facts and normalized resource scope.
// Agent, role, task, issue, run, trigger, runtime, and display name are absent by
// design so callers cannot accidentally turn provenance into authorization.
type Request struct {
	Issuer           string
	IssuerInstanceID string
	AssertionKeyID   string
	Subject          string
	Target           string
	Service          string
	Repository       string
	Operation        string
	// WorkspaceID, TeamIdentityID, PolicyClass, and MembershipEpoch are
	// verified canonical workload facts. They are absent only for typed legacy
	// mode, whose subject binding has its own explicit compatibility contract.
	WorkspaceID     string
	TeamIdentityID  string
	PolicyClass     string
	MembershipEpoch int64
	Now             time.Time
}

type Operation struct {
	Name               string     `json:"name"`
	RequiredPermission Permission `json:"required_permission"`
	Capabilities       []string   `json:"capabilities"`
}

type Resolved struct {
	Issuer      TrustedIssuer
	Binding     PrincipalBinding // set only for typed legacy compatibility
	TeamBinding TeamBinding      // set only for canonical team authority
	PolicyClass PolicyClass      // set only for canonical team authority
	Resource    ResourcePolicy
	Operation   Operation
	PrincipalID uint
}

// ResourceOperationRequest selects the shared resource/operation policy used by
// Access Grant transport Sessions and authenticated durable principals. Target
// may be omitted only when service+repository identify exactly one policy.
type ResourceOperationRequest struct {
	Target      string
	Service     string
	Repository  string
	Operation   string
	PolicyClass string
}

type ResolvedResourceOperation struct {
	Resource  ResourcePolicy
	Operation Operation
}

// UnmarshalYAML rejects unknown, authorization-shaped, and secret-shaped
// fields before decoding the typed desired state.
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	if err := validateMappingFields(value, "principal_sessions", map[string]fieldValidator{
		"version": nil, "contract_revision": nil, "legacy_compatibility_mode": nil,
		"trusted_issuers": sequenceOf("trusted issuer", map[string]fieldValidator{
			"id": nil, "issuer": nil, "key_ids": nil, "status": nil, "trust_revision": nil,
		}),
		"bindings": sequenceOf("principal binding", map[string]fieldValidator{
			"id": nil, "issuer_instance_id": nil, "subject": nil, "principal_id": nil,
			"status": nil, "binding_revision": nil, "expires_at": nil, "revoked_at": nil,
		}),
		"team_bindings": sequenceOf("team authority binding", map[string]fieldValidator{
			"id": nil, "issuer_instance_id": nil, "workspace_id": nil, "team_identity_id": nil, "policy_class": nil,
			"principal_id": nil, "status": nil, "binding_revision": nil, "epoch_floor": nil,
		}),
		"policy_classes": sequenceOf("policy class", map[string]fieldValidator{
			"id": nil, "status": nil, "policy_revision": nil, "operations": nil,
		}),
		"resource_defaults": sequenceOf("session resource default", map[string]fieldValidator{
			"id": nil, "target": nil, "service": nil, "status": nil,
			"max_session_ttl": nil, "policy_revision": nil,
		}),
		"resources": sequenceOf("session resource", map[string]fieldValidator{
			"id": nil, "target": nil, "service": nil, "repository": nil, "status": nil,
			"max_session_ttl": nil, "policy_revision": nil,
		}),
	}); err != nil {
		return err
	}
	type plainConfig Config
	var decoded plainConfig
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*c = Config(decoded)
	return nil
}

type fieldValidator func(*yaml.Node) error

func sequenceOf(label string, fields map[string]fieldValidator) fieldValidator {
	return func(node *yaml.Node) error {
		if node.Kind != yaml.SequenceNode {
			return fmt.Errorf("%s entries must be a sequence", label)
		}
		for index, child := range node.Content {
			if err := validateMappingFields(child, fmt.Sprintf("%s %d", label, index), fields); err != nil {
				return err
			}
		}
		return nil
	}
}

func validateMappingFields(node *yaml.Node, label string, allowed map[string]fieldValidator) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return fmt.Errorf("%s must be a mapping", label)
	}
	seen := map[string]struct{}{}
	for index := 0; index+1 < len(node.Content); index += 2 {
		key, child := strings.TrimSpace(node.Content[index].Value), node.Content[index+1]
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%s contains duplicate field", label)
		}
		seen[key] = struct{}{}
		validator, ok := allowed[key]
		if !ok {
			return fmt.Errorf("%s contains unsupported field", label)
		}
		if validator != nil {
			if err := validator(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// Set is an immutable normalized authority snapshot.
type Set struct {
	config                  Config
	issuersByID             map[string]TrustedIssuer
	issuerIDByIssuerKey     map[string]string
	bindingsByID            map[string]PrincipalBinding
	bindingsBySubject       map[string]PrincipalBinding
	teamBindingsByKey       map[string]TeamBinding
	policyClassesByID       map[string]PolicyClass
	resourceDefaultsByScope map[string]ResourceDefault
	resourcesByScope        map[string]ResourcePolicy
}

// defaultDynamicPolicyOperations is the legacy principal_sessions default class
// ceiling. Access Grant standard envelopes no longer read this list; risk and
// standard operation truth live in operationcatalog (AGS-T022).
var defaultDynamicPolicyOperations = []string{
	"ci.read",
	"git.push",
	"git.read",
	"pr.create",
	"pr.read",
	"pr.rebase",
	"repo.read",
	"review.read",
}

var operations = map[string]Operation{
	"repo.read":               {Name: "repo.read", RequiredPermission: PermissionRead, Capabilities: []string{"repo:read"}},
	"git.push":                {Name: "git.push", RequiredPermission: PermissionWrite, Capabilities: []string{"repo:read", "repo:write"}},
	"git.force_push":          {Name: "git.force_push", RequiredPermission: PermissionWrite, Capabilities: []string{"repo:read", "repo:write"}},
	"pr.create":               {Name: "pr.create", RequiredPermission: PermissionWrite, Capabilities: []string{"repo:read", "pr:create"}},
	"pr.comment":              {Name: "pr.comment", RequiredPermission: PermissionWrite, Capabilities: []string{"repo:read", "repo:write"}},
	"pr.edit":                 {Name: "pr.edit", RequiredPermission: PermissionWrite, Capabilities: []string{"repo:read", "repo:write"}},
	"pr.close":                {Name: "pr.close", RequiredPermission: PermissionWrite, Capabilities: []string{"repo:read", "repo:write"}},
	"pr.reopen":               {Name: "pr.reopen", RequiredPermission: PermissionWrite, Capabilities: []string{"repo:read", "repo:write"}},
	"pr.rebase":               {Name: "pr.rebase", RequiredPermission: PermissionWrite, Capabilities: []string{"repo:read", "repo:write"}},
	"pr.read":                 {Name: "pr.read", RequiredPermission: PermissionRead, Capabilities: []string{"repo:read"}},
	"pr.merge":                {Name: "pr.merge", RequiredPermission: PermissionWrite, Capabilities: []string{"repo:read", "repo:write"}},
	"ci.read":                 {Name: "ci.read", RequiredPermission: PermissionRead, Capabilities: []string{"repo:read"}},
	"review.read":             {Name: "review.read", RequiredPermission: PermissionRead, Capabilities: []string{"repo:read"}},
	"review.write":            {Name: "review.write", RequiredPermission: PermissionWrite, Capabilities: []string{"repo:read", "repo:write"}},
	"review.dismiss":          {Name: "review.dismiss", RequiredPermission: PermissionWrite, Capabilities: []string{"repo:read", "repo:write"}},
	"git.read":                {Name: "git.read", RequiredPermission: PermissionRead, Capabilities: []string{"repo:read"}},
	"repo.admin":              {Name: "repo.admin", RequiredPermission: PermissionAdmin, Capabilities: []string{"repo:read", "repo:write"}},
	"repo.create":             {Name: "repo.create", RequiredPermission: PermissionAdmin, Capabilities: []string{"repo:read", "repo:write"}},
	"repo.delete":             {Name: "repo.delete", RequiredPermission: PermissionAdmin, Capabilities: []string{"repo:read", "repo:write"}},
	"protected_ref.write":     {Name: "protected_ref.write", RequiredPermission: PermissionAdmin, Capabilities: []string{"repo:read", "repo:write"}},
	"ref.delete":              {Name: "ref.delete", RequiredPermission: PermissionWrite, Capabilities: []string{"repo:read", "repo:write"}},
	"branch_protection.write": {Name: "branch_protection.write", RequiredPermission: PermissionAdmin, Capabilities: []string{"repo:read", "repo:write"}},
	"webhook.write":           {Name: "webhook.write", RequiredPermission: PermissionAdmin, Capabilities: []string{"repo:read", "repo:write"}},
	// Legacy alias; authorization routes through review.write.
	"review.submit": {Name: "review.submit", RequiredPermission: PermissionWrite, Capabilities: []string{"repo:read", "repo:write"}},
}

// LookupOperation returns a defensive copy from the canonical operation
// registry. It is safe for assertion verifiers and callers to retain or mutate
// the returned value.
func LookupOperation(raw string) (Operation, bool) {
	operation, ok := operations[normalizeOperation(raw)]
	return cloneOperation(operation), ok
}

// RegisteredOperations returns defensive copies in stable order for complete
// policy/evaluator integration matrices.
func RegisteredOperations() []Operation {
	names := make([]string, 0, len(operations))
	for name := range operations {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Operation, 0, len(names))
	for _, name := range names {
		operation, _ := LookupOperation(name)
		out = append(out, operation)
	}
	return out
}

// DefaultOperationForCapabilities preserves the protocol's capability-only
// shorthand. Ambiguous capability sets intentionally choose the least-specific
// operation; callers that need another operation must sign it explicitly.
func DefaultOperationForCapabilities(capabilities []string) string {
	switch strings.Join(normalizeCapabilities(capabilities), "\x00") {
	case "repo:read":
		return "repo.read"
	case "repo:read\x00repo:write":
		return "git.push"
	case "pr:create", "pr:create\x00repo:read":
		return "pr.create"
	default:
		return ""
	}
}

// OperationSupportsCapabilities verifies an exact operation/capability match
// against the same registry consumed by resource authorization.
func OperationSupportsCapabilities(raw string, capabilities []string) bool {
	operation, ok := LookupOperation(raw)
	if !ok {
		return false
	}
	requested := strings.Join(normalizeCapabilities(capabilities), "\x00")
	for _, scope := range operationCapabilityScopes(operation) {
		if requested == strings.Join(normalizeCapabilities(scope), "\x00") {
			return true
		}
	}
	return false
}

// operationCapabilityScopes declares every wire-compatible capability shape
// for one registry entry. PR creation historically permits its dedicated
// transport capability alone; all other operations require their exact scope.
func operationCapabilityScopes(operation Operation) [][]string {
	if operation.Name == "pr.create" {
		return [][]string{{"pr:create"}, operation.Capabilities}
	}
	return [][]string{operation.Capabilities}
}

func normalizeCapabilities(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func New(input Config) (*Set, error) {
	if input.Version == 0 && input.ContractRevision == "" && input.TrustedIssuers == nil && input.Bindings == nil && input.TeamBindings == nil && input.PolicyClasses == nil && input.ResourceDefaults == nil && input.Resources == nil {
		return &Set{
			config: Config{}, issuersByID: map[string]TrustedIssuer{}, issuerIDByIssuerKey: map[string]string{},
			bindingsByID: map[string]PrincipalBinding{}, bindingsBySubject: map[string]PrincipalBinding{},
			teamBindingsByKey: map[string]TeamBinding{}, policyClassesByID: map[string]PolicyClass{},
			resourceDefaultsByScope: map[string]ResourceDefault{}, resourcesByScope: map[string]ResourcePolicy{},
		}, nil
	}
	if err := rejectSecretShapedAuthorityConfig(input); err != nil {
		return nil, err
	}
	legacyInput := input.Version == 1 && strings.TrimSpace(input.ContractRevision) == LegacyContractRevision
	if input.Version != CurrentVersion && !legacyInput {
		return nil, fmt.Errorf("unsupported principal session authority version %d", input.Version)
	}
	if strings.TrimSpace(input.ContractRevision) != ContractRevision && !legacyInput {
		return nil, fmt.Errorf("principal session contract_revision must be %q", ContractRevision)
	}
	if legacyInput && (len(input.TeamBindings) != 0 || len(input.PolicyClasses) != 0) {
		return nil, fmt.Errorf("legacy principal session authority cannot contain canonical team authority fields")
	}
	input.LegacyCompatibilityMode = strings.TrimSpace(input.LegacyCompatibilityMode)
	if len(input.Bindings) != 0 && input.LegacyCompatibilityMode != LegacySubjectCompatibilityMode {
		return nil, fmt.Errorf("legacy subject bindings require explicit compatibility mode")
	}
	if len(input.TeamBindings) != 0 && input.LegacyCompatibilityMode != "" {
		return nil, fmt.Errorf("canonical team authority cannot enable legacy compatibility mode")
	}

	set := &Set{
		config:      Config{Version: CurrentVersion, ContractRevision: ContractRevision, LegacyCompatibilityMode: input.LegacyCompatibilityMode},
		issuersByID: map[string]TrustedIssuer{}, issuerIDByIssuerKey: map[string]string{},
		bindingsByID: map[string]PrincipalBinding{}, bindingsBySubject: map[string]PrincipalBinding{},
		teamBindingsByKey: map[string]TeamBinding{}, policyClassesByID: map[string]PolicyClass{},
		resourceDefaultsByScope: map[string]ResourceDefault{}, resourcesByScope: map[string]ResourcePolicy{},
	}
	for index, candidate := range input.TrustedIssuers {
		issuer, err := normalizeIssuer(candidate)
		if err != nil {
			return nil, fmt.Errorf("trusted issuer %d: %w", index, err)
		}
		if _, exists := set.issuersByID[issuer.ID]; exists {
			return nil, fmt.Errorf("duplicate trusted issuer id %q", issuer.ID)
		}
		set.issuersByID[issuer.ID] = issuer
		for _, keyID := range issuer.KeyIDs {
			key := issuerKey(issuer.Issuer, keyID)
			if existing := set.issuerIDByIssuerKey[key]; existing != "" {
				return nil, fmt.Errorf("trusted issuer key id %q is ambiguous between instances %q and %q", keyID, existing, issuer.ID)
			}
			set.issuerIDByIssuerKey[key] = issuer.ID
		}
		set.config.TrustedIssuers = append(set.config.TrustedIssuers, issuer)
	}
	for index, candidate := range input.Bindings {
		binding, err := normalizeBinding(candidate)
		if err != nil {
			return nil, fmt.Errorf("principal binding %d: %w", index, err)
		}
		if _, exists := set.issuersByID[binding.IssuerInstanceID]; !exists {
			return nil, fmt.Errorf("principal binding %q references unknown trusted issuer %q", binding.ID, binding.IssuerInstanceID)
		}
		if _, exists := set.bindingsByID[binding.ID]; exists {
			return nil, fmt.Errorf("duplicate principal binding id %q", binding.ID)
		}
		key := bindingKey(binding.IssuerInstanceID, binding.Subject)
		if _, exists := set.bindingsBySubject[key]; exists {
			return nil, fmt.Errorf("duplicate or ambiguous principal binding issuer_instance_id=%q subject=%q", binding.IssuerInstanceID, binding.Subject)
		}
		set.bindingsByID[binding.ID] = binding
		set.bindingsBySubject[key] = binding
		set.config.Bindings = append(set.config.Bindings, binding)
	}
	for index, candidate := range input.PolicyClasses {
		policyClass, err := normalizePolicyClass(candidate)
		if err != nil {
			return nil, fmt.Errorf("policy class %d: %w", index, err)
		}
		if _, exists := set.policyClassesByID[policyClass.ID]; exists {
			return nil, fmt.Errorf("duplicate policy class id %q", policyClass.ID)
		}
		set.policyClassesByID[policyClass.ID] = policyClass
		set.config.PolicyClasses = append(set.config.PolicyClasses, policyClass)
	}
	for index, candidate := range input.TeamBindings {
		binding, err := normalizeTeamBinding(candidate)
		if err != nil {
			return nil, fmt.Errorf("team authority binding %d: %w", index, err)
		}
		if _, exists := set.issuersByID[binding.IssuerInstanceID]; !exists {
			return nil, fmt.Errorf("team authority binding %q references unknown trusted issuer %q", binding.ID, binding.IssuerInstanceID)
		}
		if _, exists := set.policyClassesByID[binding.PolicyClass]; !exists {
			return nil, fmt.Errorf("team authority binding %q references unknown policy class %q", binding.ID, binding.PolicyClass)
		}
		key := teamBindingKey(binding.IssuerInstanceID, binding.TeamIdentityID, binding.PolicyClass)
		if _, exists := set.teamBindingsByKey[key]; exists {
			return nil, fmt.Errorf("duplicate team authority binding issuer_instance_id=%q team_identity_id=%q policy_class=%q", binding.IssuerInstanceID, binding.TeamIdentityID, binding.PolicyClass)
		}
		set.teamBindingsByKey[key] = binding
		set.config.TeamBindings = append(set.config.TeamBindings, binding)
	}
	if len(input.TeamBindings) > 0 && len(input.Bindings) > 0 {
		return nil, fmt.Errorf("canonical team authority and legacy subject bindings cannot be combined")
	}
	for index, candidate := range input.ResourceDefaults {
		resourceDefault, err := normalizeResourceDefault(candidate)
		if err != nil {
			return nil, fmt.Errorf("session resource default %d: %w", index, err)
		}
		key := resourceDefaultKey(resourceDefault.Target, resourceDefault.Service)
		if _, exists := set.resourceDefaultsByScope[key]; exists {
			return nil, fmt.Errorf("duplicate session resource default target=%q service=%q", resourceDefault.Target, resourceDefault.Service)
		}
		set.resourceDefaultsByScope[key] = resourceDefault
		set.config.ResourceDefaults = append(set.config.ResourceDefaults, resourceDefault)
	}
	for index, candidate := range input.Resources {
		resource, err := normalizeResource(candidate)
		if err != nil {
			return nil, fmt.Errorf("session resource %d: %w", index, err)
		}
		key := resourceKey(resource.Target, resource.Service, resource.Repository)
		if _, exists := set.resourcesByScope[key]; exists {
			return nil, fmt.Errorf("duplicate session resource target=%q service=%q repository=%q", resource.Target, resource.Service, resource.Repository)
		}
		set.resourcesByScope[key] = resource
		set.config.Resources = append(set.config.Resources, resource)
	}
	if len(set.config.TrustedIssuers) == 0 || (len(set.config.ResourceDefaults) == 0 && len(set.config.Resources) == 0) || (len(set.config.Bindings) == 0 && len(set.config.TeamBindings) == 0) {
		return nil, fmt.Errorf("principal session authority requires trusted_issuers, resource_defaults or resources, and either legacy bindings or canonical team_bindings")
	}
	if len(set.config.TeamBindings) > 0 && len(set.config.PolicyClasses) == 0 {
		return nil, fmt.Errorf("canonical team authority requires policy_classes")
	}
	sort.Slice(set.config.TrustedIssuers, func(i, j int) bool { return set.config.TrustedIssuers[i].ID < set.config.TrustedIssuers[j].ID })
	sort.Slice(set.config.Bindings, func(i, j int) bool { return set.config.Bindings[i].ID < set.config.Bindings[j].ID })
	sort.Slice(set.config.TeamBindings, func(i, j int) bool { return set.config.TeamBindings[i].ID < set.config.TeamBindings[j].ID })
	sort.Slice(set.config.PolicyClasses, func(i, j int) bool { return set.config.PolicyClasses[i].ID < set.config.PolicyClasses[j].ID })
	sort.Slice(set.config.ResourceDefaults, func(i, j int) bool { return set.config.ResourceDefaults[i].ID < set.config.ResourceDefaults[j].ID })
	sort.Slice(set.config.Resources, func(i, j int) bool { return set.config.Resources[i].ID < set.config.Resources[j].ID })
	return set, nil
}

func (s *Set) Resolve(request Request) (Resolved, error) {
	if s == nil || s.config.Version != CurrentVersion {
		return Resolved{}, ErrAuthorityNotConfigured
	}
	now := request.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	issuer, err := s.resolveIssuer(request.Issuer, request.IssuerInstanceID, request.AssertionKeyID)
	if err != nil {
		return Resolved{}, err
	}
	// Presence of any canonical authority selector commits the request to
	// canonical team mode: it can never fall back to a subject binding.
	if strings.TrimSpace(request.WorkspaceID) != "" || strings.TrimSpace(request.TeamIdentityID) != "" || strings.TrimSpace(request.PolicyClass) != "" || request.MembershipEpoch != 0 {
		if strings.TrimSpace(request.WorkspaceID) == "" || strings.TrimSpace(request.TeamIdentityID) == "" || strings.TrimSpace(request.PolicyClass) == "" || request.MembershipEpoch <= 0 {
			return Resolved{}, ErrTeamBindingNotFound
		}
		binding, ok := s.teamBindingsByKey[teamBindingKey(issuer.ID, request.TeamIdentityID, request.PolicyClass)]
		if !ok || binding.WorkspaceID != strings.TrimSpace(request.WorkspaceID) {
			return Resolved{}, ErrTeamBindingNotFound
		}
		if binding.Status != "active" || request.MembershipEpoch < binding.EpochFloor {
			return Resolved{}, ErrTeamBindingInactive
		}
		policyClass, ok := s.policyClassesByID[binding.PolicyClass]
		if !ok {
			return Resolved{}, ErrPolicyClassNotFound
		}
		if policyClass.Status != "active" {
			return Resolved{}, ErrPolicyClassInactive
		}
		scope, err := s.ResolveResourceOperation(ResourceOperationRequest{
			Target: request.Target, Service: request.Service, Repository: request.Repository, Operation: request.Operation, PolicyClass: binding.PolicyClass,
		})
		if err != nil {
			return Resolved{}, err
		}
		return cloneResolved(Resolved{Issuer: issuer, TeamBinding: binding, PolicyClass: policyClass, Resource: scope.Resource, Operation: scope.Operation, PrincipalID: binding.PrincipalID}), nil
	}
	binding, ok := s.bindingsBySubject[bindingKey(issuer.ID, request.Subject)]
	if !ok {
		return Resolved{}, ErrBindingNotFound
	}
	if !bindingActive(binding, now) {
		return Resolved{}, ErrBindingInactive
	}
	scope, err := s.ResolveResourceOperation(ResourceOperationRequest{
		Target: request.Target, Service: request.Service, Repository: request.Repository, Operation: request.Operation,
	})
	if err != nil {
		return Resolved{}, err
	}
	return cloneResolved(Resolved{Issuer: issuer, Binding: binding, Resource: scope.Resource, Operation: scope.Operation, PrincipalID: binding.PrincipalID}), nil
}

// ResolveResourceOperation applies the shared resource and operation evaluator
// without selecting an identity. Durable callers already have an authenticated
// immutable AGS principal; Access Grant callers separately resolve canonical
// actor/executor authority and then use this same evaluator.
func (s *Set) ResolveResourceOperation(request ResourceOperationRequest) (ResolvedResourceOperation, error) {
	if s == nil || s.config.Version != CurrentVersion {
		return ResolvedResourceOperation{}, ErrAuthorityNotConfigured
	}

	target := strings.TrimSpace(request.Target)
	service := strings.ToLower(strings.TrimSpace(request.Service))
	repository := normalizeRepository(request.Repository)
	if service == "" || repository == "" {
		return ResolvedResourceOperation{}, ErrResourceNotFound
	}
	var resource ResourcePolicy
	if target != "" {
		if exact, ok := s.resourcesByScope[resourceKey(target, service, repository)]; ok {
			resource = exact
			if resource.Status != "active" {
				return ResolvedResourceOperation{}, ErrResourceInactive
			}
		} else {
			resourceDefault, ok := s.resourceDefaultsByScope[resourceDefaultKey(target, service)]
			if !ok {
				return ResolvedResourceOperation{}, ErrResourceNotFound
			}
			if resourceDefault.Status != "active" {
				return ResolvedResourceOperation{}, ErrResourceInactive
			}
			resource = resourcePolicyFromDefault(resourceDefault, repository)
		}
	} else {
		// Targetless resolution computes one effective candidate per target.
		// An exact row overrides only its own target default; it must not hide
		// an eligible default on another target or make a cross-target choice.
		targets := make(map[string]struct{})
		for _, candidate := range s.config.Resources {
			if candidate.Service == service && candidate.Repository == repository {
				targets[candidate.Target] = struct{}{}
			}
		}
		for _, candidate := range s.config.ResourceDefaults {
			if candidate.Service == service {
				targets[candidate.Target] = struct{}{}
			}
		}
		inactiveDefaultSeen := false
		for candidateTarget := range targets {
			exact, exactExists := s.resourcesByScope[resourceKey(candidateTarget, service, repository)]
			if exactExists {
				if exact.Status == "active" {
					if resource.ID != "" {
						return ResolvedResourceOperation{}, ErrResourceAmbiguous
					}
					resource = exact
				} else if _, defaultExists := s.resourceDefaultsByScope[resourceDefaultKey(candidateTarget, service)]; defaultExists {
					inactiveDefaultSeen = true
				}
				continue
			}
			resourceDefault, defaultExists := s.resourceDefaultsByScope[resourceDefaultKey(candidateTarget, service)]
			if !defaultExists {
				continue
			}
			if resourceDefault.Status != "active" {
				inactiveDefaultSeen = true
				continue
			}
			if resource.ID != "" {
				return ResolvedResourceOperation{}, ErrResourceAmbiguous
			}
			resource = resourcePolicyFromDefault(resourceDefault, repository)
		}
		if resource.ID == "" {
			if inactiveDefaultSeen {
				return ResolvedResourceOperation{}, ErrResourceInactive
			}
			return ResolvedResourceOperation{}, ErrResourceNotFound
		}
	}
	operation, ok := LookupOperation(request.Operation)
	if !ok {
		return ResolvedResourceOperation{}, ErrOperationNotSupported
	}
	if classID := strings.TrimSpace(request.PolicyClass); classID != "" {
		policyClass, ok := s.policyClassesByID[classID]
		if !ok {
			return ResolvedResourceOperation{}, ErrPolicyClassNotFound
		}
		if policyClass.Status != "active" {
			return ResolvedResourceOperation{}, ErrPolicyClassInactive
		}
		if !containsString(policyClass.Operations, operation.Name) {
			return ResolvedResourceOperation{}, ErrOperationNotSupported
		}
	}
	return ResolvedResourceOperation{Resource: resource, Operation: operation}, nil
}

func (s *Set) resolveIssuer(rawIssuer, rawID, rawKeyID string) (TrustedIssuer, error) {
	issuerName, id, keyID := strings.TrimSpace(rawIssuer), strings.TrimSpace(rawID), strings.TrimSpace(rawKeyID)
	if issuerName == "" || keyID == "" {
		return TrustedIssuer{}, ErrIssuerNotFound
	}
	if id != "" {
		issuer, ok := s.issuersByID[id]
		if !ok || issuer.Issuer != issuerName || !containsString(issuer.KeyIDs, keyID) {
			return TrustedIssuer{}, ErrIssuerNotFound
		}
		if issuer.Status != "active" {
			return TrustedIssuer{}, ErrIssuerInactive
		}
		return issuer, nil
	}
	issuerID := s.issuerIDByIssuerKey[issuerKey(issuerName, keyID)]
	if issuerID == "" {
		return TrustedIssuer{}, ErrIssuerNotFound
	}
	issuer := s.issuersByID[issuerID]
	if issuer.Status != "active" {
		return TrustedIssuer{}, ErrIssuerInactive
	}
	return issuer, nil
}

// DurablePrincipalAuthority resolves the one immutable team/policy binding for
// an already authenticated principal. Durable workflows persist this exact
// identity snapshot and must reject ambiguous bindings, including two active
// teams that happen to share one policy class.
type DurablePrincipalAuthority struct {
	TeamBinding TeamBinding
	PolicyClass PolicyClass
}

func (s *Set) DurablePrincipalAuthority(principalID uint) (DurablePrincipalAuthority, error) {
	if s == nil || s.config.Version != CurrentVersion {
		return DurablePrincipalAuthority{}, ErrAuthorityNotConfigured
	}
	if len(s.config.TeamBindings) == 0 {
		return DurablePrincipalAuthority{}, nil // typed legacy compatibility has no team authority.
	}
	var resolved DurablePrincipalAuthority
	for _, binding := range s.config.TeamBindings {
		if binding.Status != "active" || binding.PrincipalID != principalID {
			continue
		}
		policy, ok := s.policyClassesByID[binding.PolicyClass]
		if !ok || policy.Status != "active" {
			continue
		}
		if resolved.TeamBinding.ID != "" {
			return DurablePrincipalAuthority{}, ErrTeamBindingNotFound
		}
		resolved = DurablePrincipalAuthority{TeamBinding: binding, PolicyClass: policy}
	}
	if resolved.TeamBinding.ID == "" {
		return DurablePrincipalAuthority{}, ErrTeamBindingNotFound
	}
	resolved.PolicyClass.Operations = append([]string(nil), resolved.PolicyClass.Operations...)
	return resolved, nil
}

// DurablePolicyClass preserves the public evaluator helper while delegating
// canonical identity selection to the exact durable-principal resolver.
func (s *Set) DurablePolicyClass(principalID uint) (string, error) {
	resolved, err := s.DurablePrincipalAuthority(principalID)
	if err != nil {
		return "", err
	}
	return resolved.PolicyClass.ID, nil
}

// UsesCanonicalTeamAuthority reports whether this immutable snapshot accepts
// workload.authority.v1 identities rather than legacy issuer subjects.
func (s *Set) UsesCanonicalTeamAuthority() bool {
	return s != nil && len(s.config.TeamBindings) != 0
}

// AllowsLegacySubjectCompatibility reports whether both the desired state and
// the assertion must opt into the bounded subject-binding rollback path.
func (s *Set) AllowsLegacySubjectCompatibility() bool {
	return s != nil && len(s.config.Bindings) != 0 && s.config.LegacyCompatibilityMode == LegacySubjectCompatibilityMode
}

// TeamBinding returns a copy selected solely by immutable canonical authority
// coordinates. It is used by target-local revoke/floor operations.
func (s *Set) TeamBinding(issuerInstanceID, teamIdentityID, policyClass string) (TeamBinding, bool) {
	if s == nil {
		return TeamBinding{}, false
	}
	binding, ok := s.teamBindingsByKey[teamBindingKey(issuerInstanceID, teamIdentityID, policyClass)]
	return binding, ok && binding.Status == "active"
}

// RuntimePolicyAuthority selects one active policy binding using only facts
// already authenticated by an AGS execution-context connector. TeamIdentityID
// remains an authority output; callers cannot select it.
type RuntimePolicyAuthority struct {
	TeamBinding TeamBinding
	PolicyClass PolicyClass
}

// ResolveRuntimePolicyAuthority resolves one unambiguous source/workspace/class
// authority tuple for Access Grant issuance. It deliberately does not fall back
// to subject bindings or infer a class from an Agent/Squad name.
func (s *Set) ResolveRuntimePolicyAuthority(issuerInstanceID, workspaceID, policyClass string) (RuntimePolicyAuthority, error) {
	if s == nil || s.config.Version != CurrentVersion {
		return RuntimePolicyAuthority{}, ErrAuthorityNotConfigured
	}
	issuerInstanceID = strings.TrimSpace(issuerInstanceID)
	workspaceID = strings.TrimSpace(workspaceID)
	policyClass = strings.TrimSpace(policyClass)
	if issuerInstanceID == "" || workspaceID == "" || policyClass == "" {
		return RuntimePolicyAuthority{}, ErrTeamBindingNotFound
	}
	issuer, ok := s.issuersByID[issuerInstanceID]
	if !ok {
		return RuntimePolicyAuthority{}, ErrIssuerNotFound
	}
	if issuer.Status != "active" {
		return RuntimePolicyAuthority{}, ErrIssuerInactive
	}
	var selected TeamBinding
	inactiveSeen := false
	for _, binding := range s.config.TeamBindings {
		if binding.IssuerInstanceID != issuerInstanceID || binding.WorkspaceID != workspaceID || binding.PolicyClass != policyClass {
			continue
		}
		if binding.Status != "active" {
			inactiveSeen = true
			continue
		}
		if selected.ID != "" {
			return RuntimePolicyAuthority{}, ErrTeamBindingAmbiguous
		}
		selected = binding
	}
	if selected.ID == "" {
		if inactiveSeen {
			return RuntimePolicyAuthority{}, ErrTeamBindingInactive
		}
		return RuntimePolicyAuthority{}, ErrTeamBindingNotFound
	}
	policy, ok := s.policyClassesByID[selected.PolicyClass]
	if !ok {
		return RuntimePolicyAuthority{}, ErrPolicyClassNotFound
	}
	if policy.Status != "active" {
		return RuntimePolicyAuthority{}, ErrPolicyClassInactive
	}
	policy.Operations = append([]string(nil), policy.Operations...)
	return RuntimePolicyAuthority{TeamBinding: selected, PolicyClass: policy}, nil
}

func (s *Set) BindingActive(id string, now time.Time) bool {
	if s == nil {
		return false
	}
	binding, ok := s.bindingsByID[strings.TrimSpace(id)]
	return ok && bindingActive(binding, now.UTC())
}

func (s *Set) Config() Config {
	if s == nil || s.config.Version == 0 {
		return Config{}
	}
	out := s.config
	out.TrustedIssuers = make([]TrustedIssuer, len(s.config.TrustedIssuers))
	for index, issuer := range s.config.TrustedIssuers {
		out.TrustedIssuers[index] = issuer
		out.TrustedIssuers[index].KeyIDs = append([]string(nil), issuer.KeyIDs...)
	}
	out.Bindings = append([]PrincipalBinding(nil), s.config.Bindings...)
	out.TeamBindings = append([]TeamBinding(nil), s.config.TeamBindings...)
	out.PolicyClasses = make([]PolicyClass, len(s.config.PolicyClasses))
	for index, policyClass := range s.config.PolicyClasses {
		out.PolicyClasses[index] = policyClass
		out.PolicyClasses[index].Operations = append([]string(nil), policyClass.Operations...)
	}
	out.ResourceDefaults = append([]ResourceDefault(nil), s.config.ResourceDefaults...)
	out.Resources = append([]ResourcePolicy(nil), s.config.Resources...)
	return out
}

func (r ResourceDefault) MaxSessionDuration() time.Duration {
	if r.maxSessionTTL > 0 {
		return r.maxSessionTTL
	}
	d, _ := time.ParseDuration(r.MaxSessionTTL)
	return d
}

func (r ResourcePolicy) MaxSessionDuration() time.Duration {
	if r.maxSessionTTL > 0 {
		return r.maxSessionTTL
	}
	d, _ := time.ParseDuration(r.MaxSessionTTL)
	return d
}

func normalizeIssuer(input TrustedIssuer) (TrustedIssuer, error) {
	out := input
	out.ID = strings.TrimSpace(out.ID)
	out.Issuer = strings.TrimSpace(out.Issuer)
	out.Status = strings.ToLower(strings.TrimSpace(out.Status))
	out.TrustRevision = strings.TrimSpace(out.TrustRevision)
	seenKeys := make(map[string]struct{}, len(out.KeyIDs))
	keys := make([]string, 0, len(out.KeyIDs))
	for _, rawKeyID := range out.KeyIDs {
		keyID := strings.TrimSpace(rawKeyID)
		if keyID == "" {
			return TrustedIssuer{}, fmt.Errorf("key_ids must contain non-empty values")
		}
		if _, duplicate := seenKeys[keyID]; duplicate {
			return TrustedIssuer{}, fmt.Errorf("duplicate key_id %q", keyID)
		}
		seenKeys[keyID] = struct{}{}
		keys = append(keys, keyID)
	}
	sort.Strings(keys)
	out.KeyIDs = keys
	if out.ID == "" || out.Issuer == "" || out.TrustRevision == "" || len(out.KeyIDs) == 0 {
		return TrustedIssuer{}, fmt.Errorf("id, issuer, key_ids, and trust_revision are required")
	}
	if out.Status != "active" && out.Status != "disabled" {
		return TrustedIssuer{}, fmt.Errorf("status must be active or disabled")
	}
	return out, nil
}

func normalizeBinding(input PrincipalBinding) (PrincipalBinding, error) {
	out := input
	out.ID = strings.TrimSpace(out.ID)
	out.IssuerInstanceID = strings.TrimSpace(out.IssuerInstanceID)
	out.Subject = strings.TrimSpace(out.Subject)
	out.Status = strings.ToLower(strings.TrimSpace(out.Status))
	out.BindingRevision = strings.TrimSpace(out.BindingRevision)
	out.ExpiresAt = strings.TrimSpace(out.ExpiresAt)
	out.RevokedAt = strings.TrimSpace(out.RevokedAt)
	if out.ID == "" || out.IssuerInstanceID == "" || out.Subject == "" || out.PrincipalID == 0 || out.BindingRevision == "" {
		return PrincipalBinding{}, fmt.Errorf("id, issuer_instance_id, subject, principal_id, and binding_revision are required")
	}
	if out.Status != "active" && out.Status != "disabled" && out.Status != "revoked" {
		return PrincipalBinding{}, fmt.Errorf("status must be active, disabled, or revoked")
	}
	if out.RevokedAt != "" {
		parsed, err := time.Parse(time.RFC3339, out.RevokedAt)
		if err != nil {
			return PrincipalBinding{}, fmt.Errorf("revoked_at must be RFC3339")
		}
		out.RevokedAt = parsed.UTC().Format(time.RFC3339)
	}
	if out.Status == "revoked" && out.RevokedAt == "" {
		return PrincipalBinding{}, fmt.Errorf("revoked binding requires revoked_at")
	}
	if out.Status != "revoked" && out.RevokedAt != "" {
		return PrincipalBinding{}, fmt.Errorf("revoked_at requires revoked status")
	}
	if out.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, out.ExpiresAt)
		if err != nil {
			return PrincipalBinding{}, fmt.Errorf("expires_at must be RFC3339")
		}
		parsed = parsed.UTC()
		out.ExpiresAt = parsed.Format(time.RFC3339)
		out.expiresAt = &parsed
	}
	return out, nil
}

func normalizeTeamBinding(input TeamBinding) (TeamBinding, error) {
	out := input
	out.ID = strings.TrimSpace(out.ID)
	out.IssuerInstanceID = strings.TrimSpace(out.IssuerInstanceID)
	out.WorkspaceID = strings.TrimSpace(out.WorkspaceID)
	out.TeamIdentityID = strings.TrimSpace(out.TeamIdentityID)
	out.PolicyClass = strings.TrimSpace(out.PolicyClass)
	out.Status = strings.ToLower(strings.TrimSpace(out.Status))
	out.BindingRevision = strings.TrimSpace(out.BindingRevision)
	if out.ID == "" || out.IssuerInstanceID == "" || out.WorkspaceID == "" || out.TeamIdentityID == "" || out.PolicyClass == "" || out.PrincipalID == 0 || out.BindingRevision == "" {
		return TeamBinding{}, fmt.Errorf("id, issuer_instance_id, workspace_id, team_identity_id, policy_class, principal_id, and binding_revision are required")
	}
	if out.Status != "active" && out.Status != "disabled" && out.Status != "revoked" {
		return TeamBinding{}, fmt.Errorf("status must be active, disabled, or revoked")
	}
	if out.EpochFloor < 0 {
		return TeamBinding{}, fmt.Errorf("epoch_floor must not be negative")
	}
	return out, nil
}

func normalizePolicyClass(input PolicyClass) (PolicyClass, error) {
	out := input
	out.ID = strings.TrimSpace(out.ID)
	out.Status = strings.ToLower(strings.TrimSpace(out.Status))
	out.PolicyRevision = strings.TrimSpace(out.PolicyRevision)
	if out.ID == "" || out.PolicyRevision == "" || (out.Status != "active" && out.Status != "disabled") {
		return PolicyClass{}, fmt.Errorf("id, status, and policy_revision are required")
	}
	seen := map[string]struct{}{}
	operations := make([]string, 0, len(out.Operations))
	for _, raw := range out.Operations {
		name := normalizeOperation(raw)
		if _, supported := LookupOperation(name); !supported || name == "" {
			return PolicyClass{}, errors.New("contains unsupported operation")
		}
		if _, duplicate := seen[name]; duplicate {
			return PolicyClass{}, errors.New("contains duplicate operation")
		}
		seen[name] = struct{}{}
		operations = append(operations, name)
	}
	if len(operations) == 0 {
		return PolicyClass{}, fmt.Errorf("operations must not be empty")
	}
	sort.Strings(operations)
	if out.ID == DefaultDynamicPolicyClass && !sameOperations(operations, defaultDynamicPolicyOperations) {
		return PolicyClass{}, errors.New("default dynamic policy class operations must match its fixed ceiling")
	}
	out.Operations = operations
	return out, nil
}

func sameOperations(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func normalizeResourceDefault(input ResourceDefault) (ResourceDefault, error) {
	out := input
	out.ID = strings.TrimSpace(out.ID)
	out.Target = strings.TrimSpace(out.Target)
	out.Service = strings.ToLower(strings.TrimSpace(out.Service))
	out.Status = strings.ToLower(strings.TrimSpace(out.Status))
	out.MaxSessionTTL = strings.TrimSpace(out.MaxSessionTTL)
	out.PolicyRevision = strings.TrimSpace(out.PolicyRevision)
	if out.ID == "" || out.Target == "" || out.Service == "" || out.MaxSessionTTL == "" || out.PolicyRevision == "" {
		return ResourceDefault{}, fmt.Errorf("id, target, service, max_session_ttl, and policy_revision are required")
	}
	if out.Service != "ags" {
		return ResourceDefault{}, fmt.Errorf("service must be ags")
	}
	if out.Status != "active" && out.Status != "disabled" {
		return ResourceDefault{}, fmt.Errorf("status must be active or disabled")
	}
	ttl, err := time.ParseDuration(out.MaxSessionTTL)
	if err != nil || ttl <= 0 || ttl > MaximumSessionTTL {
		return ResourceDefault{}, fmt.Errorf("max_session_ttl must be positive and at most %s", MaximumSessionTTL)
	}
	out.maxSessionTTL = ttl
	return out, nil
}

func resourcePolicyFromDefault(input ResourceDefault, repository string) ResourcePolicy {
	return ResourcePolicy{
		ID: input.ID, Target: input.Target, Service: input.Service, Repository: repository,
		Status: input.Status, MaxSessionTTL: input.MaxSessionTTL, PolicyRevision: input.PolicyRevision,
		maxSessionTTL: input.maxSessionTTL,
	}
}

func normalizeResource(input ResourcePolicy) (ResourcePolicy, error) {
	out := input
	out.ID = strings.TrimSpace(out.ID)
	out.Target = strings.TrimSpace(out.Target)
	out.Service = strings.ToLower(strings.TrimSpace(out.Service))
	out.Repository = normalizeRepository(out.Repository)
	out.Status = strings.ToLower(strings.TrimSpace(out.Status))
	out.MaxSessionTTL = strings.TrimSpace(out.MaxSessionTTL)
	out.PolicyRevision = strings.TrimSpace(out.PolicyRevision)
	if out.ID == "" || out.Target == "" || out.Service == "" || out.Repository == "" || out.MaxSessionTTL == "" || out.PolicyRevision == "" {
		return ResourcePolicy{}, fmt.Errorf("id, target, service, repository, max_session_ttl, and policy_revision are required")
	}
	if out.Service != "ags" {
		return ResourcePolicy{}, fmt.Errorf("service must be ags")
	}
	if out.Status != "active" && out.Status != "disabled" {
		return ResourcePolicy{}, fmt.Errorf("status must be active or disabled")
	}
	ttl, err := time.ParseDuration(out.MaxSessionTTL)
	if err != nil || ttl <= 0 || ttl > MaximumSessionTTL {
		return ResourcePolicy{}, fmt.Errorf("max_session_ttl must be positive and at most %s", MaximumSessionTTL)
	}
	out.maxSessionTTL = ttl
	return out, nil
}

func bindingActive(binding PrincipalBinding, now time.Time) bool {
	if binding.Status != "active" {
		return false
	}
	return binding.expiresAt == nil || binding.expiresAt.After(now)
}

func normalizeRepository(raw string) string {
	parts := strings.Split(strings.TrimSpace(raw), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return ""
	}
	return strings.TrimSpace(parts[0]) + "/" + strings.TrimSpace(parts[1])
}

func normalizeOperation(raw string) string { return strings.ToLower(strings.TrimSpace(raw)) }

// rejectSecretShapedAuthorityConfig keeps bearer-like material out of
// authority configuration before any validation path can include it in an
// error, receipt, or audit entry. Authority coordinates are identifiers, not
// credential transport fields.
func rejectSecretShapedAuthorityConfig(input Config) error {
	values := []string{input.ContractRevision, input.LegacyCompatibilityMode}
	for _, issuer := range input.TrustedIssuers {
		values = append(values, issuer.ID, issuer.Issuer, issuer.Status, issuer.TrustRevision)
		values = append(values, issuer.KeyIDs...)
	}
	for _, binding := range input.Bindings {
		values = append(values, binding.ID, binding.IssuerInstanceID, binding.Subject, binding.Status, binding.BindingRevision, binding.ExpiresAt, binding.RevokedAt)
	}
	for _, binding := range input.TeamBindings {
		values = append(values, binding.ID, binding.IssuerInstanceID, binding.WorkspaceID, binding.TeamIdentityID, binding.PolicyClass, binding.Status, binding.BindingRevision)
	}
	for _, policy := range input.PolicyClasses {
		values = append(values, policy.ID, policy.Status, policy.PolicyRevision)
		values = append(values, policy.Operations...)
	}
	for _, resourceDefault := range input.ResourceDefaults {
		values = append(values, resourceDefault.ID, resourceDefault.Target, resourceDefault.Service, resourceDefault.Status, resourceDefault.MaxSessionTTL, resourceDefault.PolicyRevision)
	}
	for _, resource := range input.Resources {
		values = append(values, resource.ID, resource.Target, resource.Service, resource.Repository, resource.Status, resource.MaxSessionTTL, resource.PolicyRevision)
	}
	for _, value := range values {
		if IsSecretShapedValue(value) {
			return errors.New("authority configuration contains secret-shaped value")
		}
	}
	return nil
}

// IsSecretShapedValue identifies bearer-like material that must never cross an
// authority configuration, request, receipt, audit, or public-error boundary.
func IsSecretShapedValue(value string) bool {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if strings.Contains(lower, "mat_") || strings.Contains(lower, "ags_sess_") || strings.Contains(lower, "-----begin") || strings.Contains(lower, "private key") {
		return true
	}
	return strings.HasPrefix(value, "eyJ") && strings.Count(value, ".") == 2
}

func issuerKey(issuer, keyID string) string {
	return strings.TrimSpace(issuer) + "\x00" + strings.TrimSpace(keyID)
}
func containsString(values []string, wanted string) bool {
	index := sort.SearchStrings(values, wanted)
	return index < len(values) && values[index] == wanted
}
func bindingKey(issuerInstanceID, subject string) string {
	return strings.TrimSpace(issuerInstanceID) + "\x00" + strings.TrimSpace(subject)
}
func teamBindingKey(issuerInstanceID, teamIdentityID, policyClass string) string {
	return strings.TrimSpace(issuerInstanceID) + "\x00" + strings.TrimSpace(teamIdentityID) + "\x00" + strings.TrimSpace(policyClass)
}
func resourceDefaultKey(target, service string) string {
	return strings.TrimSpace(target) + "\x00" + strings.ToLower(strings.TrimSpace(service))
}
func resourceKey(target, service, repository string) string {
	return resourceDefaultKey(target, service) + "\x00" + normalizeRepository(repository)
}
func cloneResolved(input Resolved) Resolved {
	out := input
	out.Issuer.KeyIDs = append([]string(nil), input.Issuer.KeyIDs...)
	out.PolicyClass.Operations = append([]string(nil), input.PolicyClass.Operations...)
	out.Operation = cloneOperation(input.Operation)
	return out
}

func cloneOperation(input Operation) Operation {
	out := input
	out.Capabilities = append([]string(nil), input.Capabilities...)
	return out
}
