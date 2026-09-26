package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	applog "github.com/ngaut/agent-git-service/internal/logging"
	"github.com/ngaut/agent-git-service/internal/mentions"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"

	"gorm.io/gorm"
)

// CountPRsByRepoID returns the count of open PRs for a repo by ID.
func (s *Service) CountPRsByRepoID(ctx context.Context, repoID uint) int {
	var count int64
	if err := s.DBForCtx(ctx).Model(&db.PullRequest{}).Where("repository_id = ? AND state = ? AND merged = false", repoID, db.StateOpen).Count(&count).Error; err != nil {
		slog.Error("CountPRsByRepoID", "error", err)
	}
	return int(count)
}

// PRRefProjectionDeniedError reports the narrow post-commit boundary where the
// AGS PR fact exists but fresh delegated authority denied the separate local ref
// projection. Callers must expose the committed PR number without treating the
// ref effect as successful.
type PRRefProjectionDeniedError struct {
	PRNumber int
	Created  bool
	Cause    error
}

func (e *PRRefProjectionDeniedError) Error() string {
	return fmt.Sprintf("PR #%d committed but ref projection denied by fresh delegated authority", e.PRNumber)
}

func (e *PRRefProjectionDeniedError) Unwrap() error { return e.Cause }

// CreatePRInput holds PR creation parameters.
type CreatePRInput struct {
	RepoFullName        string // target repo (base repo)
	HeadRepoFullName    string // source repo for head branch (fork for cross-repo PRs, defaults to RepoFullName)
	Title               string
	Body                string
	HeadRef             string
	BaseRef             string
	Draft               bool
	MaintainerCanModify bool
	AuthorLogin         string
}

// CreatePRWithResult creates one pull request. Assertion-keyed idempotency was
// retired with the workload assertion exchange; callers use normal PR facts and
// Access Grant invocation receipts instead.
func (s *Service) CreatePRWithResult(ctx context.Context, in CreatePRInput) (db.PullRequest, bool, error) {
	pr, err := s.CreatePR(ctx, in)
	if err != nil {
		// CreatePR can fail after the AGS PR fact commits but before a separate
		// repository projection. Preserve that fact and its created status for
		// callers while the non-nil error prevents a false success response.
		return pr, pr.ID != 0, err
	}
	return pr, true, nil
}

