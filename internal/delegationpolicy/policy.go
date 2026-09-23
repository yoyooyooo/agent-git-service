// Package delegationpolicy parses legacy delegation v1/v2 snapshots as
// compatibility migration inventory. These selectors, roles, task IDs, and
// capability ceilings are never principal/session-v2 authorization inputs.
package delegationpolicy

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

const (
	LegacyVersion     = 1
	CurrentVersion    = 2
	MaximumSessionTTL = 2 * time.Hour
)

var (
	ErrPolicyNotFound       = errors.New("delegation policy not found")
	ErrRepositoryNotAllowed = errors.New("repository not allowed by delegation policy")
)

var allowedCapabilities = map[string]struct{}{
	"repo:read":  {},
	"repo:write": {},
	"pr:read":    {},
	"pr:create":  {},
	"pr:update":  {},
	"pr:comment": {},
	"pr:review":  {},
}

var hardForbiddenCapabilities = map[string]struct{}{
	"pr:merge":        {},
	"repo:admin":      {},
	"identity:manage": {},
	"token:manage":    {},
	"access:manage":   {},
	"policy:manage":   {},
}

// legacyRoleShape validates that migration input matches the historical v2
// schema. It is not consulted by session exchange or native authorization.
type legacyRoleShape struct {
	capabilities          map[string]struct{}
	maxTTL                time.Duration
	requiresLegacyTaskIDs bool
}

var legacyRoleShapes = map[string]legacyRoleShape{
	"implementer-a": {capabilities: capabilitySet("repo:read", "repo:write", "pr:create"), maxTTL: 30 * time.Minute},
	"implementer-b": {capabilities: capabilitySet("repo:read", "repo:write", "pr:create"), maxTTL: 30 * time.Minute},
	"critic":        {capabilities: capabilitySet("repo:read"), maxTTL: 30 * time.Minute},
	"ci-repair":     {capabilities: capabilitySet("repo:read", "repo:write", "pr:create"), maxTTL: 15 * time.Minute, requiresLegacyTaskIDs: true},
	"coordinator":   {capabilities: capabilitySet("repo:read"), maxTTL: 15 * time.Minute},
	"planner":       {capabilities: capabilitySet("repo:read"), maxTTL: 15 * time.Minute},
}

func capabilitySet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

// Config is the runtime projection shape stored under integrations.yaml's
// top-level delegation key. Version 1 uses policies; version 2 uses mappings.
// The two generations are deliberately not valid in one snapshot.
type Config struct {
	Version  int            `yaml:"version" json:"version"`
	Policies []Policy       `yaml:"policies,omitempty" json:"policies,omitempty"`
	Mappings []AgentMapping `yaml:"mappings,omitempty" json:"mappings,omitempty"`
}

// Policy is the legacy v1 workspace-scoped migration input.
type Policy struct {
	ID                 string                      `yaml:"id" json:"id"`
	Issuer             string                      `yaml:"issuer" json:"issuer"`
	WorkspaceID        string                      `yaml:"workspace_id" json:"workspace_id"`
	Target             string                      `yaml:"target" json:"target"`
	Principal          string                      `yaml:"principal" json:"principal"`
	Repositories       map[string]RepositoryPolicy `yaml:"repositories" json:"repositories"`
	MaxSessionTTL      string                      `yaml:"max_session_ttl" json:"max_session_ttl"`
	AllowMerge         bool                        `yaml:"allow_merge" json:"allow_merge"`
	Status             string                      `yaml:"status" json:"status"`
	PolicyVersion      string                      `yaml:"policy_version" json:"policy_version"`
	maxSessionDuration time.Duration
}

