// Package githttp serves the Git Smart HTTP protocol backed by system git-http-backend.
// Routes:
//
//	GET  /{owner}/{repo}.git/info/refs?service=git-{upload,receive}-pack
//	POST /{owner}/{repo}.git/git-upload-pack
//	POST /{owner}/{repo}.git/git-receive-pack
package githttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ngaut/agent-git-service/internal/gitbackend"
	"github.com/ngaut/agent-git-service/internal/gitstore"
	applog "github.com/ngaut/agent-git-service/internal/logging"
	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

const wikiReceivePackRepairOwnerRefreshInterval = 15 * time.Minute

// Store defines the git storage operations required by the HTTP handler.
type Store interface {
	Exists(ctx context.Context, fullName string) bool
	Init(ctx context.Context, fullName, defaultBranch string, seed bool) error
	GetRepoPath(ctx context.Context, fullName string) (string, error)
	RepoRoot(ctx context.Context) (string, error)
	WithRepoLock(ctx context.Context, fullName string, fn func() error) error
}

type receivePolicyEnsurer interface {
	EnsureReceivePolicy(ctx context.Context, fullName string) error
}

// Handler wraps the gitstore for HTTP git protocol serving.
type Handler struct {
	store Store
	Svc   *service.Service
}

type serveRequest struct {
	projectRoot                string
	repoPath                   string
	svcName                    string
	advertise                  bool
	repoFullName               string
	allowDeleteCurrent         bool
	gitHTTPProtectedBranches   []string
	delegatedSession           bool
	delegatedProtectedBranches []string
}

// repoContext holds the resolved repository context for serving git HTTP requests.
type repoContext struct {
	projectRoot     string
	repoPath        string
	repoFullName    string
	gitRepoFullName string
	repositoryID    uint
	defaultBranch   string
	isWikiBacking   bool
}

type bufferedResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newBufferedResponseWriter() *bufferedResponseWriter {
	return &bufferedResponseWriter{header: make(http.Header)}
}

func (w *bufferedResponseWriter) Header() http.Header {
	return w.header
}

func (w *bufferedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *bufferedResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(body)
}

func (w *bufferedResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
}

func (w *bufferedResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *bufferedResponseWriter) writeTo(dst http.ResponseWriter) error {
	for key, values := range w.header {
		for _, value := range values {
			dst.Header().Add(key, value)
		}
	}
	dst.WriteHeader(w.statusCode())
	_, err := io.Copy(dst, &w.body)
	return err
}

func gitAuthChallenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
	respond.Error(w, http.StatusUnauthorized, "Requires authentication")
}

// New creates a new git HTTP handler.
func New(store Store, svc *service.Service) *Handler {
	return &Handler{store: store, Svc: svc}
}

// ensureRepo creates missing storage and refreshes managed receive hooks on
// existing repositories. Hook refresh is fail-closed: serving Git while the
// authority guard cannot be installed would silently weaken repository policy.
func (h *Handler) ensureRepo(ctx context.Context, fullName, defaultBranch string) error {
	if h.store.Exists(ctx, fullName) {
		if ensurer, ok := h.store.(receivePolicyEnsurer); ok {
			return ensurer.EnsureReceivePolicy(ctx, fullName)
		}
		return nil
	}
	if err := h.store.Init(ctx, fullName, defaultBranch, false); err != nil {
		return err
	}
	return nil
}