// CreatePR creates a pull request.
func (s *Service) CreatePR(ctx context.Context, in CreatePRInput) (db.PullRequest, error) {
	if err := ValidateDelegatedSessionOperationConstraints(ctx, "pr.create", map[string]string{"base_ref": in.BaseRef, "head_ref": in.HeadRef}); err != nil {
		return db.PullRequest{}, fmt.Errorf("service: create pr: operation constraints: %w", err)
	}
	rep, err := s.GetRepo(ctx, in.RepoFullName)
	if err != nil {
		return db.PullRequest{}, fmt.Errorf("service: create pr: repo: %w", err)
	}
	delegatedSession, delegated := DelegatedSessionFromContext(ctx)
	if delegated {
		fresh, err := s.RevalidateDelegatedSession(ctx, rep.ID, "pr.create", "pr:create", map[string]string{"base_ref": in.BaseRef, "head_ref": in.HeadRef})
		if err != nil {
			return db.PullRequest{}, fmt.Errorf("service: create pr: delegated session capability: %w", ErrForbidden)
		}
		delegatedSession = fresh.Session
	}
	authorizationUserID := uint(0)
	var author db.User
	if delegated && isAccessGrantTransportSession(delegatedSession) {
		author, err = s.accessGrantPRAuthor(ctx, delegatedSession)
		authorizationUserID = delegatedSession.PrincipalUserID
	} else {
		author, err = s.createPRAuthor(ctx, in.AuthorLogin)
		authorizationUserID = author.ID
	}
	if err != nil {
		return db.PullRequest{}, fmt.Errorf("service: create pr: author: %w", err)
	}
	canCreatePR, err := s.CanCreatePR(ctx, rep.ID, authorizationUserID)
	if err != nil {
		return db.PullRequest{}, fmt.Errorf("service: create pr: permission check: %w", err)
	}
	if !canCreatePR {
		return db.PullRequest{}, fmt.Errorf("%w: create pr requires repository owner or collaborator with write/admin permission", ErrForbidden)
	}
	baseRef := in.BaseRef
	if baseRef == "" {
		baseRef = rep.DefaultBranch
	}
	// Resolve head repo identity so the same-branch check uses canonical IDs.
	headRepoFullName := in.HeadRepoFullName
	if headRepoFullName == "" {
		headRepoFullName = rep.FullName
	}
	headRep := rep // fall back to base repo if head repo not found
	if headRepoFullName != rep.FullName {
		if resolvedHeadRep, err := s.GetRepo(ctx, headRepoFullName); err == nil {
			headRep = resolvedHeadRep
		}
	}
	headSHA, _ := s.Git.HeadSHA(ctx, headRepoFullName, in.HeadRef)
	baseSHA, _ := s.Git.HeadSHA(ctx, rep.FullName, baseRef)

	// Reject PRs where head and base branches are the same within the same repo.
	// Uses resolved repo IDs so a bogus HeadRepoFullName that falls back to
	// the base repo cannot bypass this check.
	// Cross-repo PRs (forks) may legitimately use the same branch name.
	if in.HeadRef == baseRef && headRep.ID == rep.ID {
		return db.PullRequest{}, fmt.Errorf("service: create pr: head and base must be different branches")
	}
	cleanBody, multicaLinkToken := extractMulticaPRLinkToken(in.Body)
	body := s.enrichPRBodyWithMulticaLink(rep.FullName, cleanBody, in.Title, in.HeadRef)
	var authoritativeMulticaClaims multicaprojection.PRLinkClaims
	multicaLinkAccepted := false
	var grantPRContext accessGrantPRContext
	grantLinkAccepted := false
	if delegated && isAccessGrantTransportSession(delegatedSession) {
		loaded, contextErr := s.accessGrantPRContextForSession(ctx, delegatedSession)
		if contextErr != nil {
			slog.WarnContext(ctx, "create PR without Access Grant context association", "repo", rep.FullName, "reason", "snapshot_unavailable")
		} else {
			grantPRContext = loaded
			body = s.enrichPRBodyWithAccessGrantContext(cleanBody, loaded)
			_, grantLinkAccepted = accessGrantSnapshotMulticaLink(0, rep.ID, loaded, s.accessGrantPRIssueURL(loaded))
		}
		if strings.TrimSpace(multicaLinkToken) != "" {
			slog.WarnContext(ctx, "ignore legacy Multica PR assertion for Access Grant transport Session", "repo", rep.FullName)
		}
	} else if strings.TrimSpace(multicaLinkToken) != "" {
		if s == nil || s.MulticaProjection == nil {
			slog.WarnContext(ctx, "create PR without Multica association", "repo", rep.FullName, "reason", "projection_not_configured")
		} else if claims, verifyErr := s.MulticaProjection.VerifyPRLinkToken(multicaLinkToken); verifyErr != nil {
			slog.WarnContext(ctx, "create PR without Multica association", "repo", rep.FullName, "reason", "link_token_invalid")
		} else {
			authoritativeMulticaClaims = claims
			multicaLinkAccepted = true
			body = enrichBodyWithAuthoritativeMulticaLink(cleanBody, claims)
		}
	}

	var agentSessionID *string
	if delegated {
		id := delegatedSession.ID
		agentSessionID = &id
	}

	if delegated {
		fresh, err := s.RevalidateDelegatedSession(ctx, rep.ID, "pr.create", "pr:create", map[string]string{"base_ref": baseRef, "head_ref": in.HeadRef})
		if err != nil {
			return db.PullRequest{}, fmt.Errorf("service: create pr: final delegated authority: %w", ErrForbidden)
		}
		delegatedSession = fresh.Session
		agentSessionID = &fresh.Session.ID
	}

	const maxRetries = 25
	for attempt := 0; attempt < maxRetries; attempt++ {
		var pr db.PullRequest
		attemptSession := delegatedSession
		if err := s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
			txCtx := ContextWithDB(ctx, tx)
			attemptSessionID := agentSessionID
			if delegated {
				fresh, err := s.RevalidateDelegatedSession(txCtx, rep.ID, "pr.create", "pr:create", map[string]string{"base_ref": baseRef, "head_ref": in.HeadRef})
				if err != nil {
					return fmt.Errorf("delegated authority before PR transaction write: %w", ErrForbidden)
				}
				attemptSession = fresh.Session
				attemptSessionID = &attemptSession.ID
			}
			if err := lockRepoForNumbering(tx, rep.ID); err != nil {
				return err
			}
			num, err := nextIssueOrPRNumberTx(tx, rep.ID)
			if err != nil {
				return err
			}
			pr = db.PullRequest{
				Number:              num,
				RepositoryID:        rep.ID,
				HeadRepositoryID:    headRep.ID,
				Title:               in.Title,
				Body:                db.LargeText(body),
				HeadRef:             in.HeadRef,
				HeadSHA:             headSHA,
				BaseRef:             baseRef,
				BaseSHA:             baseSHA,
				Draft:               in.Draft,
				MaintainerCanModify: in.MaintainerCanModify,
				State:               db.StateOpen,
				AuthorID:            author.ID,
				AgentSessionID:      attemptSessionID,
			}
			if err := tx.Create(&pr).Error; err != nil {
				return err
			}
			if err := s.syncPullRequestBodyReferences(txCtx, pr); err != nil {
				return fmt.Errorf("sync pull request references: %w", err)
			}
			if multicaLinkAccepted {
				linkClaims := authoritativeMulticaClaims
				issueURL := strings.TrimSpace(linkClaims.IssueURL)
				if issueURL == "" && strings.TrimSpace(linkClaims.Workspace) != "" {
					issueURL = multicaprojection.IssueURL(s.MulticaProjection.AppURL(), linkClaims.Workspace, linkClaims.IssueKey)
				}
				link := authoritativeMulticaLink(pr.ID, rep.ID, linkClaims, issueURL)
				if err := tx.Create(&link).Error; err != nil {
					return fmt.Errorf("bind Multica issue link: %w", err)
				}
			}
			if grantLinkAccepted {
				link, ok := accessGrantSnapshotMulticaLink(pr.ID, rep.ID, grantPRContext, s.accessGrantPRIssueURL(grantPRContext))
				if ok {
					if err := tx.Create(&link).Error; err != nil {
						return fmt.Errorf("bind Access Grant context link: %w", err)
					}
				}
			}
			if delegated {
				if _, err := s.RevalidateDelegatedSession(txCtx, rep.ID, "pr.create", "pr:create", map[string]string{"base_ref": baseRef, "head_ref": in.HeadRef}); err != nil {
					return fmt.Errorf("delegated authority at PR commit boundary: %w", ErrForbidden)
				}
				if err := s.LogCurrentDelegatedSessionAudit(txCtx, DelegatedSessionAuditEvent{
					Action: AuditActionDelegatedPRCreate, Operation: "pull_request.create", Outcome: "success", PRNumber: pr.Number,
				}); err != nil {
					return fmt.Errorf("audit delegated PR create: %w", err)
				}
			}
			return nil
		}); err != nil {
			if isDuplicateErr(err) {
				if s.testCreatePRRetry != nil {
					s.testCreatePRRetry(attempt)
				}
				time.Sleep(retryDelay(attempt))
				continue
			}
			return db.PullRequest{}, fmt.Errorf("service: create pr: %w", err)
		}
		if delegated {
			delegatedSession = attemptSession
		}

		// The PR row is an AGS-owned durable fact; the refs/pull write is a
		// separate repository effect. Revalidate immediately before that effect
		// and return the committed fact with an error if authority drifted.
		if s.testCreatePRRefBeforeWrite != nil {
			s.testCreatePRRefBeforeWrite()
		}
		if delegated {
			fresh, err := s.RevalidateDelegatedSession(ctx, rep.ID, "pr.create", "pr:create", map[string]string{"base_ref": baseRef, "head_ref": in.HeadRef})
			if err != nil {
				hydrateCreatedPR(&pr, rep, headRep, author)
				sessionSnapshot := delegatedSession
				if sessionSnapshot.PrincipalUser.ID == 0 && sessionSnapshot.PrincipalUserID == author.ID {
					sessionSnapshot.PrincipalUser = author
				}
				pr.AgentSession = &sessionSnapshot
				return pr, &PRRefProjectionDeniedError{PRNumber: pr.Number, Created: true, Cause: ErrForbidden}
			}
			delegatedSession = fresh.Session
		}

		// Create the refs/pull/ID/head ref in the git repository
		if err := s.Git.CreatePRRef(ctx, rep.FullName, headRepoFullName, headSHA, pr.Number); err != nil {
			// Log the error but don't fail the PR creation
			slog.WarnContext(ctx, "create PR ref failed", "repo", rep.FullName, "pr_number", pr.Number, "error", err)
		}

		hydrateCreatedPR(&pr, rep, headRep, author)
		if delegated {
			sessionSnapshot := delegatedSession
			if sessionSnapshot.PrincipalUser.ID == 0 && sessionSnapshot.PrincipalUserID == author.ID {
				sessionSnapshot.PrincipalUser = author
			}
			pr.AgentSession = &sessionSnapshot
		}
		// Generate and store embedding for semantic search (fire-and-forget).
		s.EmbedPR(ctx, pr.ID, pr.Title, string(pr.Body))
		// Update commit messages and filenames for search.
		if s.Ctx == nil {
			// In tests (no server ctx), run synchronously to avoid stray goroutines.
			if err := s.updatePRCommitDataFromLoaded(ctx, rep.FullName, headRepoFullName, pr); err != nil {
				slog.WarnContext(ctx, "update PR commit data failed", "repo", rep.FullName, "pr_number", pr.Number, "error", err)
			}
		} else {
			// Fire-and-forget in production; use the server context so work can outlive the request.
			bgCtx := s.ServerCtx()
			bgCtx = applog.CloneContext(bgCtx, ctx)
			if scopedDB, ok := DBFromContext(ctx); ok {
				bgCtx = ContextWithDB(bgCtx, scopedDB)
			}
			applog.AddAttrs(bgCtx,
				slog.String("repo", rep.FullName),
				slog.Int("pr_number", pr.Number),
			)
			s.Wg.Add(1)
			go func(pr db.PullRequest) {
				defer s.Wg.Done()
				if err := s.updatePRCommitDataFromLoaded(bgCtx, rep.FullName, headRepoFullName, pr); err != nil {
					slog.WarnContext(bgCtx, "update PR commit data failed", "error", err)
				}
			}(pr)
		}
		s.ObserveClientRunPR(ctx, pr)
		return pr, nil
	}
	return db.PullRequest{}, fmt.Errorf("service: create pr: failed after %d retries", maxRetries)
}