// AgentMapping is one historical v2 migration record. Role, TaskIDs,
// MaxCapabilities, and MaxSessionTTL are preserved only to explain the source
// snapshot; target authorization does not consume them.
type AgentMapping struct {
	ID                 string   `yaml:"id" json:"id"`
	Issuer             string   `yaml:"issuer" json:"issuer"`
	WorkspaceID        string   `yaml:"workspace_id" json:"workspace_id"`
	AgentID            string   `yaml:"agent_id" json:"agent_id"`
	Role               string   `yaml:"role" json:"role"`
	TaskIDs            []string `yaml:"task_ids,omitempty" json:"task_ids,omitempty"`
	Target             string   `yaml:"target" json:"target"`
	Repository         string   `yaml:"repository" json:"repository"`
	Principal          string   `yaml:"principal" json:"principal"`
	MaxCapabilities    []string `yaml:"max_capabilities" json:"max_capabilities"`
	MaxSessionTTL      string   `yaml:"max_session_ttl" json:"max_session_ttl"`
	AllowMerge         bool     `yaml:"allow_merge" json:"allow_merge"`
	Status             string   `yaml:"status" json:"status"`
	PolicyVersion      string   `yaml:"policy_version" json:"policy_version"`
	maxSessionDuration time.Duration
}

// RepositoryPolicy is the v1 capability ceiling for one exact owner/name repo.
type RepositoryPolicy struct {
	MaxCapabilities []string `yaml:"max_capabilities" json:"max_capabilities"`
}

// Selector contains only verified workload and target facts. Display names,
// prompt roles, runtime hosts, and stable-principal logins are intentionally
// absent from this interface.
type Selector struct {
	Issuer      string
	WorkspaceID string
	AgentID     string
	TaskID      string
	Target      string
	Repository  string
}

// ResolvedPolicy is the normalized authority selected for one exact repo.
// Exactly one of Policy (v1) or Mapping (v2) is populated.
type ResolvedPolicy struct {
	SchemaVersion    int              `json:"schema_version"`
	Policy           Policy           `json:"policy,omitempty"`
	Mapping          AgentMapping     `json:"mapping,omitempty"`
	Repository       string           `json:"repository"`
	RepositoryPolicy RepositoryPolicy `json:"repository_policy"`
}

// Set is an immutable, normalized policy snapshot loaded at process start.
type Set struct {
	config           Config
	legacyByTrustKey map[string]Policy
	bySelector       map[string]AgentMapping
}

// UnmarshalYAML rejects bearer/signing material anywhere in the committed
// delegation subtree before decoding the typed policy.
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	if field, ok := sensitiveField(value); ok {
		return fmt.Errorf("delegation policy contains forbidden sensitive field %q", field)
	}
	if err := validateConfigShape(value); err != nil {
		return err
	}
	type plainConfig Config
	var decoded plainConfig
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	versionPresent, policiesPresent, mappingsPresent := false, false, false
	for i := 0; i+1 < len(value.Content); i += 2 {
		switch value.Content[i].Value {
		case "version":
			versionPresent = true
		case "policies":
			policiesPresent = true
		case "mappings":
			mappingsPresent = true
		}
	}
	if versionPresent && decoded.Version != LegacyVersion && decoded.Version != CurrentVersion {
		return fmt.Errorf("unsupported delegation policy version %d", decoded.Version)
	}
	if !versionPresent && (policiesPresent || mappingsPresent) {
		return fmt.Errorf("unsupported delegation policy version 0")
	}
	if (decoded.Version == LegacyVersion && mappingsPresent) || (decoded.Version == CurrentVersion && policiesPresent) {
		return fmt.Errorf("delegation v1 policies and v2 mappings cannot coexist")
	}
	*c = Config(decoded)
	return nil
}

