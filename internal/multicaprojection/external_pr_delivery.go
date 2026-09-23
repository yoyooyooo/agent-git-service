package multicaprojection

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ExternalPRTerminalDeliverySchema identifies the AGS-private durable row
// wrapper. It is not an HTTP schema: the wire body remains the already
// deployed ExternalPRLinkRequest.
const ExternalPRTerminalDeliverySchema = "ags.multica-external-pr-projection.v1"

const (
	ExternalPRTerminalDeliveryMergedPath = "/api/integrations/external-pr/complete-from-merge"
	ExternalPRTerminalDeliveryClosedPath = "/api/integrations/external-pr/links"
)

var (
	canonicalGitSHA        = regexp.MustCompile(`^[0-9a-f]{40}$`)
	canonicalBindingDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ExternalPRTerminalDeliveryTarget identifies the configured Multica instance
// in the AGS-private durable wrapper. It is never sent on the wire.
type ExternalPRTerminalDeliveryTarget struct {
	Kind       string `json:"kind"`
	InstanceID string `json:"instance_id"`
	Workspace  string `json:"workspace_id"`
	Issue      string `json:"issue_id"`
}

// ExternalPRTerminalDeliverySource identifies the AGS source fact in the
// durable row. It contains no credential or raw provider response.
type ExternalPRTerminalDeliverySource struct {
	AGSPrID    string `json:"ags_pr_id"`
	ObservedAt string `json:"observed_at"`
}

// ExternalPRTerminalDelivery is an AGS-private typed wrapper. Request is the
// exact closed ExternalPRLinkRequest that the dispatcher sends to Multica.
type ExternalPRTerminalDelivery struct {
	Schema         string                           `json:"schema"`
	DeliveryID     string                           `json:"delivery_id"`
	IdempotencyKey string                           `json:"idempotency_key"`
	Target         ExternalPRTerminalDeliveryTarget `json:"target"`
	Request        ExternalPRLinkRequest            `json:"request"`
	Source         ExternalPRTerminalDeliverySource `json:"source"`
}

// ExternalPRTerminalDeliveryInput is the secret-free input used by AGS to
// construct one deterministic terminal delivery.
type ExternalPRTerminalDeliveryInput struct {
	TargetInstance string
	Request        ExternalPRLinkRequest
	AGSPrID        string
	ObservedAt     time.Time
}

// NewExternalPRTerminalDelivery validates and constructs a deterministic
// durable wrapper. The identity key deliberately excludes mutable projection
// details, so a changed payload for the same terminal identity is a conflict,
// not a second delivery row.
func NewExternalPRTerminalDelivery(input ExternalPRTerminalDeliveryInput) (ExternalPRTerminalDelivery, error) {
	instance := strings.TrimSpace(input.TargetInstance)
	if instance == "" {
		return ExternalPRTerminalDelivery{}, fmt.Errorf("external PR terminal delivery target instance is required")
	}
	request := normalizeExternalPRLinkRequest(input.Request)
	if err := validateExternalPRLinkRequest(request, instance, false); err != nil {
		return ExternalPRTerminalDelivery{}, err
	}
	expectedAGSPrID := fmt.Sprintf("%s#%d", request.ExternalRepo, request.ExternalNumber)
	if strings.TrimSpace(input.AGSPrID) == "" || strings.TrimSpace(input.AGSPrID) != expectedAGSPrID {
		return ExternalPRTerminalDelivery{}, fmt.Errorf("external PR terminal delivery AGS PR identity does not match the canonical request")
	}
	if input.ObservedAt.IsZero() {
		input.ObservedAt = time.Now().UTC()
	}

	digest, err := externalPRTerminalIdentityDigest(instance, request)
	if err != nil {
		return ExternalPRTerminalDelivery{}, err
	}
	key := "external-pr-terminal:v1:" + hex.EncodeToString(digest[:])
	request.IdempotencyKey = key
	request = normalizeExternalPRLinkRequest(request)
	if err := validateExternalPRLinkRequest(request, instance, true); err != nil {
		return ExternalPRTerminalDelivery{}, err
	}
	return ExternalPRTerminalDelivery{
		Schema:         ExternalPRTerminalDeliverySchema,
		DeliveryID:     deterministicUUID(digest[:]),
		IdempotencyKey: key,
		Target: ExternalPRTerminalDeliveryTarget{
			Kind:       "multica",
			InstanceID: instance,
			Workspace:  request.WorkspaceID,
			Issue:      request.IssueID,
		},
		Request: request,
		Source: ExternalPRTerminalDeliverySource{
			AGSPrID:    strings.TrimSpace(input.AGSPrID),
			ObservedAt: input.ObservedAt.UTC().Format(time.RFC3339),
		},
	}, nil
}

// MarshalExternalPRTerminalDelivery returns the persisted secret-free wrapper.
func MarshalExternalPRTerminalDelivery(delivery ExternalPRTerminalDelivery) (string, error) {
	data, err := json.Marshal(delivery)
	if err != nil {
		return "", fmt.Errorf("marshal external PR terminal delivery: %w", err)
	}
	return string(data), nil
}

// DecodeExternalPRTerminalDelivery performs a closed decode of the AGS-private
// wrapper. It is intentionally stricter than json.Unmarshal and rejects
// trailing JSON and unknown fields before any HTTP request is made.
func DecodeExternalPRTerminalDelivery(data []byte) (ExternalPRTerminalDelivery, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var delivery ExternalPRTerminalDelivery
	if err := decoder.Decode(&delivery); err != nil {
		return ExternalPRTerminalDelivery{}, fmt.Errorf("decode external PR terminal delivery: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return ExternalPRTerminalDelivery{}, fmt.Errorf("decode external PR terminal delivery: trailing JSON")
		}
		return ExternalPRTerminalDelivery{}, fmt.Errorf("decode external PR terminal delivery trailing data: %w", err)
	}
	if delivery.Schema != ExternalPRTerminalDeliverySchema {
		return ExternalPRTerminalDelivery{}, fmt.Errorf("external PR terminal delivery schema is invalid")
	}
	if strings.TrimSpace(delivery.DeliveryID) == "" || strings.TrimSpace(delivery.IdempotencyKey) == "" {
		return ExternalPRTerminalDelivery{}, fmt.Errorf("external PR terminal delivery identity is incomplete")
	}
	if delivery.Target.Kind != "multica" || strings.TrimSpace(delivery.Target.InstanceID) == "" || strings.TrimSpace(delivery.Target.Workspace) == "" || strings.TrimSpace(delivery.Target.Issue) == "" {
		return ExternalPRTerminalDelivery{}, fmt.Errorf("external PR terminal delivery target is incomplete")
	}
	if delivery.Target.Workspace != delivery.Request.WorkspaceID || delivery.Target.Issue != delivery.Request.IssueID || delivery.IdempotencyKey != delivery.Request.IdempotencyKey {
		return ExternalPRTerminalDelivery{}, fmt.Errorf("external PR terminal delivery target or idempotency binding conflicts")
	}
	digest, err := externalPRTerminalIdentityDigest(delivery.Target.InstanceID, delivery.Request)
	if err != nil {
		return ExternalPRTerminalDelivery{}, err
	}
	if delivery.IdempotencyKey != "external-pr-terminal:v1:"+hex.EncodeToString(digest[:]) || delivery.DeliveryID != deterministicUUID(digest[:]) {
		return ExternalPRTerminalDelivery{}, fmt.Errorf("external PR terminal delivery identity is not deterministic")
	}
	if strings.TrimSpace(delivery.Source.AGSPrID) == "" || strings.TrimSpace(delivery.Source.AGSPrID) != fmt.Sprintf("%s#%d", delivery.Request.ExternalRepo, delivery.Request.ExternalNumber) || strings.TrimSpace(delivery.Source.ObservedAt) == "" {
		return ExternalPRTerminalDelivery{}, fmt.Errorf("external PR terminal delivery source is incomplete or does not match the canonical request")
	}
	if _, err := time.Parse(time.RFC3339, delivery.Source.ObservedAt); err != nil {
		return ExternalPRTerminalDelivery{}, fmt.Errorf("external PR terminal delivery source timestamp is invalid")
	}
	if err := validateExternalPRLinkRequest(delivery.Request, delivery.Target.InstanceID, true); err != nil {
		return ExternalPRTerminalDelivery{}, err
	}
	return delivery, nil
}

// MarshalExternalPRLinkRequest returns the exact closed JSON body used by the
// deployed Multica /links and /complete-from-merge handlers.
func MarshalExternalPRLinkRequest(request ExternalPRLinkRequest) ([]byte, error) {
	request = normalizeExternalPRLinkRequest(request)
	if err := validateExternalPRLinkRequest(request, request.TargetInstance, true); err != nil {
		return nil, err
	}
	return json.Marshal(request)
}

// ExternalPRTerminalPath returns the only allowed path for a terminal state.
func ExternalPRTerminalPath(state string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "merged":
		return ExternalPRTerminalDeliveryMergedPath, nil
	case "closed":
		return ExternalPRTerminalDeliveryClosedPath, nil
	default:
		return "", fmt.Errorf("external PR terminal state is unsupported")
	}
}

