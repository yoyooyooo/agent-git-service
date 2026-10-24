package forgejointegration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/attribution"
	"github.com/ngaut/agent-git-service/internal/gittransport"
)

// Principal is evidence for the selected credential, never an authority grant.
// Unknown is null, not the actor, TokenOwner or IntegrationBot.
type Principal struct {
	ID     *int64  `json:"authenticated_principal_id"`
	Login  *string `json:"authenticated_login"`
	Status string  `json:"identity_status"`
	Source string  `json:"identity_source"`
}

func unknownPrincipal() Principal { return Principal{Status: "unknown", Source: "none"} }

var diagnosticLogin = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)

func principal(id int64, login, source string) Principal {
	if id <= 0 || !diagnosticLogin.MatchString(login) {
		return unknownPrincipal()
	}
	return Principal{ID: &id, Login: &login, Status: "verified", Source: source}
}

// These probes are read-only and never go through attributedRequest (Sudo,
// fallback comments, key registration and ACL projection are not diagnostics).
func (c *HTTPClient) readPrincipal(ctx context.Context, login string) Principal {
	path := "/api/v1/user"
	source := "credential_user_get"
	if login != "" {
		path = apiPath("/api/v1/users/%s", login)
		source = "selected_key_owner_get"
	}
	// Identity evidence must come from this exact credential endpoint, not
	// a redirect to a different origin/account. Copy the client, not its cache.
	probe := *c
	probe.client = noRedirectClient(c.client)
	status, data, err := probe.rawRequest(ctx, http.MethodGet, path, nil, "")
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err != nil || status != 200 || json.Unmarshal(data, &user) != nil || login != "" && user.Login != login {
		return unknownPrincipal()
	}
	return principal(user.ID, user.Login, source)
}
func (i *Integration) pushPrincipal(ctx context.Context, req PushRequest) Principal {
	// An injected pusher or non-HTTP transport cannot be attested by HTTP /user.
	u, err := url.Parse(req.RemoteURL)
	if !i.nativePush || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User == nil {
		return unknownPrincipal()
	}
	base, baseErr := url.Parse(i.cfg.BaseURL)
	if baseErr != nil || base.Scheme != u.Scheme || base.Host != u.Host {
		return unknownPrincipal()
	}
	token, ok := u.User.Password()
	if !ok || token == "" || token != i.cfg.Token {
		return unknownPrincipal()
	}
	return NewHTTPClient(i.cfg.BaseURL, token).readPrincipal(ctx, "")
}
func noRedirectClient(c *http.Client) *http.Client {
	client := *c
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func selectedSSHPrincipal(ctx context.Context, c *HTTPClient, k managedKey) Principal {
	probe := *c
	probe.client = noRedirectClient(c.client)
	keys, err := listManagedKeys(ctx, &probe, k.Login)
	if err != nil {
		return unknownPrincipal()
	}
	for _, key := range keys {
		if samePublic(key.Key, k.Public) && key.Title == managedTitle(k) {
			p := c.readPrincipal(ctx, k.Login)
			if p.Status == "verified" {
				p.Source = "selected_key_binding"
			}
			return p
		}
	}
	return unknownPrincipal()
}

type diagnosticContextKey struct{}
type DiagnosticLink struct {
	JobID              uint   `json:"job_id,omitempty"`
	Attempt            int    `json:"attempt,omitempty"`
	Phase              string `json:"phase,omitempty"`
	SourceRepositoryID uint   `json:"source_repository_id,omitempty"`
	ExpectedNewOID     string `json:"expected_new_oid,omitempty"`
}

// WithProjectionDiagnosticLink associates one attempt without persisting a new
// diagnostics row. It cannot change a claim, retry policy or terminal state.
func WithProjectionDiagnosticLink(ctx context.Context, link DiagnosticLink) context.Context {
	return context.WithValue(ctx, diagnosticContextKey{}, link)
}

// ProjectionDiagnostic is the single allowlisted immediate-log DTO. Do not
// marshal errors, PushRequest, provider bodies or configuration in its place.
type ProjectionDiagnostic struct {
	SchemaVersion int    `json:"schema_version"`
	Provider      string `json:"provider"`
	DiagnosticLink
	RequestID  string `json:"request_id,omitempty"`
	ActorID    uint   `json:"actor_id,omitempty"`
	ActorLogin string `json:"actor_login,omitempty"`
	Principal
	Transport          string  `json:"transport"`
	Ref                string  `json:"ref,omitempty"`
	TargetRepositoryID *int64  `json:"target_repository_id"` // no immutable observation in legacy push
	ExpectedOldOID     *string `json:"expected_old_oid"`
	ObservedOID        *string `json:"observed_oid"` // immediate transport log is not exact readback
	OperationID        *string `json:"operation_id"` // reserved; no operation runtime in S0/S1
	ItemID             *string `json:"item_id"`
	EffectID           *string `json:"effect_id"`
	FenceEpoch         *uint64 `json:"fence_epoch"`
	gittransport.Failure
	EffectStatus string    `json:"effect_status"`
	ObservedAt   time.Time `json:"observed_at"`
}

// LogValue keeps text and JSON slog handlers on the same field allowlist and
// nullable identity semantics instead of formatting a Go struct or pointer.
func (d ProjectionDiagnostic) LogValue() slog.Value {
	data, _ := json.Marshal(d)
	var fields map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	_ = decoder.Decode(&fields)
	return slog.AnyValue(fields)
}

func projectionDiagnostic(ctx context.Context, req PushRequest, p Principal, result error) ProjectionDiagnostic {
	link, _ := ctx.Value(diagnosticContextKey{}).(DiagnosticLink)
	// Phase is server-owned; reject an accidental raw string rather than escaping
	// and persisting credentials inside a perfectly valid JSON string.
	switch link.Phase {
	case "preflight", "mirroring_branch", "projecting_pr", "verifying", "push":
	default:
		link.Phase = "push"
	}
	if !isFullGitObjectID(link.ExpectedNewOID) || strings.TrimSpace(link.ExpectedNewOID) != link.ExpectedNewOID {
		link.ExpectedNewOID = ""
	}
	actor := attribution.FromContext(ctx)
	actorLogin, requestID := "", ""
	if diagnosticLogin.MatchString(actor.Login) {
		actorLogin = actor.Login
	}
	if diagnosticLogin.MatchString(actor.RequestID) {
		requestID = actor.RequestID
	}
	d := ProjectionDiagnostic{SchemaVersion: 1, Provider: "forgejo", DiagnosticLink: link, RequestID: requestID, ActorID: actor.UserID, ActorLogin: actorLogin, Principal: p, Transport: "unknown", EffectStatus: "unknown", ObservedAt: time.Now().UTC()}
	u, err := url.Parse(req.RemoteURL)
	if err == nil {
		switch u.Scheme {
		case "http", "https", "ssh", "file":
			d.Transport = u.Scheme
		}
	}
	if req.SSH != nil {
		d.Transport = "ssh"
	}
	if isFullGitObjectID(req.ForceWithLeaseSHA) && strings.TrimSpace(req.ForceWithLeaseSHA) == req.ForceWithLeaseSHA {
		old := req.ForceWithLeaseSHA
		d.ExpectedOldOID = &old
	}
	if req.BranchName != "" {
		ref := "refs/heads/" + req.BranchName
		if safeDiagnosticRef(ref) {
			d.Ref = ref
		}
	}
	if result == nil {
		d.Failure = gittransport.Failure{Category: "none", ReasonCode: "none", EvidenceSource: "process", RetryAdvice: "none"}
		d.EffectStatus = "transport_succeeded_unverified"
	} else {
		d.Failure = projectionFailure(result)
	}
	return d
}
func safeDiagnosticRef(ref string) bool {
	if !strings.HasPrefix(ref, "refs/heads/") || len(ref) > 512 || strings.HasSuffix(ref, "/") {
		return false
	}
	for _, c := range ref {
		if c <= 32 || c >= 127 || strings.ContainsRune(`~^:?*[\\`, c) {
			return false
		}
	}
	return !strings.Contains(ref, "..") && !strings.Contains(ref, "@{") && !strings.Contains(ref, "//")
}
func projectionFailure(err error) gittransport.Failure {
	var h *ProviderHTTPError
	if errors.As(err, &h) {
		return httpFailure(h)
	}
	return gittransport.Describe(err)
}