func validateConfigShape(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return fmt.Errorf("delegation policy config must be a mapping")
	}
	allowedConfig := map[string]struct{}{"version": {}, "policies": {}, "mappings": {}}
	seen := map[string]struct{}{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("delegation config contains duplicate field %q", key)
		}
		seen[key] = struct{}{}
		if _, ok := allowedConfig[key]; !ok {
			return fmt.Errorf("delegation config contains unsupported field %q", key)
		}
		switch key {
		case "policies":
			if value.Kind != yaml.SequenceNode {
				return fmt.Errorf("delegation policies must be a sequence")
			}
			for index, policy := range value.Content {
				if err := validatePolicyShape(policy, index); err != nil {
					return err
				}
			}
		case "mappings":
			if value.Kind != yaml.SequenceNode {
				return fmt.Errorf("delegation mappings must be a sequence")
			}
			for index, mapping := range value.Content {
				if err := validateMappingShape(mapping, index); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validatePolicyShape(node *yaml.Node, index int) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("delegation policy %d must be a mapping", index)
	}
	allowed := map[string]struct{}{
		"id": {}, "issuer": {}, "workspace_id": {}, "target": {}, "principal": {}, "repositories": {},
		"max_session_ttl": {}, "allow_merge": {}, "status": {}, "policy_version": {},
	}
	seen := map[string]struct{}{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("delegation policy %d contains duplicate field %q", index, key)
		}
		seen[key] = struct{}{}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("delegation policy %d contains unsupported field %q", index, key)
		}
		if key != "repositories" {
			continue
		}
		if value.Kind != yaml.MappingNode {
			return fmt.Errorf("delegation policy %d repositories must be a mapping", index)
		}
		for repoIndex := 0; repoIndex+1 < len(value.Content); repoIndex += 2 {
			repository, repositoryPolicy := value.Content[repoIndex].Value, value.Content[repoIndex+1]
			if repositoryPolicy.Kind != yaml.MappingNode {
				return fmt.Errorf("repository %q policy must be a mapping", repository)
			}
			for fieldIndex := 0; fieldIndex+1 < len(repositoryPolicy.Content); fieldIndex += 2 {
				field := repositoryPolicy.Content[fieldIndex].Value
				if field != "max_capabilities" {
					return fmt.Errorf("repository %q contains unsupported field %q", repository, field)
				}
			}
		}
	}
	return nil
}

func validateMappingShape(node *yaml.Node, index int) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("delegation mapping %d must be a mapping", index)
	}
	allowed := map[string]struct{}{
		"id": {}, "issuer": {}, "workspace_id": {}, "agent_id": {}, "role": {}, "task_ids": {},
		"target": {}, "repository": {}, "principal": {}, "max_capabilities": {}, "max_session_ttl": {},
		"allow_merge": {}, "status": {}, "policy_version": {},
	}
	seen := map[string]struct{}{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("delegation mapping %d contains duplicate field %q", index, key)
		}
		seen[key] = struct{}{}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("delegation mapping %d contains unsupported field %q", index, key)
		}
	}
	return nil
}

func sensitiveField(node *yaml.Node) (string, bool) {
	if node == nil {
		return "", false
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := strings.ToLower(strings.TrimSpace(node.Content[i].Value))
			for _, fragment := range []string{"secret", "token", "credential", "password", "private_key", "signing_key"} {
				if strings.Contains(key, fragment) {
					return node.Content[i].Value, true
				}
			}
			if field, ok := sensitiveField(node.Content[i+1]); ok {
				return field, true
			}
		}
		return "", false
	}
	for _, child := range node.Content {
		if field, ok := sensitiveField(child); ok {
			return field, true
		}
	}
	return "", false
}

// New validates and normalizes one policy projection. Empty config is allowed
// so deployments can upgrade before any workspace is trusted.
func New(input Config) (*Set, error) {
	if input.Version == 0 && input.Policies == nil && input.Mappings == nil {
		return &Set{config: Config{}, legacyByTrustKey: map[string]Policy{}, bySelector: map[string]AgentMapping{}}, nil
	}
	if input.Version != LegacyVersion && input.Version != CurrentVersion {
		return nil, fmt.Errorf("unsupported delegation policy version %d", input.Version)
	}
	if input.Version == LegacyVersion {
		if input.Mappings != nil {
			return nil, fmt.Errorf("delegation v1 policies and v2 mappings cannot coexist")
		}
		return newLegacySet(input)
	}
	if input.Policies != nil {
		return nil, fmt.Errorf("delegation v1 policies and v2 mappings cannot coexist")
	}
	return newAgentMappingSet(input)
}

