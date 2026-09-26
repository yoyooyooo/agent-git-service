// Package middleware provides shared HTTP middleware.
package middleware

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	agsauth "github.com/ngaut/agent-git-service/auth"
	"github.com/ngaut/agent-git-service/internal/db"
	applog "github.com/ngaut/agent-git-service/internal/logging"
	"github.com/ngaut/agent-git-service/internal/ratelimit"
	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

// EmbeddedIdentity is the shared trusted host-provided identity shape used
// across the embedding surface, auth middleware, and service resolver.
type EmbeddedIdentity = agsauth.Identity

// EmbeddedIdentityAuthenticator authenticates a request using host-provided
// identity instead of AGS-issued tokens. ok=false means no embedded identity
// was present and token auth should continue if applicable.
type EmbeddedIdentityAuthenticator interface {
	Authenticate(*http.Request) (EmbeddedIdentity, bool, error)
}

type EmbeddedAuthConfig struct {
	Authenticator EmbeddedIdentityAuthenticator
}

// TokenAuth returns middleware that validates GitHub-compatible auth headers.
// Accepts "token <val>" or "Bearer <val>" with any non-empty value.
func TokenAuth(svc *service.Service) func(http.Handler) http.Handler {
	return TokenAuthWithEmbeddedIdentity(svc, EmbeddedAuthConfig{})
}

// TokenAuthWithEmbeddedIdentity returns middleware that first attempts trusted
// host-provided identity injection before falling back to token auth.
func TokenAuthWithEmbeddedIdentity(svc *service.Service, embedded EmbeddedAuthConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			applog.AddAttrs(r.Context(), slog.String("auth_scheme", authScheme(r.Header.Get("Authorization"))))
			if ctx, handled := resolveEmbeddedIdentityAndInjectContext(w, r, svc, embedded, false); handled {
				if ctx == nil {
					return
				}
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			token := ExtractToken(r)
			if token == "" {
				logAuthFailure(r.Context(), "header_missing_or_empty", "", "", nil)
				respond.Error(w, http.StatusUnauthorized, "Requires authentication")
				return
			}

			ctx, shouldReturn := resolveTokenAndInjectContext(w, r, token, svc)
			if shouldReturn {
				return
			}
			if ctx != nil {
				r = r.WithContext(ctx)
			}

			next.ServeHTTP(w, r)
		})
	}
}

// OptionalTokenAuth returns middleware that allows unauthenticated access
// but rejects requests with invalid Authorization headers.
func OptionalTokenAuth(svc *service.Service) func(http.Handler) http.Handler {
	return OptionalTokenAuthWithEmbeddedIdentity(svc, EmbeddedAuthConfig{})
}

// OptionalTokenAuthWithEmbeddedIdentity returns middleware that first attempts
// trusted host-provided identity injection before falling back to the
// historical optional token path.
func OptionalTokenAuthWithEmbeddedIdentity(svc *service.Service, embedded EmbeddedAuthConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ctx, handled := resolveEmbeddedIdentityAndInjectContext(w, r, svc, embedded, true); handled {
				if ctx == nil {
					return
				}
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			auth := r.Header.Get("Authorization")
			if auth == "" {
				ctx := service.ContextWithAnonRequest(r.Context())
				ctx = service.ContextWithRepoCache(ctx)
				r = r.WithContext(ctx)
				applog.AddAttrs(r.Context(), slog.String("auth_mode", "anonymous"))
				next.ServeHTTP(w, r)
				return
			}

			applog.AddAttrs(r.Context(), slog.String("auth_scheme", authScheme(auth)))
			token := ExtractToken(r)
			if token == "" {
				logAuthFailure(r.Context(), "malformed_authorization_header", "", "", nil)
				respond.Error(w, http.StatusUnauthorized, "Bad credentials")
				return
			}

			ctx, shouldReturn := resolveTokenAndInjectContext(w, r, token, svc)
			if shouldReturn {
				return
			}
			if ctx != nil {
				r = r.WithContext(ctx)
			}

			next.ServeHTTP(w, r)
		})
	}
}