func (s *Service) createPRAuthor(ctx context.Context, login string) (db.User, error) {
	if viewer, ok := UserFromContext(ctx); ok && viewer.ID != 0 && strings.EqualFold(viewer.Login, login) {
		return viewer, nil
	}
	return s.GetUser(ctx, login)
}

func hydrateCreatedPR(pr *db.PullRequest, rep, headRep db.Repository, author db.User) {
	if pr == nil {
		return
	}
	pr.Author = author
	pr.Repository = rep
	pr.HeadRepository = headRep
}

// CanCreatePR reports whether a user can create pull requests in the target repository.
// Allowed: repository owner, or collaborators with write/admin permission.
func (s *Service) CanCreatePR(ctx context.Context, repoID, userID uint) (bool, error) {
	if viewer, ok := UserFromContext(ctx); ok && viewer.ID == userID {
		if perm, ok := s.CachedRepoPermission(ctx, repoID); ok {
			return perm.AtLeast(RepoPermissionWrite), nil
		}
	}
	perm, err := s.HasRepoAccess(ctx, repoID, userID)
	if err != nil {
		return false, err
	}
	return perm.AtLeast(RepoPermissionWrite), nil
}

// GetPR fetches a single pull request.
func (s *Service) GetPR(ctx context.Context, repoFullName string, number int) (db.PullRequest, error) {
	rep, err := s.GetRepo(ctx, repoFullName)
	if err != nil {
		return db.PullRequest{}, err
	}
	var pr db.PullRequest
	if err := preloadPRFull(s.DBForCtx(ctx)).
		First(&pr, "repository_id = ? AND number = ?", rep.ID, number).Error; err != nil {
		return pr, wrapErrf(err, "pull request #%d", number)
	}
	return pr, nil
}