func newLegacySet(input Config) (*Set, error) {
	normalized := Config{Version: LegacyVersion, Policies: make([]Policy, 0, len(input.Policies))}
	byTrustKey := make(map[string]Policy, len(input.Policies))
	ids := make(map[string]struct{}, len(input.Policies))
	for index, candidate := range input.Policies {
		policy, err := normalizePolicy(candidate)
		if err != nil {
			return nil, fmt.Errorf("delegation policy %d: %w", index, err)
		}
		if _, exists := ids[policy.ID]; exists {
			return nil, fmt.Errorf("duplicate delegation policy id %q", policy.ID)
		}
		ids[policy.ID] = struct{}{}
		key := trustKey(policy.Issuer, policy.WorkspaceID, policy.Target)
		if _, exists := byTrustKey[key]; exists {
			return nil, fmt.Errorf("duplicate delegation trust key issuer=%q workspace_id=%q target=%q", policy.Issuer, policy.WorkspaceID, policy.Target)
		}
		byTrustKey[key] = policy
		normalized.Policies = append(normalized.Policies, policy)
	}
	sort.Slice(normalized.Policies, func(i, j int) bool { return normalized.Policies[i].ID < normalized.Policies[j].ID })
	return &Set{config: normalized, legacyByTrustKey: byTrustKey, bySelector: map[string]AgentMapping{}}, nil
}

func newAgentMappingSet(input Config) (*Set, error) {
	normalized := Config{Version: CurrentVersion, Mappings: make([]AgentMapping, 0, len(input.Mappings))}
	bySelector := make(map[string]AgentMapping, len(input.Mappings))
	ids := make(map[string]struct{}, len(input.Mappings))
	for index, candidate := range input.Mappings {
		mapping, err := normalizeAgentMapping(candidate)
		if err != nil {
			return nil, fmt.Errorf("delegation mapping %d: %w", index, err)
		}
		if _, exists := ids[mapping.ID]; exists {
			return nil, fmt.Errorf("duplicate delegation mapping id %q", mapping.ID)
		}
		ids[mapping.ID] = struct{}{}
		key := selectorKey(mapping.Issuer, mapping.WorkspaceID, mapping.AgentID, mapping.Target, mapping.Repository)
		if _, exists := bySelector[key]; exists {
			return nil, fmt.Errorf("duplicate or ambiguous delegation selector issuer=%q workspace_id=%q agent_id=%q target=%q repository=%q", mapping.Issuer, mapping.WorkspaceID, mapping.AgentID, mapping.Target, mapping.Repository)
		}
		bySelector[key] = mapping
		normalized.Mappings = append(normalized.Mappings, mapping)
	}
	sort.Slice(normalized.Mappings, func(i, j int) bool { return normalized.Mappings[i].ID < normalized.Mappings[j].ID })
	return &Set{config: normalized, legacyByTrustKey: map[string]Policy{}, bySelector: bySelector}, nil
}