func resolveEmbeddedIdentityAndInjectContext(w http.ResponseWriter, r *http.Request, svc *service.Service, embedded EmbeddedAuthConfig, _ bool) (context.Context, bool) {
	if embedded.Authenticator == nil {
		return nil, false
	}
	identity, ok, err := embedded.Authenticator.Authenticate(r)
	if err != nil {
		logAuthFailure(r.Context(), "embedded_identity_auth_failed", "", "embedded", err)
		respond.Error(w, http.StatusUnauthorized, "Bad credentials")
		return nil, true
	}
	if !ok {
		return nil, false
	}
	resolved := service.EmbeddedIdentity{
		Provider:  identity.Provider,
		Subject:   identity.Subject,
		Login:     identity.Login,
		Name:      identity.Name,
		Email:     identity.Email,
		Groups:    append([]string(nil), identity.Groups...),
		SiteAdmin: identity.SiteAdmin,
	}
	user, err := svc.ResolveEmbeddedIdentity(r.Context(), resolved)
	if err != nil {
		logAuthFailure(r.Context(), "embedded_identity_user_resolution_failed", "", "embedded", err)
		respond.Error(w, http.StatusUnauthorized, "Bad credentials")
		return nil, true
	}
	ctx := service.ContextWithUser(r.Context(), user)
	ctx = service.ContextWithRepoCache(ctx)
	if actor := embeddedIdentityActor(resolved); actor != "" {
		ctx = ratelimit.WithActor(ctx, actor)
	}
	applog.AddAttrs(ctx,
		slog.String("auth_mode", "embedded"),
		slog.String("auth_provider", resolved.Provider),
		slog.String("user_login", user.Login),
	)
	return ctx, true
}

func embeddedIdentityActor(identity service.EmbeddedIdentity) string {
	provider := strings.TrimSpace(identity.Provider)
	subject := strings.TrimSpace(identity.Subject)
	if provider == "" || subject == "" {
		return ""
	}
	return "embedded:" + provider + ":" + subject
}

// RequireAuthForWrites returns middleware that rejects unauthenticated
// write requests (POST/PUT/PATCH/DELETE) with 401. GET/HEAD/OPTIONS pass through.
func RequireAuthForWrites(svc *service.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}
			if _, ok := service.UserFromContext(r.Context()); !ok {
				respond.Error(w, http.StatusUnauthorized, "Requires authentication")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// MaxBodySize returns middleware that limits request body size.
// Returns 413 Payload Too Large if the body exceeds maxBytes.
func MaxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return MaxBodySizeUnless(maxBytes, nil)
}

// MaxBodySizeUnless returns middleware that limits request body size unless
// skip returns true for the current request.
func MaxBodySizeUnless(maxBytes int64, skip func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skip != nil && skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ExtractToken extracts the token value from an Authorization header.
// Supports "token <val>", "Bearer <val>", and "Basic <base64>" formats.
// For Basic auth the password portion is used as the token, matching the
// convention used by Git credential helpers (username:token).
func ExtractToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	authTrim := strings.TrimSpace(auth)
	lower := strings.ToLower(authTrim)
	if strings.HasPrefix(lower, "token ") {
		return strings.TrimSpace(authTrim[6:])
	}
	if strings.HasPrefix(lower, "bearer ") {
		return strings.TrimSpace(authTrim[7:])
	}
	if strings.HasPrefix(lower, "basic ") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(authTrim[6:]))
		if err != nil {
			return ""
		}
		// Basic auth format: "username:password" — use password as token
		if _, pass, ok := strings.Cut(string(decoded), ":"); ok && pass != "" {
			return pass
		}
		return ""
	}
	return ""
}

// handleAuthError handles authentication errors consistently.
// Returns true if the request should be rejected.
func handleAuthError(w http.ResponseWriter, r *http.Request, token string, mode string, reason string, err error) bool {
	logAuthFailure(r.Context(), reason, token, mode, err)
	respond.Error(w, http.StatusUnauthorized, "Bad credentials")
	return true
}