// PRListFilter groups filters for GitHub-compatible pull request listing.
type PRListFilter struct {
	RepoFullName string
	State        string
	Head         string
	Base         string
	Sort         string
	Direction    string
	Mentioned    string
}

// ListPRs returns pull requests filtered by state.
func (s *Service) ListPRs(ctx context.Context, repoFullName, state string) ([]db.PullRequest, error) {
	return s.ListPRsFiltered(ctx, PRListFilter{RepoFullName: repoFullName, State: state})
}

// ListPRsFiltered returns pull requests filtered by GitHub's list-pulls query params.
func (s *Service) ListPRsFiltered(ctx context.Context, filter PRListFilter) ([]db.PullRequest, error) {
	state := filter.State
	if state == "" {
		state = db.StateOpen
	}
	rep, err := s.GetRepo(ctx, filter.RepoFullName)
	if err != nil {
		return nil, err
	}
	q := preloadPRFull(s.DBForCtx(ctx)).Where("repository_id = ?", rep.ID)
	switch state {
	case db.StateClosed:
		q = q.Where("state = 'closed' OR merged = true")
	case "all":
		// no filter
	default: // open
		q = q.Where("state = 'open' AND merged = false")
	}
	if base := strings.TrimSpace(filter.Base); base != "" {
		q = q.Where("base_ref = ?", base)
	}
	if head := strings.TrimSpace(filter.Head); head != "" {
		if owner, ref, ok := strings.Cut(head, ":"); ok {
			q = q.Joins("JOIN repositories AS head_repos ON head_repos.id = pull_requests.head_repository_id").
				Where("LOWER(head_repos.full_name) LIKE ?", strings.ToLower(owner)+"/%").
				Where("head_ref = ?", ref)
		} else {
			q = q.Where("head_ref = ?", head)
		}
	}
	q = applyPRMentionedFilter(q, filter.Mentioned)
	orderColumn, orderDirection := prListOrder(filter.Sort, filter.Direction)
	orderExpr := orderColumn + " " + orderDirection + ", pull_requests.number desc"
	if filter.Mentioned == "" {
		var prs []db.PullRequest
		if err := q.Order(orderExpr).Limit(defaultListLimit).Find(&prs).Error; err != nil {
			return nil, err
		}
		return prs, nil
	}
	batchSize := defaultListLimit
	if batchSize < 25 {
		batchSize = 25
	}
	var (
		offset  int
		matches []db.PullRequest
	)
	for len(matches) < defaultListLimit {
		var batch []db.PullRequest
		if err := q.Order(orderExpr).Limit(batchSize).Offset(offset).Find(&batch).Error; err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, pr := range batch {
			ok, err := s.prMatchesMention(ctx, pr, filter.Mentioned)
			if err != nil {
				return nil, err
			}
			if ok {
				matches = append(matches, pr)
				if len(matches) >= defaultListLimit {
					break
				}
			}
		}
		if len(batch) < batchSize {
			break
		}
		offset += len(batch)
	}
	return matches, nil
}