func normalizePolicy(input Policy) (Policy, error) {
	policy := input
	policy.ID = strings.TrimSpace(policy.ID)
	policy.Issuer = strings.TrimSpace(policy.Issuer)
	policy.WorkspaceID = strings.TrimSpace(policy.WorkspaceID)
	policy.Target = strings.TrimSpace(policy.Target)
	policy.Principal = strings.TrimSpace(policy.Principal)
	policy.MaxSessionTTL = strings.TrimSpace(policy.MaxSessionTTL)
	policy.Status = strings.ToLower(strings.TrimSpace(policy.Status))
	policy.PolicyVersion = strings.TrimSpace(policy.PolicyVersion)

	for field, value := range map[string]string{
		"id": policy.ID, "issuer": policy.Issuer, "workspace_id": policy.WorkspaceID,
		"target": policy.Target, "principal": policy.Principal, "max_session_ttl": policy.MaxSessionTTL,
		"policy_version": policy.PolicyVersion,
	} {
		if value == "" {
			return Policy{}, fmt.Errorf("%s is required", field)
		}
	}
	if policy.Status != "active" && policy.Status != "disabled" {
		return Policy{}, fmt.Errorf("status must be active or disabled")
	}
	if policy.AllowMerge {
		return Policy{}, fmt.Errorf("allow_merge must be false for delegated workloads")
	}
	ttl, err := time.ParseDuration(policy.MaxSessionTTL)
	if err != nil || ttl <= 0 {
		return Policy{}, fmt.Errorf("max_session_ttl must be a positive duration")
	}
	if ttl > MaximumSessionTTL {
		return Policy{}, fmt.Errorf("max_session_ttl must not exceed %s", MaximumSessionTTL)
	}
	policy.maxSessionDuration = ttl

	if len(policy.Repositories) == 0 {
		return Policy{}, fmt.Errorf("at least one repository is required")
	}
	repositories := make(map[string]RepositoryPolicy, len(policy.Repositories))
	for rawRepo, rawRepoPolicy := range policy.Repositories {
		repo, err := normalizeRepository(rawRepo)
		if err != nil {
			return Policy{}, err
		}
		if _, exists := repositories[repo]; exists {
			return Policy{}, fmt.Errorf("duplicate normalized repository %q", repo)
		}
		capabilities, err := normalizeCapabilities(rawRepoPolicy.MaxCapabilities, false)
		if err != nil {
			return Policy{}, fmt.Errorf("repository %s: %w", repo, err)
		}
		repositories[repo] = RepositoryPolicy{MaxCapabilities: capabilities}
	}
	policy.Repositories = repositories
	return policy, nil
}