// resolveRepoContext resolves and prepares repository context for git HTTP serving.
// It handles repo lookup, ensures repo exists, and fetches repo path information.
// Returns repoContext on success, or writes error response and returns false on failure.
func (h *Handler) resolveRepoContext(w http.ResponseWriter, r *http.Request, action, gitService string, required service.RepoPermission) (*repoContext, bool) {
	owner := pathParam(r, "owner")
	repo := strings.TrimSuffix(pathParam(r, "repo"), ".git")
	requested := owner + "/" + repo
	applog.AddAttrs(r.Context(), slog.String("repo", requested))

	rep, err := h.Svc.AuthorizeGitTransport(r.Context(), requested, gitService)
	if err != nil {
		var denied *service.GitTransportAccessError
		if errors.As(err, &denied) {
			switch denied.Stage {
			case service.GitAccessLookup:
				if errors.Is(err, service.ErrNotFound) {
					if _, wiki := wikiRepoParentName(requested); wiki {
						return h.resolveWikiRepoContext(w, r, action, gitService, requested)
					}
				}
			case service.GitAccessOperation:
				respond.Error(w, http.StatusBadRequest, "Unsupported Git service")
				return nil, false
			case service.GitAccessAuthentication:
				gitAuthChallenge(w)
				return nil, false
			case service.GitAccessDelegated:
				h.logDelegatedGitOperation(r.Context(), gitService, "denied", service.DelegatedSessionDenialReason(err))
				respond.Error(w, http.StatusForbidden, "Delegated session operation does not allow this Git transport")
				return nil, false
			case service.GitAccessPermission:
				if required.Effective() == service.RepoPermissionWrite {
					h.logDelegatedGitWrite(r.Context(), "denied", "permission_denied")
				}
			}
		}
		switch {
		case errors.Is(err, service.ErrForbidden):
			respond.Error(w, http.StatusForbidden, "Delegated session capability does not allow this Git operation")
		case errors.Is(err, service.ErrNotFound):
			respond.NotFound(w)
		default:
			slog.ErrorContext(r.Context(), "githttp authorization failed", "action", action, "repo", requested, "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return nil, false
	}
	fullName := rep.FullName
	applog.AddAttrs(r.Context(), slog.String("repo", fullName))

	if err := h.ensureRepo(r.Context(), fullName, rep.DefaultBranch); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			respond.NotFound(w)
			return nil, false
		}
		slog.ErrorContext(r.Context(), "githttp ensure repo failed", "action", action, "repo", requested, "resolved_repo", fullName, "error", err)
		http.Error(w, "Internal Server Error", 500)
		return nil, false
	}

	repoPath, err := h.store.GetRepoPath(r.Context(), fullName)
	if err != nil {
		slog.ErrorContext(r.Context(), "githttp repo path lookup failed", "action", action, "repo", fullName, "error", err)
		http.Error(w, "Internal Server Error", 500)
		return nil, false
	}

	projectRoot, err := h.store.RepoRoot(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "githttp repo root lookup failed", "action", action, "repo", fullName, "error", err)
		http.Error(w, "Internal Server Error", 500)
		return nil, false
	}

	return &repoContext{
		projectRoot:     projectRoot,
		repoPath:        repoPath,
		repoFullName:    fullName,
		gitRepoFullName: fullName,
		repositoryID:    rep.ID,
		defaultBranch:   rep.DefaultBranch,
	}, true
}

func (h *Handler) revalidateDelegatedGit(ctx context.Context, repositoryID uint, gitService string) error {
	return h.Svc.RevalidateDelegatedGitTransport(ctx, repositoryID, gitService)
}

func (h *Handler) logDelegatedGitOperation(ctx context.Context, operation, outcome, reason string) {
	if _, ok := service.DelegatedSessionFromContext(ctx); !ok {
		return
	}
	operation = delegatedGitAuditOperation(operation)
	if err := h.Svc.LogCurrentDelegatedSessionAudit(ctx, service.DelegatedSessionAuditEvent{
		Action: service.AuditActionDelegatedGitWrite, Operation: operation, Outcome: outcome, Reason: reason,
	}); err != nil {
		slog.WarnContext(ctx, "delegated Git operation audit failed", "operation", operation, "outcome", outcome, "reason", reason, "error", err)
	}
}

func delegatedGitAuditOperation(gitService string) string {
	switch gitService {
	case "git-receive-pack", "git.receive_pack":
		return "git.receive_pack"
	case "git-upload-pack", "git.upload_pack":
		return "git.upload_pack"
	default:
		return strings.TrimSpace(gitService)
	}
}