func applyPRMentionedFilter(q *gorm.DB, mention string) *gorm.DB {
	pattern := mentionCandidatePattern(mention)
	if pattern == "" {
		return q
	}
	issueCommentExists := "EXISTS (" +
		"SELECT 1 FROM issue_comments icc " +
		"WHERE icc.repository_id = pull_requests.repository_id " +
		"AND icc.issue_number = pull_requests.number " +
		"AND LOWER(icc.body) LIKE ?)"
	reviewCommentExists := "EXISTS (" +
		"SELECT 1 FROM pr_review_comments prc " +
		"WHERE prc.pull_request_id = pull_requests.id " +
		"AND LOWER(prc.body) LIKE ?)"
	reviewSummaryExists := "EXISTS (" +
		"SELECT 1 FROM pull_request_reviews prr " +
		"WHERE prr.pull_request_id = pull_requests.id " +
		"AND LOWER(prr.body) LIKE ?)"
	return q.Where(
		"(LOWER(pull_requests.title) LIKE ? OR LOWER(pull_requests.body) LIKE ? OR "+issueCommentExists+" OR "+reviewCommentExists+" OR "+reviewSummaryExists+")",
		pattern, pattern, pattern, pattern, pattern,
	)
}

func mentionCandidatePattern(mention string) string {
	mention = strings.TrimSpace(mention)
	if mention == "" {
		return ""
	}
	if !strings.HasPrefix(mention, "@") {
		mention = "@" + mention
	}
	return "%" + escapeLike(strings.ToLower(mention)) + "%"
}

func (s *Service) prMatchesMention(ctx context.Context, pr db.PullRequest, mention string) (bool, error) {
	if mentions.ContainsLogin(pr.Title, mention) || mentions.ContainsLogin(string(pr.Body), mention) {
		return true, nil
	}
	var issueCommentBodies []string
	if err := s.DBForCtx(ctx).WithContext(ctx).
		Table("issue_comments").
		Where("repository_id = ? AND issue_number = ?", pr.RepositoryID, pr.Number).
		Pluck("body", &issueCommentBodies).Error; err != nil {
		return false, err
	}
	for _, body := range issueCommentBodies {
		if mentions.ContainsLogin(body, mention) {
			return true, nil
		}
	}
	var reviewCommentBodies []string
	if err := s.DBForCtx(ctx).WithContext(ctx).
		Table("pr_review_comments").
		Where("pull_request_id = ?", pr.ID).
		Pluck("body", &reviewCommentBodies).Error; err != nil {
		return false, err
	}
	for _, body := range reviewCommentBodies {
		if mentions.ContainsLogin(body, mention) {
			return true, nil
		}
	}
	var reviewBodies []string
	if err := s.DBForCtx(ctx).WithContext(ctx).
		Table("pull_request_reviews").
		Where("pull_request_id = ?", pr.ID).
		Pluck("body", &reviewBodies).Error; err != nil {
		return false, err
	}
	for _, body := range reviewBodies {
		if mentions.ContainsLogin(body, mention) {
			return true, nil
		}
	}
	return false, nil
}