// resolveTokenAndInjectContext resolves the token and injects user/DB context.
// Returns (newContext, shouldReturn). If shouldReturn is true, the handler should return immediately.
func resolveTokenAndInjectContext(w http.ResponseWriter, r *http.Request, token string, svc *service.Service) (context.Context, bool) {
	if service.IsClientRunCredential(token) {
		user, run, err := svc.ResolveClientRun(r.Context(), token)
		if err != nil {
			return nil, handleAuthError(w, r, token, "client_run", "invalid_client_run", err)
		}
		ctx := service.ContextWithUser(r.Context(), user)
		ctx = service.ContextWithClientRun(ctx, run)
		ctx = service.ContextWithRepoCache(ctx)
		// Parallel run credentials share an actor budget; creating another
		// association session must not multiply request capacity.
		ctx = ratelimit.WithActor(ctx, "client-run-user:"+strconv.FormatUint(uint64(user.ID), 10))
		if !clientRunCredentialManagementAllowed(r) {
			respond.Error(w, http.StatusForbidden, "A task run credential cannot manage durable account credentials")
			return nil, true
		}
		applog.AddAttrs(ctx, slog.String("auth_mode", "client_run"), slog.String("user_login", user.Login), slog.String("client_run_id", run.ID), slog.String("association_status", run.AssociationStatus))
		return ctx, false
	}
	if service.IsDelegatedSessionCredential(token) {
		user, session, err := svc.ResolveDelegatedSessionCredential(r.Context(), token)
		if err != nil {
			return nil, handleAuthError(w, r, token, "delegated_session", "invalid_session", err)
		}
		ctx := service.ContextWithUser(r.Context(), user)
		ctx = service.ContextWithDelegatedSession(ctx, session)
		ctx = service.ContextWithRepoCache(ctx)
		ctx = ratelimit.WithActor(ctx, "delegated-session:"+session.ID)
		applog.AddAttrs(ctx,
			slog.String("auth_mode", "delegated_session"),
			slog.String("user_login", user.Login),
			slog.String("delegated_session_id", session.ID),
		)
		return ctx, false
	}

	// Validate and resolve user in a single pass to avoid
	// duplicate COUNT(*) and SELECT queries (see #1038).
	u, failure, err := svc.ValidateAndResolveTokenDetailed(r.Context(), token)
	if err != nil || failure != service.TokenValidationFailureNone {
		return nil, handleAuthError(w, r, token, "single_db", string(failure), err)
	}
	svc.TouchToken(r.Context(), token)
	singleCtx := service.ContextWithUser(r.Context(), u)
	singleCtx = service.ContextWithRepoCache(singleCtx)
	if fingerprint := applog.TokenFingerprint(token); fingerprint != "" {
		singleCtx = ratelimit.WithActor(singleCtx, "token:"+fingerprint)
	}
	applog.AddAttrs(singleCtx,
		slog.String("auth_mode", "single_db"),
		slog.String("user_login", u.Login),
	)
	return singleCtx, false
}

// EnforceDelegatedSessionSurface keeps delegated REST access on an exact
// allowlist. Capability and repository checks still run in the service layer.
// Durable credentials and anonymous requests are unaffected.
func EnforceDelegatedSessionSurface(svc *service.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			session, ok := service.DelegatedSessionFromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			if delegatedSessionRequestAllowed(session, r.Method, r.URL.Path) || delegatedSessionGraphQLRequestAllowed(session, r) {
				next.ServeHTTP(w, r)
				return
			}
			if svc != nil {
				_ = svc.LogCurrentDelegatedSessionAudit(r.Context(), service.DelegatedSessionAuditEvent{
					Action: service.AuditActionDelegatedWriteDenied, Operation: r.Method + " " + strings.TrimSuffix(r.URL.Path, "/"),
					Outcome: "denied", Reason: "surface_not_allowed",
				})
			}
			respond.Error(w, http.StatusForbidden, "Delegated session capability does not allow this operation")
		})
	}
}