func (h *Handler) logDelegatedGitWrite(ctx context.Context, outcome, reason string) {
	h.logDelegatedGitOperation(ctx, "git.receive_pack", outcome, reason)
}

func pathParam(r *http.Request, key string) string {
	raw := chi.URLParam(r, key)
	if r.URL.RawPath != "" {
		decoded, err := url.PathUnescape(raw)
		if err == nil {
			return decoded
		}
	}
	return raw
}

// InfoRefs handles GET /{owner}/{repo}.git/info/refs?service=git-*
func (h *Handler) InfoRefs(w http.ResponseWriter, r *http.Request) {
	svc := r.URL.Query().Get("service")
	required := service.RepoPermissionRead
	if svc == "git-receive-pack" {
		required = service.RepoPermissionWrite
	}
	repoCtx, ok := h.resolveRepoContext(w, r, "info/refs", svc, required)
	if !ok {
		return
	}
	if _, delegated := service.DelegatedSessionIDFromContext(r.Context()); delegated {
		if err := h.revalidateDelegatedGit(r.Context(), repoCtx.repositoryID, svc); err != nil {
			respond.Error(w, http.StatusForbidden, "Delegated session authority changed before Git operation")
			return
		}
	}
	if err := h.serve(w, r, serveRequest{
		projectRoot:  repoCtx.projectRoot,
		repoPath:     repoCtx.repoPath,
		svcName:      svc,
		advertise:    true,
		repoFullName: repoCtx.gitRepoFullName,
	}); err != nil {
		slog.ErrorContext(r.Context(), "githttp info refs failed", "repo", repoCtx.repoFullName, "error", err)
		http.Error(w, "Internal Server Error", 500)
	}
}

// UploadPack handles POST /{owner}/{repo}.git/git-upload-pack (clone/fetch)
func (h *Handler) UploadPack(w http.ResponseWriter, r *http.Request) {
	repoCtx, ok := h.resolveRepoContext(w, r, "upload-pack", "git-upload-pack", service.RepoPermissionRead)
	if !ok {
		return
	}
	if _, delegated := service.DelegatedSessionIDFromContext(r.Context()); delegated {
		if err := h.revalidateDelegatedGit(r.Context(), repoCtx.repositoryID, "git-upload-pack"); err != nil {
			respond.Error(w, http.StatusForbidden, "Delegated session authority changed before Git operation")
			return
		}
	}
	if err := h.serve(w, r, serveRequest{
		projectRoot:  repoCtx.projectRoot,
		repoPath:     repoCtx.repoPath,
		svcName:      "git-upload-pack",
		advertise:    false,
		repoFullName: repoCtx.gitRepoFullName,
	}); err != nil {
		slog.ErrorContext(r.Context(), "githttp upload-pack failed", "repo", repoCtx.repoFullName, "error", err)
		http.Error(w, "Internal Server Error", 500)
	}
}