func prListOrder(sortParam, directionParam string) (string, string) {
	sortParam = strings.ToLower(strings.TrimSpace(sortParam))
	directionParam = strings.ToLower(strings.TrimSpace(directionParam))
	column := "pull_requests.created_at"
	switch sortParam {
	case "updated":
		column = "pull_requests.updated_at"
	case "created", "popularity", "long-running", "":
		column = "pull_requests.created_at"
	}
	direction := "desc"
	if sortParam == "long-running" && directionParam == "" {
		direction = "asc"
	}
	if directionParam == "asc" || directionParam == "desc" {
		direction = directionParam
	}
	return column, direction
}

// UpdatePRInput for partial PR update.
type UpdatePRInput struct {
	Title   *string
	Body    *string
	State   *string
	BaseRef *string
	Draft   *bool
}

// UpdatePR applies a partial update to a PR.
func (s *Service) UpdatePR(ctx context.Context, repoFullName string, number int, in UpdatePRInput) (db.PullRequest, error) {
	rep, err := s.GetRepo(ctx, repoFullName)
	if err != nil {
		return db.PullRequest{}, err
	}
	var pr db.PullRequest
	if err := s.DBForCtx(ctx).First(&pr, "repository_id = ? AND number = ?", rep.ID, number).Error; err != nil {
		return pr, wrapErrf(err, "pull request #%d", number)
	}

	origTitle := pr.Title
	origBody := pr.Body
	origState := pr.State
	if in.Title != nil {
		pr.Title = *in.Title
	}
	if in.Body != nil {
		title := pr.Title
		if in.Title != nil {
			title = *in.Title
		}
		pr.Body = db.LargeText(s.enrichPRBodyWithMulticaLink(rep.FullName, *in.Body, title, pr.HeadRef))
	}
	if in.State != nil {
		pr.State = *in.State
		if *in.State == db.StateClosed {
			if pr.ClosedAt == nil || pr.ClosedAt.IsZero() {
				now := time.Now().UTC()
				pr.ClosedAt = &now
			}
		} else {
			pr.ClosedAt = nil
		}
	}
	if in.BaseRef != nil {
		pr.BaseRef = *in.BaseRef
	}
	if in.Draft != nil {
		pr.Draft = *in.Draft
	}
	if err := s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&pr).Error; err != nil {
			return err
		}
		if in.State != nil && *in.State == db.StateClosed && !pr.Merged {
			now := time.Now().UTC()
			if err := s.enqueueClosedMulticaExternalPRTerminalDeliveryTx(ctx, tx, pr, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return pr, err
	}
	if in.Body != nil && pr.Body != origBody {
		if err := s.syncPullRequestBodyReferences(ctx, pr); err != nil {
			return pr, err
		}
	}
	// Re-embed if title or body actually changed.
	if pr.Title != origTitle || pr.Body != origBody {
		s.EmbedPR(ctx, pr.ID, pr.Title, string(pr.Body))
	}
	if err := preloadPRFull(s.DBForCtx(ctx)).First(&pr, pr.ID).Error; err != nil {
		return pr, wrapErr(err)
	}
	if in.State != nil && *in.State == db.StateClosed && origState != db.StateClosed && !pr.Merged {
		if err := s.DispatchPullRequestClosedIntegrations(ctx, pr); err != nil {
			slog.WarnContext(ctx, "close PR projections failed", "repo", repoFullName, "pr_number", number, "error", err)
		}
	} else if pr.State == db.StateOpen && (pr.Title != origTitle || pr.Body != origBody || in.BaseRef != nil || (in.State != nil && *in.State == db.StateOpen && origState == db.StateClosed)) {
		if _, err := s.DispatchPullRequestIntegrations(ctx, pr); err != nil {
			slog.WarnContext(ctx, "refresh PR projections failed", "repo", repoFullName, "pr_number", number, "error", err)
		}
	}
	return pr, nil
}

func (s *Service) enrichPRBodyWithMulticaLink(repoFullName, body string, values ...string) string {
	if s == nil || s.MulticaProjection == nil {
		return body
	}
	return s.MulticaProjection.EnrichPullRequestBodyForRepo(repoFullName, body, values...)
}