func normalizeAgentMapping(input AgentMapping) (AgentMapping, error) {
	mapping := input
	mapping.ID = strings.TrimSpace(mapping.ID)
	mapping.Issuer = strings.TrimSpace(mapping.Issuer)
	mapping.WorkspaceID = strings.TrimSpace(mapping.WorkspaceID)
	mapping.AgentID = strings.TrimSpace(mapping.AgentID)
	mapping.Role = strings.ToLower(strings.TrimSpace(mapping.Role))
	mapping.Target = strings.TrimSpace(mapping.Target)
	mapping.Repository = strings.TrimSpace(mapping.Repository)
	mapping.Principal = strings.TrimSpace(mapping.Principal)
	mapping.MaxSessionTTL = strings.TrimSpace(mapping.MaxSessionTTL)
	mapping.Status = strings.ToLower(strings.TrimSpace(mapping.Status))
	mapping.PolicyVersion = strings.TrimSpace(mapping.PolicyVersion)

	for field, value := range map[string]string{
		"id": mapping.ID, "issuer": mapping.Issuer, "workspace_id": mapping.WorkspaceID,
		"agent_id": mapping.AgentID, "role": mapping.Role, "target": mapping.Target,
		"repository": mapping.Repository, "principal": mapping.Principal,
		"max_session_ttl": mapping.MaxSessionTTL, "policy_version": mapping.PolicyVersion,
	} {
		if value == "" {
			return AgentMapping{}, fmt.Errorf("%s is required", field)
		}
	}
	if !isCanonicalUUID(mapping.WorkspaceID) {
		return AgentMapping{}, fmt.Errorf("workspace_id must be an immutable canonical UUID")
	}
	if !isCanonicalUUID(mapping.AgentID) {
		return AgentMapping{}, fmt.Errorf("agent_id must be an immutable canonical UUID")
	}
	ceiling, ok := legacyRoleShapes[mapping.Role]
	if !ok {
		return AgentMapping{}, fmt.Errorf("unknown role %q", mapping.Role)
	}
	if mapping.Status != "active" && mapping.Status != "disabled" {
		return AgentMapping{}, fmt.Errorf("status must be active or disabled")
	}
	if mapping.AllowMerge {
		return AgentMapping{}, fmt.Errorf("allow_merge must be false for every delegated role")
	}
	repository, err := normalizeRepository(mapping.Repository)
	if err != nil {
		return AgentMapping{}, err
	}
	mapping.Repository = repository
	capabilities, err := normalizeCapabilities(mapping.MaxCapabilities, true)
	if err != nil {
		return AgentMapping{}, err
	}
	for _, capability := range capabilities {
		if _, allowed := ceiling.capabilities[capability]; !allowed {
			return AgentMapping{}, fmt.Errorf("capability %q widens role %q ceiling", capability, mapping.Role)
		}
	}
	mapping.MaxCapabilities = capabilities
	ttl, err := time.ParseDuration(mapping.MaxSessionTTL)
	if err != nil || ttl <= 0 {
		return AgentMapping{}, fmt.Errorf("max_session_ttl must be a positive duration")
	}
	if ttl > ceiling.maxTTL {
		return AgentMapping{}, fmt.Errorf("max_session_ttl widens role %q ceiling of %s", mapping.Role, ceiling.maxTTL)
	}
	mapping.maxSessionDuration = ttl

	seenTasks := make(map[string]struct{}, len(mapping.TaskIDs))
	tasks := make([]string, 0, len(mapping.TaskIDs))
	for _, rawTaskID := range mapping.TaskIDs {
		taskID := strings.TrimSpace(rawTaskID)
		if !isCanonicalUUID(taskID) {
			return AgentMapping{}, fmt.Errorf("task_ids must contain immutable canonical UUIDs")
		}
		if _, duplicate := seenTasks[taskID]; duplicate {
			return AgentMapping{}, fmt.Errorf("duplicate task_id %q", taskID)
		}
		seenTasks[taskID] = struct{}{}
		tasks = append(tasks, taskID)
	}
	sort.Strings(tasks)
	mapping.TaskIDs = tasks
	if ceiling.requiresLegacyTaskIDs && len(mapping.TaskIDs) == 0 {
		return AgentMapping{}, fmt.Errorf("role %q requires exact observed shared-failure task_ids", mapping.Role)
	}
	if !ceiling.requiresLegacyTaskIDs && len(mapping.TaskIDs) != 0 {
		return AgentMapping{}, fmt.Errorf("role %q must not use task-scoped repair authority", mapping.Role)
	}
	return mapping, nil
}

func isCanonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func normalizeRepository(raw string) (string, error) {
	parts := strings.Split(raw, "/")
	if len(parts) != 2 {
		return "", fmt.Errorf("repository %q must use owner/name", strings.TrimSpace(raw))
	}
	owner, name := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if owner == "" || name == "" {
		return "", fmt.Errorf("repository %q must use owner/name", strings.TrimSpace(raw))
	}
	return owner + "/" + name, nil
}

func normalizeCapabilities(input []string, rejectDuplicates bool) ([]string, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("max_capabilities must not be empty")
	}
	seen := make(map[string]struct{}, len(input))
	capabilities := make([]string, 0, len(input))
	for _, raw := range input {
		capability := strings.ToLower(strings.TrimSpace(raw))
		if _, forbidden := hardForbiddenCapabilities[capability]; forbidden {
			return nil, fmt.Errorf("hard-forbidden capability %q", capability)
		}
		if _, allowed := allowedCapabilities[capability]; !allowed {
			return nil, fmt.Errorf("unsupported capability %q", capability)
		}
		if _, exists := seen[capability]; exists {
			if rejectDuplicates {
				return nil, fmt.Errorf("duplicate capability %q", capability)
			}
			continue
		}
		seen[capability] = struct{}{}
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)
	return capabilities, nil
}