func externalPRTerminalIdentityDigest(targetInstance string, request ExternalPRLinkRequest) ([32]byte, error) {
	identityJSON, err := json.Marshal(struct {
		TargetInstance string `json:"target_instance"`
		WorkspaceID    string `json:"workspace_id"`
		IssueID        string `json:"issue_id"`
		Provider       string `json:"provider"`
		ExternalRepo   string `json:"external_repo"`
		ExternalNumber int    `json:"external_number"`
		State          string `json:"state"`
		MergedSHA      string `json:"merged_sha"`
	}{strings.TrimSpace(targetInstance), request.WorkspaceID, request.IssueID, request.Provider, request.ExternalRepo, request.ExternalNumber, request.State, request.MergedSHA})
	if err != nil {
		return [32]byte{}, fmt.Errorf("marshal external PR terminal identity: %w", err)
	}
	return sha256.Sum256(identityJSON), nil
}

func normalizeExternalPRLinkRequest(request ExternalPRLinkRequest) ExternalPRLinkRequest {
	request.Provider = strings.ToLower(strings.TrimSpace(request.Provider))
	request.IssueID = strings.TrimSpace(request.IssueID)
	request.WorkspaceID = strings.TrimSpace(request.WorkspaceID)
	request.Workspace = strings.Trim(strings.ToLower(strings.TrimSpace(request.Workspace)), "/")
	request.IssueKey = strings.ToUpper(strings.TrimSpace(request.IssueKey))
	request.ExternalRepo = strings.TrimSpace(request.ExternalRepo)
	request.ExternalURL = strings.TrimSpace(request.ExternalURL)
	request.MergeProvider = strings.ToLower(strings.TrimSpace(request.MergeProvider))
	request.MergeRepo = strings.TrimSpace(request.MergeRepo)
	request.MergeURL = strings.TrimSpace(request.MergeURL)
	request.MergedSHA = strings.TrimSpace(request.MergedSHA)
	request.TargetInstance = strings.TrimSpace(request.TargetInstance)
	request.CanonicalRepositoryID = strings.TrimSpace(request.CanonicalRepositoryID)
	request.CanonicalRepository = strings.TrimSpace(request.CanonicalRepository)
	request.ProviderBindingID = strings.TrimSpace(request.ProviderBindingID)
	request.ProviderBindingRevision = strings.TrimSpace(request.ProviderBindingRevision)
	request.ProviderRepository = strings.TrimSpace(request.ProviderRepository)
	request.ExpectedHeadSHA = strings.ToLower(strings.TrimSpace(request.ExpectedHeadSHA))
	request.ExpectedBaseSHA = strings.ToLower(strings.TrimSpace(request.ExpectedBaseSHA))
	request.BaseRef = strings.TrimSpace(request.BaseRef)
	request.DelegatedMergeMethod = strings.TrimSpace(request.DelegatedMergeMethod)
	request.ProjectionFactsRevision = strings.TrimSpace(request.ProjectionFactsRevision)
	request.LinkConfidence = strings.TrimSpace(request.LinkConfidence)
	request.State = strings.ToLower(strings.TrimSpace(request.State))
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	return request
}