// EnforceDelegatedSessionReadSurface remains as an internal compatibility
// wrapper for callers that have not yet adopted audit-aware surface wiring.
func EnforceDelegatedSessionReadSurface() func(http.Handler) http.Handler {
	return EnforceDelegatedSessionSurface(nil)
}

func delegatedSessionRequestAllowed(session db.DelegatedAgentSession, method, path string) bool {
	exactPath := path
	path = strings.TrimSuffix(path, "/")
	if method == http.MethodGet && path == "/api/v3/user" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(session.OperationName)) {
	case "repo.read":
		return delegatedExactVerificationRead(method, path) || delegatedRepoBranchProtectionRead(method, exactPath)
	case "pr.read":
		return delegatedExactVerificationRead(method, path) || delegatedPRListRead(method, path) || delegatedProviderEvidenceRead(method, path, "projection")
	case "review.read":
		return delegatedReviewCIVerificationRead(method, path)
	case "ci.read":
		return delegatedReviewCIVerificationRead(method, path) || delegatedProviderEvidenceRead(method, path, "ci")
	case "pr.create":
		return delegatedExactVerificationRead(method, path) || delegatedPRCreate(method, path)
	case "pr.comment":
		return delegatedExactVerificationRead(method, path) || delegatedPRComment(method, path)
	case "pr.edit", "pr.close", "pr.reopen":
		// Handler body distinguishes edit vs close vs reopen; middleware only
		// admits the exact PATCH pull route for the bound operation.
		return delegatedExactVerificationRead(method, path) || delegatedPRPatch(method, path)
	case "review.write", "review.submit":
		return delegatedExactVerificationRead(method, path) || delegatedPRReviewWrite(method, path)
	case "pr.rebase":
		return delegatedExactVerificationRead(method, path) || delegatedPRRebaseAction(method, path)
	case "git.read", "git.push":
		// Git transport is authorized and revalidated by githttp immediately
		// before upload-pack/receive-pack, never by the REST surface.
		return false
	default:
		return false
	}
}

func delegatedPRListRead(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	return len(parts) == 6 && delegatedRepositoryPath(parts) && parts[5] == "pulls"
}

func delegatedExactVerificationRead(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) == 5 && delegatedRepositoryPath(parts) {
		return true
	}
	if len(parts) == 7 && delegatedRepositoryPath(parts) && parts[5] == "pulls" {
		_, err := strconv.ParseUint(parts[6], 10, 64)
		return err == nil && parts[6] != "0"
	}
	return len(parts) >= 9 && delegatedRepositoryPath(parts) && parts[5] == "git" && parts[6] == "ref" && parts[7] == "heads" && delegatedHeadRefAllowed(strings.Join(parts[8:], "/"))
}

func delegatedRepoBranchProtectionRead(method, path string) bool {
	if method != http.MethodGet || path != strings.TrimSuffix(path, "/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 8 || !delegatedRepositoryPath(parts) || parts[5] != "branches" || parts[len(parts)-1] != "protection" {
		return false
	}
	return delegatedHeadRefAllowed(strings.Join(parts[6:len(parts)-1], "/"))
}

func delegatedReviewCIVerificationRead(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) == 5 && delegatedRepositoryPath(parts) {
		return true
	}
	if len(parts) == 7 && delegatedRepositoryPath(parts) && parts[5] == "pulls" {
		number, err := strconv.ParseUint(parts[6], 10, 64)
		return err == nil && number > 0
	}
	return false
}

