package forgejointegration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxResponseBodyBytes = 8 * 1024 * 1024
	defaultHTTPTimeout   = 60 * time.Second
)

// HTTPClient calls Forgejo's Gitea-compatible REST API.
type HTTPClient struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPClient constructs a Forgejo REST client.
func NewHTTPClient(baseURL, token string) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: defaultHTTPTimeout},
	}
}

// GetRepository reads the exact provider repository identity without credentials.
func (c *HTTPClient) GetRepository(ctx context.Context, owner, repo string) (RepositorySnapshot, bool, error) {
	status, response, err := c.request(ctx, http.MethodGet, apiPath("/api/v1/repos/%s/%s", owner, repo), nil)
	if err != nil {
		return RepositorySnapshot{}, false, err
	}
	if status == http.StatusNotFound {
		return RepositorySnapshot{}, false, nil
	}
	if status < 200 || status >= 300 {
		return RepositorySnapshot{}, false, fmt.Errorf("get repository returned status %d: %s", status, string(response))
	}
	var payload struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		HTMLURL       string `json:"html_url"`
		CloneURL      string `json:"clone_url"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(response, &payload); err != nil {
		return RepositorySnapshot{}, false, fmt.Errorf("decode repository: %w", err)
	}
	want := owner + "/" + repo
	if payload.ID <= 0 || payload.FullName != want {
		return RepositorySnapshot{}, false, fmt.Errorf("repository identity mismatch for %s", want)
	}
	htmlURL, err := secretFreeRepositoryURL(payload.HTMLURL, "html_url")
	if err != nil {
		return RepositorySnapshot{}, false, err
	}
	cloneURL, err := secretFreeRepositoryURL(payload.CloneURL, "clone_url")
	if err != nil {
		return RepositorySnapshot{}, false, err
	}
	defaultBranch := strings.TrimSpace(payload.DefaultBranch)
	if defaultBranch == "" || strings.ContainsAny(defaultBranch, "\r\n\x00") {
		return RepositorySnapshot{}, false, fmt.Errorf("repository default_branch is invalid for %s", want)
	}
	branchStatus, branchResponse, err := c.request(ctx, http.MethodGet, apiPath("/api/v1/repos/%s/%s/branches/%s", owner, repo, defaultBranch), nil)
	if err != nil {
		return RepositorySnapshot{}, false, err
	}
	if branchStatus < 200 || branchStatus >= 300 {
		return RepositorySnapshot{}, false, fmt.Errorf("get default branch returned status %d: %s", branchStatus, string(branchResponse))
	}
	var branch struct {
		Name   string `json:"name"`
		Commit struct {
			ID  string `json:"id"`
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(branchResponse, &branch); err != nil {
		return RepositorySnapshot{}, false, fmt.Errorf("decode default branch: %w", err)
	}
	defaultBranchSHA := firstNonEmpty(branch.Commit.ID, branch.Commit.SHA)
	if branch.Name != defaultBranch || !isFullGitObjectID(defaultBranchSHA) {
		return RepositorySnapshot{}, false, fmt.Errorf("repository default branch identity mismatch for %s", want)
	}
	return RepositorySnapshot{
		ID: payload.ID, FullName: payload.FullName, HTMLURL: htmlURL, CloneURL: cloneURL,
		DefaultBranch: defaultBranch, DefaultBranchSHA: strings.ToLower(defaultBranchSHA),
	}, true, nil
}

func secretFreeRepositoryURL(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("repository %s is not a secret-free HTTP(S) URL", field)
	}
	return value, nil
}

// EnsureRepository creates the exact target repository when it is missing.
func (c *HTTPClient) EnsureRepository(ctx context.Context, owner, repo string, private bool) error {
	repoPath := apiPath("/api/v1/repos/%s/%s", owner, repo)
	status, response, err := c.request(ctx, http.MethodGet, repoPath, nil)
	if err != nil {
		return err
	}
	if status >= 200 && status < 300 {
		return verifyRepositoryFullName(response, owner, repo)
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("lookup repository returned status %d", status)
	}
	createPath, err := c.repositoryCreatePath(ctx, owner)
	if err != nil {
		return err
	}
	body := map[string]any{
		"name":      repo,
		"private":   private,
		"auto_init": false,
	}
	status, response, err = c.request(ctx, http.MethodPost, createPath, body)
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK && status != http.StatusConflict {
		return fmt.Errorf("create repository returned status %d: %s", status, string(response))
	}
	status, response, err = c.request(ctx, http.MethodGet, repoPath, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("verify repository returned status %d: %s", status, string(response))
	}
	return verifyRepositoryFullName(response, owner, repo)
}
func (c *HTTPClient) repositoryCreatePath(ctx context.Context, owner string) (string, error) {
	status, response, err := c.request(ctx, http.MethodGet, apiPath("/api/v1/orgs/%s", owner), nil)
	if err != nil {
		return "", err
	}
	if status >= 200 && status < 300 {
		return apiPath("/api/v1/orgs/%s/repos", owner), nil
	}
	if status != http.StatusNotFound {
		return "", fmt.Errorf("lookup organization returned status %d: %s", status, string(response))
	}
	status, response, err = c.request(ctx, http.MethodGet, "/api/v1/user", nil)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("lookup authenticated user returned status %d: %s", status, string(response))
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(response, &user); err != nil {
		return "", fmt.Errorf("decode authenticated user: %w", err)
	}
	if user.Login == owner {
		return "/api/v1/user/repos", nil
	}
	return "", fmt.Errorf("repository owner %s is neither an existing organization nor the authenticated user %s", owner, user.Login)
}
func verifyRepositoryFullName(raw []byte, owner, repo string) error {
	var payload struct {
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode repository: %w", err)
	}
	fullName := payload.FullName
	if fullName == "" && payload.Owner.Login != "" && payload.Name != "" {
		fullName = payload.Owner.Login + "/" + payload.Name
	}
	if fullName == "" {
		fullName = owner + "/" + repo
	}
	want := owner + "/" + repo
	if fullName != want {
		return fmt.Errorf("repository full_name mismatch: got %s, want %s", fullName, want)
	}
	return nil
}

// ListPullRequests reads every provider page so old projections remain visible
// to reconciliation and integrity checks after the repository exceeds the
// provider's default page size.
func (c *HTTPClient) ListPullRequests(ctx context.Context, owner, repo, state string) ([]PullRequestSnapshot, error) {
	const pageSize = 50
	var out []PullRequestSnapshot
	for page := 1; page <= 100; page++ {
		query := url.Values{}
		query.Set("state", strings.TrimSpace(state))
		query.Set("limit", fmt.Sprint(pageSize))
		query.Set("page", fmt.Sprint(page))
		listPath := apiPath("/api/v1/repos/%s/%s/pulls", owner, repo) + "?" + query.Encode()
		status, response, err := c.request(ctx, http.MethodGet, listPath, nil)
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("list pull requests returned status %d: %s", status, string(response))
		}
		rows, err := decodePullRequestSnapshots(response)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(rows) < pageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("list pull requests exceeded 100 pages for %s/%s", owner, repo)
}

// GetPullRequest reads one PR by number so integrity confirmation does not depend
// on a multi-page provider snapshot that can change while it is being collected.
func (c *HTTPClient) GetPullRequest(ctx context.Context, owner, repo string, number int) (PullRequestSnapshot, bool, error) {
	pullPath := apiPath("/api/v1/repos/%s/%s/pulls", owner, repo) + "/" + url.PathEscape(fmt.Sprint(number))
	status, response, err := c.request(ctx, http.MethodGet, pullPath, nil)
	if err != nil {
		return PullRequestSnapshot{}, false, err
	}
	if status == http.StatusNotFound {
		return PullRequestSnapshot{}, false, nil
	}
	if status < 200 || status >= 300 {
		return PullRequestSnapshot{}, false, fmt.Errorf("get pull request returned status %d: %s", status, string(response))
	}
	var payload pullRequestPayload
	if err := json.Unmarshal(response, &payload); err != nil {
		return PullRequestSnapshot{}, false, fmt.Errorf("decode pull request: %w", err)
	}
	return pullRequestSnapshotFromPayload(payload), true, nil
}

// EnsurePullRequest creates a PR unless an open PR already exists for the same head/base.
// Existing PRs are reconciled so an AGS PR created after an earlier push-created
// Forgejo PR can still project the AGS title/body.
func (c *HTTPClient) EnsurePullRequest(ctx context.Context, in PullRequestRequest) (PullRequestResult, error) {
	listPath := apiPath("/api/v1/repos/%s/%s/pulls", in.Owner, in.Repo) + "?state=open"
	status, response, err := c.request(ctx, http.MethodGet, listPath, nil)
	if err != nil {
		return PullRequestResult{}, err
	}
	if in.Title == "" {
		in.Title = fmt.Sprintf("%s -> %s", in.Head, in.Base)
	}
	if status >= 200 && status < 300 {
		pr, ok, decodeErr := findPullRequestSnapshot(response, in.Head, in.Base)
		if decodeErr != nil {
			return PullRequestResult{}, decodeErr
		}
		if ok {
			if in.SkipMetadataReconcile {
				return pr.result(), nil
			}
			return c.reconcilePullRequestMetadata(ctx, in, pr)
		}
	} else if status != http.StatusNotFound {
		return PullRequestResult{}, fmt.Errorf("list pull requests returned status %d: %s", status, string(response))
	}
	body := map[string]any{
		"head":  in.Head,
		"base":  in.Base,
		"title": in.Title,
		"body":  in.Body,
	}
	status, response, err = c.request(ctx, http.MethodPost, apiPath("/api/v1/repos/%s/%s/pulls", in.Owner, in.Repo), body)
	if err != nil {
		return PullRequestResult{}, err
	}
	if status == http.StatusCreated || status == http.StatusOK {
		if pr, ok := decodePullRequestResult(response); ok {
			return pr, nil
		}
		return PullRequestResult{}, nil
	}
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return PullRequestResult{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * 300 * time.Millisecond):
			}
		}
		if refetched, ok, refetchErr := c.findExistingPullRequest(ctx, in); refetchErr == nil && ok {
			if in.SkipMetadataReconcile {
				return refetched.result(), nil
			}
			return c.reconcilePullRequestMetadata(ctx, in, refetched)
		}
	}
	return PullRequestResult{}, fmt.Errorf("create pull request returned status %d: %s", status, string(response))
}

func (c *HTTPClient) findExistingPullRequest(ctx context.Context, in PullRequestRequest) (pullRequestSnapshot, bool, error) {
	listPath := apiPath("/api/v1/repos/%s/%s/pulls", in.Owner, in.Repo) + "?state=open"
	status, response, err := c.request(ctx, http.MethodGet, listPath, nil)
	if err != nil {
		return pullRequestSnapshot{}, false, err
	}
	if status < 200 || status >= 300 {
		return pullRequestSnapshot{}, false, fmt.Errorf("list pull requests returned status %d: %s", status, string(response))
	}
	pr, ok, err := findPullRequestSnapshot(response, in.Head, in.Base)
	return pr, ok, err
}

func (c *HTTPClient) reconcilePullRequestMetadata(ctx context.Context, in PullRequestRequest, existing pullRequestSnapshot) (PullRequestResult, error) {
	update := map[string]any{}
	if strings.TrimSpace(in.Title) != "" && in.Title != existing.Title {
		update["title"] = in.Title
	}
	if in.Body != existing.Body {
		update["body"] = in.Body
	}
	if len(update) == 0 {
		return existing.result(), nil
	}
	status, response, err := c.request(
		ctx,
		http.MethodPatch,
		apiPath("/api/v1/repos/%s/%s/pulls/%s", in.Owner, in.Repo, fmt.Sprint(existing.Number)),
		update,
	)
	if err != nil {
		return PullRequestResult{}, err
	}
	if status < 200 || status >= 300 {
		return PullRequestResult{}, fmt.Errorf("update pull request returned status %d: %s", status, string(response))
	}
	if pr, ok := decodePullRequestResult(response); ok {
		if pr.HeadSHA == "" {
			pr.HeadSHA = existing.HeadSHA
		}
		return pr, nil
	}
	return existing.result(), nil
}

func (c *HTTPClient) UpdatePullRequestState(ctx context.Context, owner, repo string, number int, state string) (PullRequestResult, error) {
	body := map[string]any{"state": strings.TrimSpace(state)}
	status, response, err := c.request(ctx, http.MethodPatch, apiPath("/api/v1/repos/%s/%s/pulls/%s", owner, repo, fmt.Sprint(number)), body)
	if err != nil {
		return PullRequestResult{}, err
	}
	if status < 200 || status >= 300 {
		return PullRequestResult{}, fmt.Errorf("update pull request state returned status %d: %s", status, string(response))
	}
	if pr, ok := decodePullRequestResult(response); ok {
		return pr, nil
	}
	return PullRequestResult{Number: number}, nil
}

// ListIssueLabels lists labels currently attached to a Forgejo issue or PR.
func (c *HTTPClient) MergePullRequest(ctx context.Context, owner, repo string, number int, request PullRequestMergeRequest) error {
	if number <= 0 {
		return fmt.Errorf("pull request number must be positive")
	}
	body := map[string]any{
		"Do":                        request.Method,
		"head_commit_id":            request.ExpectedHead,
		"delete_branch_after_merge": request.DeleteBranch,
	}
	if request.Title != "" {
		body["MergeTitleField"] = request.Title
	}
	if request.Message != "" {
		body["MergeMessageField"] = request.Message
	}
	status, response, err := c.request(ctx, http.MethodPost, apiPath("/api/v1/repos/%s/%s/pulls/"+strconv.Itoa(number)+"/merge", owner, repo), body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("merge pull request returned status %d: %s", status, strings.TrimSpace(string(response)))
	}
	return nil
}

func (c *HTTPClient) ListIssueLabels(ctx context.Context, owner, repo string, issueNumber int) ([]string, error) {
	status, response, err := c.request(ctx, http.MethodGet, apiPath("/api/v1/repos/%s/%s/issues/%s/labels", owner, repo, fmt.Sprint(issueNumber)), nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("list issue labels returned status %d: %s", status, string(response))
	}
	var payload []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(response, &payload); err != nil {
		return nil, fmt.Errorf("decode issue labels: %w", err)
	}
	labels := make([]string, 0, len(payload))
	for _, item := range payload {
		if label := strings.TrimSpace(item.Name); label != "" {
			labels = append(labels, label)
		}
	}
	return labels, nil
}

// AddIssueLabels adds labels to a Forgejo issue or PR.
func (c *HTTPClient) AddIssueLabels(ctx context.Context, owner, repo string, issueNumber int, labels []string) error {
	body := map[string]any{"labels": labels}
	status, response, err := c.request(ctx, http.MethodPost, apiPath("/api/v1/repos/%s/%s/issues/%s/labels", owner, repo, fmt.Sprint(issueNumber)), body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("add issue labels returned status %d: %s", status, string(response))
	}
	return nil
}

// RemoveIssueLabel removes a label from a Forgejo issue or PR. Missing labels are treated as already converged.
func (c *HTTPClient) RemoveIssueLabel(ctx context.Context, owner, repo string, issueNumber int, label string) error {
	labelID, ok, err := c.repoLabelID(ctx, owner, repo, label)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	status, response, err := c.request(ctx, http.MethodDelete, apiPath("/api/v1/repos/%s/%s/issues/%s/labels/%s", owner, repo, fmt.Sprint(issueNumber), fmt.Sprint(labelID)), nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
		return nil
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("remove issue label returned status %d: %s", status, string(response))
	}
	return nil
}

func (c *HTTPClient) repoLabelID(ctx context.Context, owner, repo, label string) (int64, bool, error) {
	status, response, err := c.request(ctx, http.MethodGet, apiPath("/api/v1/repos/%s/%s/labels", owner, repo)+"?limit=100", nil)
	if err != nil {
		return 0, false, err
	}
	if status < 200 || status >= 300 {
		return 0, false, fmt.Errorf("list labels returned status %d: %s", status, string(response))
	}
	var labels []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(response, &labels); err != nil {
		return 0, false, fmt.Errorf("decode labels: %w", err)
	}
	for _, item := range labels {
		if strings.TrimSpace(item.Name) == strings.TrimSpace(label) && item.ID > 0 {
			return item.ID, true, nil
		}
	}
	return 0, false, nil
}

// CollaboratorPermission returns a Forgejo user's effective repository permission.
func (c *HTTPClient) CollaboratorPermission(ctx context.Context, owner, repo, username string) (string, error) {
	status, response, err := c.request(ctx, http.MethodGet, apiPath("/api/v1/repos/%s/%s/collaborators/%s/permission", owner, repo, username), nil)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("lookup collaborator permission returned status %d: %s", status, string(response))
	}
	var payload struct {
		Permission string `json:"permission"`
		RoleName   string `json:"role_name"`
	}
	if err := json.Unmarshal(response, &payload); err != nil {
		return "", fmt.Errorf("decode collaborator permission: %w", err)
	}
	permission := strings.TrimSpace(payload.Permission)
	if permission == "" {
		permission = strings.TrimSpace(payload.RoleName)
	}
	return permission, nil
}

// CreateIssueComment creates a comment on a Forgejo issue or PR.
func (c *HTTPClient) CreateIssueComment(ctx context.Context, owner, repo string, issueNumber int, bodyText string) error {
	body := map[string]any{"body": bodyText}
	status, response, err := c.request(ctx, http.MethodPost, apiPath("/api/v1/repos/%s/%s/issues/%s/comments", owner, repo, fmt.Sprint(issueNumber)), body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("create issue comment returned status %d: %s", status, string(response))
	}
	return nil
}

// ListIssueComments reads one deterministic page for provider write readback.
func (c *HTTPClient) ListIssueComments(ctx context.Context, owner, repo string, issueNumber, page, limit int) ([]PullRequestComment, error) {
	if page <= 0 {
		page = 1
	}
	if limit <= 0 {
		limit = 50
	}
	path := apiPath("/api/v1/repos/%s/%s/issues/%s/comments", owner, repo, fmt.Sprint(issueNumber)) + fmt.Sprintf("?page=%d&limit=%d", page, limit)
	status, response, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("list issue comments returned status %d: %s", status, string(response))
	}
	var comments []PullRequestComment
	if err := json.Unmarshal(response, &comments); err != nil {
		return nil, fmt.Errorf("decode issue comments: %w", err)
	}
	return comments, nil
}

func (c *HTTPClient) ListWorkflowRuns(ctx context.Context, owner, repo string, limit int) ([]WorkflowRun, error) {
	runs, _, err := c.ListWorkflowRunsPage(ctx, owner, repo, 1, limit)
	return runs, err
}

// ListWorkflowRunsPage exposes provider pagination so exact-head CI evidence
// cannot turn an unscanned later page into a complete empty observation.
func (c *HTTPClient) ListWorkflowRunsPage(ctx context.Context, owner, repo string, page, limit int) ([]WorkflowRun, bool, error) {
	if page <= 0 {
		page = 1
	}
	if limit <= 0 {
		limit = 30
	}
	status, response, err := c.request(ctx, http.MethodGet, apiPath("/api/v1/repos/%s/%s/actions/tasks", owner, repo)+fmt.Sprintf("?page=%d&limit=%d", page, limit), nil)
	if err != nil {
		return nil, false, err
	}
	if status < 200 || status >= 300 {
		return nil, false, fmt.Errorf("list workflow runs returned status %d: %s", status, string(response))
	}
	var payload struct {
		TotalCount   int           `json:"total_count"`
		WorkflowRuns []WorkflowRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(response, &payload); err != nil {
		return nil, false, fmt.Errorf("decode workflow runs: %w", err)
	}
	hasMore := len(payload.WorkflowRuns) == limit
	if payload.TotalCount > 0 {
		hasMore = page*limit < payload.TotalCount
	}
	return payload.WorkflowRuns, hasMore, nil
}

func (c *HTTPClient) GetWorkflowRunLogs(ctx context.Context, owner, repo string, runID int64) ([]byte, error) {
	if runID <= 0 {
		return nil, fmt.Errorf("workflow run id is required")
	}
	path := apiPath("/api/v1/repos/%s/%s/actions/tasks/%s/logs", owner, repo, fmt.Sprint(runID))
	status, response, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		fallback := apiPath("/api/v1/repos/%s/%s/actions/runs/%s/logs", owner, repo, fmt.Sprint(runID))
		status, response, err = c.request(ctx, http.MethodGet, fallback, nil)
		if err != nil {
			return nil, err
		}
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("get workflow run logs returned status %d: %s", status, string(response))
	}
	return response, nil
}

func (c *HTTPClient) request(ctx context.Context, method, apiPath string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+apiPath, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes+1))
	if readErr != nil {
		return resp.StatusCode, nil, readErr
	}
	if len(data) > maxResponseBodyBytes {
		return resp.StatusCode, nil, fmt.Errorf("response body exceeds %d bytes", maxResponseBodyBytes)
	}
	return resp.StatusCode, data, nil
}

func apiPath(format string, parts ...string) string {
	escaped := make([]any, 0, len(parts))
	for _, part := range parts {
		escaped = append(escaped, url.PathEscape(part))
	}
	return fmt.Sprintf(format, escaped...)
}

type pullRequestSnapshot struct {
	Number  int
	URL     string
	Title   string
	Body    string
	HeadRef string
	HeadSHA string
	BaseRef string
}

type pullRequestPayload struct {
	Number         int    `json:"number"`
	HTMLURL        string `json:"html_url"`
	URL            string `json:"url"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	Mergeable      bool   `json:"mergeable"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	MergeBaseSHA   string `json:"merge_base"`
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func decodePullRequestSnapshots(raw []byte) ([]PullRequestSnapshot, error) {
	var pulls []pullRequestPayload
	if err := json.Unmarshal(raw, &pulls); err != nil {
		return nil, fmt.Errorf("decode pull request list: %w", err)
	}
	out := make([]PullRequestSnapshot, 0, len(pulls))
	for _, pr := range pulls {
		out = append(out, pullRequestSnapshotFromPayload(pr))
	}
	return out, nil
}

func pullRequestSnapshotFromPayload(pr pullRequestPayload) PullRequestSnapshot {
	labels := make([]string, 0, len(pr.Labels))
	for _, label := range pr.Labels {
		if name := strings.TrimSpace(label.Name); name != "" {
			labels = append(labels, name)
		}
	}
	return PullRequestSnapshot{
		Number: pr.Number, URL: firstNonEmpty(pr.HTMLURL, pr.URL), State: pr.State,
		Merged: pr.Merged, Mergeable: pr.Mergeable, MergeCommitSHA: pr.MergeCommitSHA, MergeBaseSHA: pr.MergeBaseSHA,
		HeadRef: pr.Head.Ref, HeadSHA: pr.Head.SHA, BaseRef: pr.Base.Ref, BaseSHA: pr.Base.SHA,
		Body: pr.Body, Labels: labels,
	}
}

func (p pullRequestSnapshot) result() PullRequestResult {
	return PullRequestResult{Number: p.Number, URL: p.URL, HeadSHA: p.HeadSHA}
}

func findPullRequest(raw []byte, head, base string) (PullRequestResult, bool, error) {
	pr, ok, err := findPullRequestSnapshot(raw, head, base)
	if err != nil || !ok {
		return PullRequestResult{}, false, err
	}
	return pr.result(), true, nil
}

func findPullRequestSnapshot(raw []byte, head, base string) (pullRequestSnapshot, bool, error) {
	var pulls []pullRequestPayload
	if err := json.Unmarshal(raw, &pulls); err != nil {
		return pullRequestSnapshot{}, false, fmt.Errorf("decode pull request list: %w", err)
	}
	for _, pr := range pulls {
		if pr.Head.Ref == head && pr.Base.Ref == base {
			return pullRequestSnapshot{
				Number:  pr.Number,
				URL:     firstNonEmpty(pr.HTMLURL, pr.URL),
				Title:   pr.Title,
				Body:    pr.Body,
				HeadRef: pr.Head.Ref,
				HeadSHA: pr.Head.SHA,
				BaseRef: pr.Base.Ref,
			}, true, nil
		}
	}
	return pullRequestSnapshot{}, false, nil
}

func decodePullRequestResult(raw []byte) (PullRequestResult, bool) {
	var pr struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		URL     string `json:"url"`
		Head    struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil {
		return PullRequestResult{}, false
	}
	if pr.Number == 0 {
		return PullRequestResult{}, false
	}
	return PullRequestResult{Number: pr.Number, URL: firstNonEmpty(pr.HTMLURL, pr.URL), HeadSHA: pr.Head.SHA}, true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