func validateExternalPRLinkRequest(request ExternalPRLinkRequest, targetInstance string, requireKey bool) error {
	request = normalizeExternalPRLinkRequest(request)
	targetInstance = strings.TrimSpace(targetInstance)
	if request.Provider != "ags" {
		return fmt.Errorf("external PR terminal request provider must be ags")
	}
	if request.IssueID == "" || request.WorkspaceID == "" || request.Workspace == "" || request.IssueKey == "" || request.ExternalRepo == "" || request.ExternalNumber <= 0 {
		return fmt.Errorf("external PR terminal request identity is incomplete")
	}
	if request.State != "merged" && request.State != "closed" {
		return fmt.Errorf("external PR terminal request state is not terminal")
	}
	if request.MergeProvider != "forgejo" || request.MergeRepo == "" || request.MergeNumber <= 0 || !validHTTPURL(request.MergeURL) {
		return fmt.Errorf("external PR terminal request Forgejo merge facts are incomplete")
	}
	if request.LinkConfidence != "authoritative" {
		return fmt.Errorf("external PR terminal request link confidence must be authoritative")
	}
	if !validHTTPURL(request.ExternalURL) {
		return fmt.Errorf("external PR terminal request AGS URL is invalid")
	}
	if request.State == "merged" {
		if !request.CompletionIntent {
			return fmt.Errorf("merged terminal request requires completion intent")
		}
		if !canonicalGitSHA.MatchString(request.MergedSHA) {
			return fmt.Errorf("merged terminal request requires a canonical merge SHA")
		}
	} else {
		if request.CompletionIntent {
			return fmt.Errorf("closed-unmerged terminal request cannot claim completion")
		}
		if request.MergedSHA != "" {
			return fmt.Errorf("closed-unmerged terminal request cannot carry a merge SHA")
		}
	}
	if requireKey && request.IdempotencyKey == "" {
		return fmt.Errorf("external PR terminal request idempotency key is required")
	}
	bindingValues := []string{
		request.TargetInstance, request.CanonicalRepositoryID, request.CanonicalRepository,
		request.ProviderBindingID, request.ProviderBindingRevision, request.ProviderRepository,
		request.ExpectedHeadSHA, request.ExpectedBaseSHA, request.BaseRef, request.DelegatedMergeMethod,
		request.ProjectionFactsRevision,
	}
	present := 0
	for _, value := range bindingValues {
		if strings.TrimSpace(value) != "" {
			present++
		}
	}
	if present != 0 && present != len(bindingValues) {
		return fmt.Errorf("external PR terminal request projection binding must be complete or omitted")
	}
	if present == len(bindingValues) {
		if targetInstance == "" || request.TargetInstance != targetInstance || request.ProviderRepository != request.MergeRepo ||
			!canonicalBindingDigest.MatchString(request.CanonicalRepositoryID) ||
			!canonicalBindingDigest.MatchString(request.ProviderBindingID) ||
			!canonicalBindingDigest.MatchString(request.ProviderBindingRevision) ||
			!canonicalBindingDigest.MatchString(request.ProjectionFactsRevision) ||
			!canonicalGitSHA.MatchString(request.ExpectedHeadSHA) || !canonicalGitSHA.MatchString(request.ExpectedBaseSHA) ||
			request.ProjectionFactsRevision == "" {
			return fmt.Errorf("external PR terminal request projection binding is invalid")
		}
	}
	return nil
}

func validHTTPURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	return err == nil && parsed.User == nil && parsed.Host != "" && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == "" && !strings.Contains(raw, "#") && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func deterministicUUID(digest []byte) string {
	if len(digest) < 16 {
		return ""
	}
	b := append([]byte(nil), digest[:16]...)
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16]))
}