func delegatedProviderEvidenceRead(method, path, capability string) bool {
	if method != http.MethodGet {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if !delegatedRepositoryPath(parts) || len(parts) < 9 || parts[5] != "pulls" || parts[7] != "provider" {
		return false
	}
	number, err := strconv.ParseUint(parts[6], 10, 64)
	if err != nil || number == 0 {
		return false
	}
	switch capability {
	case "projection":
		return len(parts) == 9 && parts[8] == "projection"
	case "ci":
		if len(parts) == 10 && parts[8] == "ci" && parts[9] == "runs" {
			return true
		}
		if len(parts) == 12 && parts[8] == "ci" && parts[9] == "runs" && parts[11] == "logs" {
			_, err := strconv.ParseUint(parts[10], 10, 64)
			return err == nil && parts[10] != "0"
		}
		return false
	default:
		return false
	}
}

func delegatedPRCreate(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	return len(parts) == 6 && parts[0] == "api" && parts[1] == "v3" && parts[2] == "repos" && parts[3] != "" && parts[4] != "" && parts[5] == "pulls"
}

func delegatedPRComment(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if !delegatedRepositoryPath(parts) {
		return false
	}
	// POST /api/v3/repos/{owner}/{repo}/issues/{n}/comments
	if len(parts) == 8 && parts[5] == "issues" && parts[7] == "comments" {
		number, err := strconv.ParseUint(parts[6], 10, 64)
		return err == nil && number > 0
	}
	// POST /api/v3/repos/{owner}/{repo}/pulls/{n}/comments
	if len(parts) == 8 && parts[5] == "pulls" && parts[7] == "comments" {
		number, err := strconv.ParseUint(parts[6], 10, 64)
		return err == nil && number > 0
	}
	return false
}

func delegatedPRPatch(method, path string) bool {
	if method != http.MethodPatch {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 7 || !delegatedRepositoryPath(parts) || parts[5] != "pulls" {
		return false
	}
	number, err := strconv.ParseUint(parts[6], 10, 64)
	return err == nil && number > 0
}

func delegatedPRReviewWrite(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	// POST /api/v3/repos/{owner}/{repo}/pulls/{n}/reviews
	if len(parts) != 8 || !delegatedRepositoryPath(parts) || parts[5] != "pulls" || parts[7] != "reviews" {
		return false
	}
	number, err := strconv.ParseUint(parts[6], 10, 64)
	return err == nil && number > 0
}

func delegatedPRRebaseAction(method, path string) bool {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if method == http.MethodGet && len(parts) == 6 && parts[0] == "api" && parts[1] == "v3" && parts[2] == "agent-sessions" && parts[3] == "current" && parts[4] == "authority-boundary-receipts" {
		return parts[5] != ""
	}
	if len(parts) < 9 || parts[0] != "api" || parts[1] != "v3" || parts[2] != "repos" || parts[3] == "" || parts[4] == "" || parts[5] != "pulls" || parts[7] != "actions" || parts[8] != "pr.rebase" {
		return false
	}
	if number, err := strconv.ParseUint(parts[6], 10, 64); err != nil || number == 0 {
		return false
	}
	if method == http.MethodPost {
		return len(parts) == 9
	}
	return method == http.MethodGet && len(parts) == 10 && strings.TrimSpace(parts[9]) != ""
}

func delegatedRepositoryPath(parts []string) bool {
	return len(parts) >= 5 && parts[0] == "api" && parts[1] == "v3" && parts[2] == "repos" && parts[3] != "" && parts[4] != ""
}

func delegatedHeadRefAllowed(ref string) bool {
	if ref == "" || ref == "@" || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") || strings.Contains(ref, "//") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.ContainsAny(ref, `\\~^:?*[]`) {
		return false
	}
	for _, part := range strings.Split(ref, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for _, char := range ref {
		if char <= ' ' || char == 0x7f {
			return false
		}
	}
	return true
}

func authScheme(auth string) string {
	auth = strings.TrimSpace(strings.ToLower(auth))
	switch {
	case auth == "":
		return "none"
	case strings.HasPrefix(auth, "token "):
		return "token"
	case strings.HasPrefix(auth, "bearer "):
		return "bearer"
	case strings.HasPrefix(auth, "basic "):
		return "basic"
	default:
		return "unknown"
	}
}

func logAuthFailure(ctx context.Context, reason string, token string, mode string, err error) {
	attrs := []any{
		"auth_reason", reason,
	}
	if mode != "" {
		attrs = append(attrs, "auth_mode", mode)
	}
	if fingerprint := applog.TokenFingerprint(token); fingerprint != "" {
		attrs = append(attrs, "token_fingerprint", fingerprint)
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	slog.WarnContext(ctx, "authentication failed", attrs...)
}
