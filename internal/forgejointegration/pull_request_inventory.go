package forgejointegration

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

type pullRequestPageLister interface {
	ListPullRequestsPage(context.Context, string, string, string, int, int) ([]PullRequestSnapshot, error)
}

// ListPullRequestsPage observes one provider page. The caller must continue to
// an empty page: a provider may cap limit below the requested page size.
func (c *HTTPClient) ListPullRequestsPage(ctx context.Context, owner, repo, state string, page, perPage int) ([]PullRequestSnapshot, error) {
	query := url.Values{
		"state": {state}, "page": {strconv.Itoa(page)},
		"limit": {strconv.Itoa(perPage)}, "sort": {"oldest"},
	}
	path := apiPath("/api/v1/repos/%s/%s/pulls", owner, repo) + "?" + query.Encode()
	status, body, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, providerHTTPError("list pull request page", status)
	}
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return nil, fmt.Errorf("decode pull request page: expected an array")
	}
	return decodePullRequestSnapshots(body)
}

// PullRequestInventoryPage uses only the configured repository target and GET.
// It does not create repositories, PRs, projection mappings or repair jobs.
func (i *Integration) PullRequestInventoryPage(ctx context.Context, repoFullName, externalRepo, state string, page, perPage int) ([]PullRequestSnapshot, error) {
	target, err := i.ConfiguredTargetFullName(repoFullName)
	if err != nil {
		return nil, err
	}
	if target != externalRepo {
		return nil, fmt.Errorf("Forgejo inventory target does not match configured repository")
	}
	lister, ok := i.client.(pullRequestPageLister)
	if !ok {
		return nil, fmt.Errorf("Forgejo client cannot inspect a pull request page")
	}
	configured, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return nil, err
	}
	return lister.ListPullRequestsPage(ctx, configured.Owner, configured.Repo, state, page, perPage)
}