func trustKey(issuer, workspaceID, target string) string {
	return strings.TrimSpace(issuer) + "\x00" + strings.TrimSpace(workspaceID) + "\x00" + strings.TrimSpace(target)
}

func selectorKey(issuer, workspaceID, agentID, target, repository string) string {
	return strings.TrimSpace(issuer) + "\x00" + strings.TrimSpace(workspaceID) + "\x00" + strings.TrimSpace(agentID) + "\x00" + strings.TrimSpace(target) + "\x00" + strings.TrimSpace(repository)
}

// Resolve explains one legacy migration input. It is retained for inventory
// tooling only; principal/session-v2 exchange never calls this method.
func (s *Set) Resolve(selector Selector) (ResolvedPolicy, error) {
	if s == nil {
		return ResolvedPolicy{}, ErrPolicyNotFound
	}
	repository, err := normalizeRepository(selector.Repository)
	if err != nil {
		return ResolvedPolicy{}, ErrRepositoryNotAllowed
	}
	if s.config.Version == LegacyVersion {
		policy, ok := s.legacyByTrustKey[trustKey(selector.Issuer, selector.WorkspaceID, selector.Target)]
		if !ok {
			return ResolvedPolicy{}, ErrPolicyNotFound
		}
		repoPolicy, ok := policy.Repositories[repository]
		if !ok {
			return ResolvedPolicy{}, ErrRepositoryNotAllowed
		}
		return ResolvedPolicy{SchemaVersion: LegacyVersion, Policy: clonePolicy(policy), Repository: repository, RepositoryPolicy: cloneRepositoryPolicy(repoPolicy)}, nil
	}
	if s.config.Version != CurrentVersion {
		return ResolvedPolicy{}, ErrPolicyNotFound
	}
	mapping, ok := s.bySelector[selectorKey(selector.Issuer, selector.WorkspaceID, selector.AgentID, selector.Target, repository)]
	if !ok {
		return ResolvedPolicy{}, ErrPolicyNotFound
	}
	if len(mapping.TaskIDs) != 0 && !contains(mapping.TaskIDs, strings.TrimSpace(selector.TaskID)) {
		return ResolvedPolicy{}, ErrPolicyNotFound
	}
	return ResolvedPolicy{
		SchemaVersion:    CurrentVersion,
		Mapping:          cloneAgentMapping(mapping),
		Repository:       repository,
		RepositoryPolicy: RepositoryPolicy{MaxCapabilities: append([]string(nil), mapping.MaxCapabilities...)},
	}, nil
}

func contains(values []string, wanted string) bool {
	index := sort.SearchStrings(values, wanted)
	return index < len(values) && values[index] == wanted
}

// ID returns the selected policy or mapping ID.
func (r ResolvedPolicy) ID() string {
	if r.SchemaVersion == CurrentVersion {
		return r.Mapping.ID
	}
	return r.Policy.ID
}

func (r ResolvedPolicy) Issuer() string {
	if r.SchemaVersion == CurrentVersion {
		return r.Mapping.Issuer
	}
	return r.Policy.Issuer
}

func (r ResolvedPolicy) WorkspaceID() string {
	if r.SchemaVersion == CurrentVersion {
		return r.Mapping.WorkspaceID
	}
	return r.Policy.WorkspaceID
}

func (r ResolvedPolicy) AgentID() string {
	if r.SchemaVersion == CurrentVersion {
		return r.Mapping.AgentID
	}
	return ""
}

func (r ResolvedPolicy) Role() string {
	if r.SchemaVersion == CurrentVersion {
		return r.Mapping.Role
	}
	return "legacy-workspace"
}

func (r ResolvedPolicy) TaskIDs() []string {
	if r.SchemaVersion == CurrentVersion {
		return append([]string(nil), r.Mapping.TaskIDs...)
	}
	return nil
}