// UpdatePRByID updates a PR's state and/or draft flag using its DB ID.
func (s *Service) UpdatePRByID(ctx context.Context, id uint, state *string, draft *bool) error {
	orig, err := s.GetPRByID(ctx, id)
	if err != nil {
		return err
	}
	updates := make(map[string]any)
	if state != nil {
		updates["state"] = *state
		if *state == db.StateClosed {
			if orig.ClosedAt == nil || orig.ClosedAt.IsZero() {
				now := time.Now().UTC()
				updates["closed_at"] = &now
			}
		} else if *state == db.StateOpen {
			updates["closed_at"] = nil
		}
	}
	if draft != nil {
		updates["draft"] = *draft
	}
	if len(updates) > 0 {
		if err := s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&db.PullRequest{}).Where("id = ?", id).Updates(updates).Error; err != nil {
				return err
			}
			if state != nil && *state == db.StateClosed {
				var closedPR db.PullRequest
				if err := tx.First(&closedPR, id).Error; err != nil {
					return err
				}
				if !closedPR.Merged {
					return s.enqueueClosedMulticaExternalPRTerminalDeliveryTx(ctx, tx, closedPR, time.Now().UTC())
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if state != nil && *state == db.StateClosed && orig.State != db.StateClosed {
			pr, err := s.GetPRByID(ctx, id)
			if err != nil {
				return err
			}
			if !pr.Merged {
				if err := s.DispatchPullRequestClosedIntegrations(ctx, pr); err != nil {
					slog.WarnContext(ctx, "close PR projections failed", "pull_request_id", id, "error", err)
				}
			}
		}
	}
	return nil
}

// UpdatePRFields updates arbitrary fields on a pull request by its DB ID.
func (s *Service) UpdatePRFields(ctx context.Context, id uint, updates map[string]any) error {
	if len(updates) > 0 {
		pr, err := s.GetPRByID(ctx, id)
		if err != nil {
			return err
		}
		origAssigneeLogins := pr.AssigneeLogins
		newAssigneeLogins := origAssigneeLogins
		if v, ok := updates["assignee_logins"].(string); ok {
			newAssigneeLogins = v
		}
		newBody, bodyChanged := issueReferenceBodyUpdate(updates["body"], pr.Body)
		if err := s.DBForCtx(ctx).Model(&db.PullRequest{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return err
		}
		if bodyChanged {
			pr.Body = db.LargeText(newBody)
			pr.UpdatedAt = time.Now()
			if err := s.syncPullRequestBodyReferences(ctx, pr); err != nil {
				return err
			}
		}
		if newAssigneeLogins != origAssigneeLogins {
			origSet := loginSet(origAssigneeLogins)
			var added []string
			for _, login := range splitLogins(newAssigneeLogins) {
				if !origSet[login] {
					added = append(added, login)
				}
			}
			if len(added) > 0 {
				if err := s.createAssignmentNotificationsForLogins(ctx, s.notificationActorIDForContext(ctx), NotificationSubjectPullRequest, id, pr.RepositoryID, added); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return nil
}

// GetPRByID loads a pull request by its internal DB ID with standard associations.
func (s *Service) GetPRByID(ctx context.Context, id uint) (db.PullRequest, error) {
	var pr db.PullRequest
	err := preloadPRFull(s.DBForCtx(ctx)).First(&pr, id).Error
	if err != nil {
		return pr, wrapErr(err)
	}
	return pr, nil
}

// AddPRAssignees adds assignees (by login) to a PR. Returns the updated PR.
func (s *Service) AddPRAssignees(ctx context.Context, repoFullName string, number int, logins []string) (db.PullRequest, error) {
	rep, err := s.GetRepo(ctx, repoFullName)
	if err != nil {
		return db.PullRequest{}, err
	}
	var pr db.PullRequest
	if err := s.DBForCtx(ctx).First(&pr, "repository_id = ? AND number = ?", rep.ID, number).Error; err != nil {
		return db.PullRequest{}, wrapErrf(err, "pull request #%d", number)
	}
	existingSet := loginSet(pr.AssigneeLogins)
	var added []string
	for _, login := range logins {
		if !existingSet[login] {
			added = append(added, login)
		}
	}
	merged := mergeLogins(pr.AssigneeLogins, logins)
	if err := s.DBForCtx(ctx).Model(&pr).Update("assignee_logins", merged).Error; err != nil {
		return db.PullRequest{}, err
	}
	if len(added) > 0 {
		if err := s.createAssignmentNotificationsForLogins(ctx, s.notificationActorIDForContext(ctx), NotificationSubjectPullRequest, pr.ID, pr.RepositoryID, added); err != nil {
			return db.PullRequest{}, err
		}
	}
	if err := preloadPRFull(s.DBForCtx(ctx)).First(&pr, pr.ID).Error; err != nil {
		return pr, wrapErr(err)
	}
	return pr, nil
}

// RemovePRAssignees removes assignees (by login) from a PR. Returns the updated PR.
func (s *Service) RemovePRAssignees(ctx context.Context, repoFullName string, number int, logins []string) (db.PullRequest, error) {
	rep, err := s.GetRepo(ctx, repoFullName)
	if err != nil {
		return db.PullRequest{}, err
	}
	var pr db.PullRequest
	if err := s.DBForCtx(ctx).First(&pr, "repository_id = ? AND number = ?", rep.ID, number).Error; err != nil {
		return db.PullRequest{}, wrapErrf(err, "pull request #%d", number)
	}
	remaining := removeLogins(pr.AssigneeLogins, logins)
	if err := s.DBForCtx(ctx).Model(&pr).Update("assignee_logins", remaining).Error; err != nil {
		return db.PullRequest{}, err
	}
	if err := preloadPRFull(s.DBForCtx(ctx)).First(&pr, pr.ID).Error; err != nil {
		return pr, wrapErr(err)
	}
	return pr, nil
}

// SetPRAssignees replaces all assignees on a PR with the given logins.
func (s *Service) SetPRAssignees(ctx context.Context, repoFullName string, number int, logins []string) (db.PullRequest, error) {
	rep, err := s.GetRepo(ctx, repoFullName)
	if err != nil {
		return db.PullRequest{}, err
	}
	var pr db.PullRequest
	if err := s.DBForCtx(ctx).First(&pr, "repository_id = ? AND number = ?", rep.ID, number).Error; err != nil {
		return db.PullRequest{}, wrapErrf(err, "pull request #%d", number)
	}
	existingSet := loginSet(pr.AssigneeLogins)
	var added []string
	for _, login := range logins {
		if !existingSet[login] {
			added = append(added, login)
		}
	}
	newVal := strings.Join(logins, ",")
	if err := s.DBForCtx(ctx).Model(&pr).Update("assignee_logins", newVal).Error; err != nil {
		return db.PullRequest{}, err
	}
	if len(added) > 0 {
		if err := s.createAssignmentNotificationsForLogins(ctx, s.notificationActorIDForContext(ctx), NotificationSubjectPullRequest, pr.ID, pr.RepositoryID, added); err != nil {
			return db.PullRequest{}, err
		}
	}
	if err := preloadPRFull(s.DBForCtx(ctx)).First(&pr, pr.ID).Error; err != nil {
		return pr, wrapErr(err)
	}
	return pr, nil
}

// UpdatePRCommitData fetches commit messages and filenames from git and updates the PR.
// This is called after PR creation and when new commits are pushed.
func (s *Service) UpdatePRCommitData(ctx context.Context, repoFullName string, number int) error {
	rep, err := s.GetRepo(ctx, repoFullName)
	if err != nil {
		return err
	}
	var pr db.PullRequest
	if err := s.DBForCtx(ctx).First(&pr, "repository_id = ? AND number = ?", rep.ID, number).Error; err != nil {
		return wrapErrf(err, "pull request #%d", number)
	}

	headFullName := rep.FullName
	if pr.HeadRepositoryID != 0 && pr.HeadRepositoryID != rep.ID {
		var headRepo db.Repository
		if err := s.DBForCtx(ctx).Select("full_name").First(&headRepo, pr.HeadRepositoryID).Error; err == nil && headRepo.FullName != "" {
			headFullName = headRepo.FullName
		}
	}

	return s.updatePRCommitDataFromLoaded(ctx, rep.FullName, headFullName, pr)
}

func (s *Service) updatePRCommitDataFromLoaded(ctx context.Context, baseFullName, headFullName string, pr db.PullRequest) error {
	// Skip if we don't have SHAs
	if pr.BaseSHA == "" || pr.HeadSHA == "" {
		return nil
	}
	if headFullName == "" {
		headFullName = baseFullName
	}

	// Ensure the base repo has the head commit (needed for cross-repo PRs).
	if err := s.Git.CreatePRRef(ctx, baseFullName, headFullName, pr.HeadSHA, pr.Number); err != nil {
		slog.Warn("UpdatePRCommitData: failed to sync PR ref", "error", err)
	}

	// Fetch commits and files from git
	result, err := s.Git.Compare(ctx, baseFullName, pr.BaseSHA, pr.HeadSHA)
	if err != nil {
		// Log but don't fail - git operations can fail for various reasons
		slog.Warn("UpdatePRCommitData: failed to compare", "error", err)
		return nil
	}

	// Concatenate commit messages
	var commitMessages strings.Builder
	for i, commit := range result.Commits {
		if i > 0 {
			commitMessages.WriteString(" ")
		}
		commitMessages.WriteString(commit.Message)
	}

	// Concatenate filenames (comma-separated)
	filenames := make([]string, 0, len(result.Files))
	for _, f := range result.Files {
		filenames = append(filenames, f.Filename)
	}

	// Update the PR
	updates := map[string]any{
		"commit_messages": commitMessages.String(),
		"filenames":       strings.Join(filenames, ","),
	}
	if err := s.DBForCtx(ctx).Model(&db.PullRequest{}).Where("id = ?", pr.ID).Updates(updates).Error; err != nil {
		return err
	}

	return nil
}