// ReceivePack handles POST /{owner}/{repo}.git/git-receive-pack (push)
func (h *Handler) ReceivePack(w http.ResponseWriter, r *http.Request) {
	repoCtx, ok := h.resolveRepoContext(w, r, "receive-pack", "git-receive-pack", service.RepoPermissionWrite)
	if !ok {
		return
	}
	if rejectOversizedReceivePack(w, r) {
		h.logDelegatedGitWrite(r.Context(), "denied", "body_too_large")
		return
	}
	var configuredProtectedBranches []string
	if !repoCtx.isWikiBacking {
		var err error
		configuredProtectedBranches, err = h.Svc.ProtectedBranchNames(r.Context(), repoCtx.repositoryID)
		if err != nil {
			slog.ErrorContext(r.Context(), "githttp protected branch lookup failed", "repo", repoCtx.repoFullName, "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}
	delegated := false
	var delegatedProtectedBranches []string
	if _, ok := service.DelegatedSessionFromContext(r.Context()); ok {
		delegated = true
		delegatedProtectedBranches = append(delegatedProtectedBranches, repoCtx.defaultBranch)
		delegatedProtectedBranches = append(delegatedProtectedBranches, configuredProtectedBranches...)
	}
	var beforeRefs map[string]string
	parentRepo := repoCtx.repoFullName
	isWikiRepo := repoCtx.isWikiBacking
	serveWriter := w
	var buffered *bufferedResponseWriter
	if isWikiRepo {
		buffered = newBufferedResponseWriter()
		serveWriter = buffered
	}
	var delegatedDenial error
	runReceivePack := func() error {
		// Repository/catalog locks precede the capture barrier. Taking the
		// mutation lease before WithRepoLock could deadlock a queued capture.
		r := r
		if h.Svc.Git != nil {
			ctx, release, err := h.Svc.Git.BeginMutation(r.Context())
			if err != nil {
				return err
			}
			defer release()
			r = r.WithContext(ctx)
		}
		var repairOwnerToken string
		clearRepairOwner := func(reason string) error {
			if !isWikiRepo || repairOwnerToken == "" {
				return nil
			}
			cleanupCtx, cancel := detachedGitHTTPContext(r.Context(), 30*time.Second)
			defer cancel()
			if err := h.Svc.ClearWikiReceivePackRepairObligationLocked(cleanupCtx, parentRepo, repairOwnerToken); err != nil {
				return fmt.Errorf("clear wiki receive-pack repair obligation after %s: %w", reason, err)
			}
			return nil
		}
		if isWikiRepo {
			if err := h.Svc.ReconcileWikiBeforeReceivePackLocked(r.Context(), parentRepo); err != nil {
				return err
			}
		}
		var beforeRefsErr error
		beforeRefs, beforeRefsErr = snapshotRefs(r.Context(), repoCtx.repoPath)
		if beforeRefsErr != nil {
			slog.WarnContext(r.Context(), "githttp snapshot refs before push failed", "repo", repoCtx.repoFullName, "error", beforeRefsErr)
		}
		if isWikiRepo {
			token, err := h.Svc.BeginWikiReceivePackRepairObligationLocked(r.Context(), parentRepo)
			if err != nil {
				return err
			}
			repairOwnerToken = token
			stopRefresh := h.startWikiReceivePackRepairOwnerRefresh(r.Context(), parentRepo, repairOwnerToken)
			defer stopRefresh()
		}
		if delegated {
			if err := h.revalidateDelegatedGit(r.Context(), repoCtx.repositoryID, "git-receive-pack"); err != nil {
				delegatedDenial = err
				h.logDelegatedGitWrite(r.Context(), "denied", service.DelegatedSessionDenialReason(err))
				return err
			}
		}
		if err := h.serve(serveWriter, r, serveRequest{
			projectRoot:                repoCtx.projectRoot,
			repoPath:                   repoCtx.repoPath,
			svcName:                    "git-receive-pack",
			advertise:                  false,
			repoFullName:               repoCtx.gitRepoFullName,
			allowDeleteCurrent:         isWikiRepo,
			gitHTTPProtectedBranches:   configuredProtectedBranches,
			delegatedSession:           delegated,
			delegatedProtectedBranches: delegatedProtectedBranches,
		}); err != nil {
			if clearErr := clearRepairOwner("pre-backend serve error"); clearErr != nil {
				return errors.Join(err, clearErr)
			}
			return err
		}
		// Compare under the repository lock so another push cannot be
		// attributed to this delegated request.
		if delegated {
			if beforeRefsErr != nil {
				h.logDelegatedGitWrite(r.Context(), "unknown", "ref_snapshot_failed")
			} else if afterRefs, err := snapshotRefs(r.Context(), repoCtx.repoPath); err != nil {
				h.logDelegatedGitWrite(r.Context(), "unknown", "ref_snapshot_failed")
			} else if len(diffPushedRefs(r.Context(), repoCtx.repoPath, beforeRefs, afterRefs)) > 0 {
				h.logDelegatedGitWrite(r.Context(), "success", "")
			} else {
				h.logDelegatedGitWrite(r.Context(), "denied", "no_ref_change")
			}
		}
		refsChanged := true
		if isWikiRepo {
			afterRefs, afterRefsErr := snapshotRefs(r.Context(), repoCtx.repoPath)
			if afterRefsErr != nil {
				slog.WarnContext(r.Context(), "githttp snapshot refs after wiki push failed", "repo", repoCtx.repoFullName, "error", afterRefsErr)
			} else {
				refsChanged = len(diffPushedRefs(r.Context(), repoCtx.repoPath, beforeRefs, afterRefs)) != 0
			}
		}
		if buffered != nil && buffered.statusCode() >= http.StatusBadRequest {
			if isWikiRepo && !refsChanged {
				return clearRepairOwner("failed receive-pack without ref changes")
			}
			return nil
		}
		if !isWikiRepo {
			return nil
		}
		if !refsChanged {
			return clearRepairOwner("receive-pack without ref changes")
		}
		_, err := h.Svc.IngestWikiGitAfterReceivePackLocked(r.Context(), parentRepo, service.WikiGitIngestOptions{
			ReceivePackRepairOwnerToken: repairOwnerToken,
		})
		return err
	}
	var err error
	if isWikiRepo {
		err = h.Svc.WithWikiCatalogWriteLockForReceivePack(r.Context(), parentRepo, func() error {
			return h.store.WithRepoLock(r.Context(), repoCtx.gitRepoFullName, runReceivePack)
		})
	} else {
		err = h.store.WithRepoLock(r.Context(), repoCtx.gitRepoFullName, runReceivePack)
	}
	if err != nil {
		if delegatedDenial != nil {
			respond.Error(w, http.StatusForbidden, "Delegated session authority changed before Git operation")
			return
		}
		h.logDelegatedGitWrite(r.Context(), "denied", "backend_error")
		slog.ErrorContext(r.Context(), "githttp receive-pack failed", "repo", repoCtx.repoFullName, "error", err)
		http.Error(w, "Internal Server Error", 500)
		return
	}
	// Run follow-up work after the repository lock is released but before the
	// request completes so push-side effects remain synchronous.
	followupCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	followupCtx = applog.CloneContext(followupCtx, r.Context())
	if scopedDB, ok := service.DBFromContext(r.Context()); ok {
		followupCtx = service.ContextWithDB(followupCtx, scopedDB)
	}
	if u, ok := service.UserFromContext(r.Context()); ok {
		followupCtx = service.ContextWithUser(followupCtx, u)
	}
	if session, ok := service.DelegatedSessionFromContext(r.Context()); ok {
		followupCtx = service.ContextWithDelegatedSession(followupCtx, session)
	}
	applog.AddAttrs(followupCtx, slog.String("repo", repoCtx.repoFullName))
	if err := h.store.WithRepoLock(followupCtx, repoCtx.gitRepoFullName, func() error {
		if h.Svc.Git != nil {
			_, release, err := h.Svc.Git.BeginMutation(followupCtx)
			if err != nil {
				return err
			}
			defer release()
		}
		fixHEAD(repoCtx.repoPath)
		return nil
	}); err != nil {
		slog.WarnContext(followupCtx, "post-push HEAD repair unavailable", "error", err)
	}
	if !repoCtx.isWikiBacking {
		if err := h.handlePostPushWebhooks(followupCtx, repoCtx.repoFullName, repoCtx.repoPath, beforeRefs); err != nil {
			slog.ErrorContext(followupCtx, "post-push webhook delivery failed", "error", err)
		}
		if err := h.Svc.SyncWorkflowsFromRepo(followupCtx, repoCtx.repoFullName); err != nil {
			slog.ErrorContext(followupCtx, "post-push workflow sync failed", "error", err)
		}
	}
	if buffered != nil {
		if err := buffered.writeTo(w); err != nil {
			slog.WarnContext(r.Context(), "githttp write buffered receive-pack response failed", "repo", repoCtx.repoFullName, "error", err)
		}
	}
}

func (h *Handler) startWikiReceivePackRepairOwnerRefresh(parent context.Context, repoFullName, ownerToken string) func() {
	if h == nil || h.Svc == nil || strings.TrimSpace(ownerToken) == "" {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(wikiReceivePackRepairOwnerRefreshInterval)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				refreshCtx, cancel := detachedGitHTTPContext(parent, 30*time.Second)
				if err := h.Svc.RefreshWikiReceivePackRepairObligationOwner(refreshCtx, repoFullName, ownerToken); err != nil {
					slog.ErrorContext(refreshCtx, "githttp wiki receive-pack owner refresh failed", "repo", repoFullName, "error", err)
				}
				cancel()
				timer.Reset(wikiReceivePackRepairOwnerRefreshInterval)
			case <-stop:
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// wikiRepoParentName returns the parent repository name for bare wiki
// repositories that follow GitHub's "{repo}.wiki.git" convention.
func wikiRepoParentName(full string) (string, bool) {
	const suffix = ".wiki"
	if !strings.HasSuffix(full, suffix) {
		return "", false
	}
	return strings.TrimSuffix(full, suffix), true
}

func rejectOversizedReceivePack(w http.ResponseWriter, r *http.Request) bool {
	limit := gitbackend.MaxPushBytes()
	if r.ContentLength > limit {
		slog.WarnContext(r.Context(), "githttp push body exceeded limit", "size", r.ContentLength, "limit", limit)
		http.Error(w, "push body exceeds maximum size", http.StatusRequestEntityTooLarge)
		return true
	}
	return false
}

// serve adapts primary-only branch policy to the shared, database-free backend.
// Only this primary write path may opt into receive-pack.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, req serveRequest) error {
	return gitbackend.Serve(w, r, gitbackend.Request{
		ProjectRoot:            req.projectRoot,
		Repository:             req.repoFullName,
		Service:                req.svcName,
		Advertise:              req.advertise,
		AllowReceive:           req.svcName == gitbackend.ReceivePack,
		AllowDeleteCurrent:     req.allowDeleteCurrent,
		ProtectedRefs:          protectedBranchRefs(req.gitHTTPProtectedBranches),
		Delegated:              req.delegatedSession,
		DelegatedProtectedRefs: protectedBranchRefs(req.delegatedProtectedBranches),
	})
}

func protectedBranchRefs(branches []string) []string {
	seen := make(map[string]struct{}, len(branches))
	refs := make([]string, 0, len(branches))
	for _, branch := range branches {
		ref := gitstore.RefsHeadsPrefix + strings.TrimSpace(branch)
		if !gitstore.IsValidRefName(ref) {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	return refs
}

func detachedGitHTTPContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	ctx = applog.CloneContext(ctx, parent)
	if scopedDB, ok := service.DBFromContext(parent); ok {
		ctx = service.ContextWithDB(ctx, scopedDB)
	}
	if u, ok := service.UserFromContext(parent); ok {
		ctx = service.ContextWithUser(ctx, u)
	}
	return ctx, cancel
}

// fixHEAD updates a bare repo's HEAD to point to the first available branch
// if the current HEAD target doesn't exist. This happens when a repo is created
// with default branch "main" but the client pushes to "master" (or vice versa).
func fixHEAD(repoPath string) {
	// Check if HEAD's target branch exists.
	out, err := exec.Command("git", "-C", repoPath, "symbolic-ref", "HEAD").Output()
	if err != nil {
		return
	}
	headRef := strings.TrimSpace(string(out))
	if _, err := exec.Command("git", "-C", repoPath, "rev-parse", "--verify", headRef).Output(); err == nil {
		return // HEAD is valid
	}
	// HEAD points to a non-existent branch — find the first real branch.
	branchOut, err := exec.Command("git", "-C", repoPath, "for-each-ref", "--format=%(refname)", gitstore.RefsHeadsPrefix, "--count=1").Output()
	if err != nil {
		return
	}
	branch := strings.TrimSpace(string(branchOut))
	if branch == "" {
		return
	}
	_ = exec.Command("git", "-C", repoPath, "symbolic-ref", "HEAD", branch).Run()
}