func (r ResolvedPolicy) Target() string {
	if r.SchemaVersion == CurrentVersion {
		return r.Mapping.Target
	}
	return r.Policy.Target
}

func (r ResolvedPolicy) Principal() string {
	if r.SchemaVersion == CurrentVersion {
		return r.Mapping.Principal
	}
	return r.Policy.Principal
}

func (r ResolvedPolicy) MaxCapabilities() []string {
	return append([]string(nil), r.RepositoryPolicy.MaxCapabilities...)
}

func (r ResolvedPolicy) MaxSessionTTL() string {
	if r.SchemaVersion == CurrentVersion {
		return r.Mapping.MaxSessionTTL
	}
	return r.Policy.MaxSessionTTL
}

func (r ResolvedPolicy) AllowMerge() bool {
	if r.SchemaVersion == CurrentVersion {
		return r.Mapping.AllowMerge
	}
	return r.Policy.AllowMerge
}

func (r ResolvedPolicy) Status() string {
	if r.SchemaVersion == CurrentVersion {
		return r.Mapping.Status
	}
	return r.Policy.Status
}

func (r ResolvedPolicy) PolicyVersion() string {
	if r.SchemaVersion == CurrentVersion {
		return r.Mapping.PolicyVersion
	}
	return r.Policy.PolicyVersion
}

func (r ResolvedPolicy) MaxSessionDuration() time.Duration {
	if r.SchemaVersion == CurrentVersion {
		return r.Mapping.MaxSessionDuration()
	}
	return r.Policy.MaxSessionDuration()
}

// Policies returns a defensive copy of legacy v1 policies sorted by ID.
func (s *Set) Policies() []Policy {
	if s == nil {
		return nil
	}
	out := make([]Policy, len(s.config.Policies))
	for i, policy := range s.config.Policies {
		out[i] = clonePolicy(policy)
	}
	return out
}

// Mappings returns a defensive copy of v2 mappings sorted by ID.
func (s *Set) Mappings() []AgentMapping {
	if s == nil {
		return nil
	}
	out := make([]AgentMapping, len(s.config.Mappings))
	for i, mapping := range s.config.Mappings {
		out[i] = cloneAgentMapping(mapping)
	}
	return out
}

// Config returns the normalized secret-free runtime projection.
func (s *Set) Config() Config {
	if s == nil {
		return Config{}
	}
	switch s.config.Version {
	case LegacyVersion:
		return Config{Version: LegacyVersion, Policies: s.Policies()}
	case CurrentVersion:
		return Config{Version: CurrentVersion, Mappings: s.Mappings()}
	default:
		return Config{}
	}
}

// MaxSessionDuration is the validated v1 TTL used by session exchange.
func (p Policy) MaxSessionDuration() time.Duration {
	if p.maxSessionDuration > 0 {
		return p.maxSessionDuration
	}
	ttl, _ := time.ParseDuration(p.MaxSessionTTL)
	return ttl
}

// MaxSessionDuration is the validated v2 TTL used by session exchange.
func (m AgentMapping) MaxSessionDuration() time.Duration {
	if m.maxSessionDuration > 0 {
		return m.maxSessionDuration
	}
	ttl, _ := time.ParseDuration(m.MaxSessionTTL)
	return ttl
}

func clonePolicy(input Policy) Policy {
	out := input
	out.Repositories = make(map[string]RepositoryPolicy, len(input.Repositories))
	for repo, policy := range input.Repositories {
		out.Repositories[repo] = cloneRepositoryPolicy(policy)
	}
	return out
}

func cloneAgentMapping(input AgentMapping) AgentMapping {
	out := input
	out.TaskIDs = append([]string(nil), input.TaskIDs...)
	out.MaxCapabilities = append([]string(nil), input.MaxCapabilities...)
	return out
}

func cloneRepositoryPolicy(input RepositoryPolicy) RepositoryPolicy {
	return RepositoryPolicy{MaxCapabilities: append([]string(nil), input.MaxCapabilities...)}
}
