package forgejointegration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type branchSHAGetter interface {
	GetBranchSHA(context.Context, string, string, string) (string, bool, error)
}

// GetBranchSHA reads the provider's live branch, independently of any PR base.
func (c *HTTPClient) GetBranchSHA(ctx context.Context, owner, repo, branch string) (string, bool, error) {
	status, body, err := c.request(ctx, http.MethodGet, apiPath("/api/v1/repos/%s/%s/branches/%s", owner, repo, branch), nil)
	if err != nil {
		return "", false, err
	}
	if status == http.StatusNotFound {
		return "", false, nil
	}
	if status < 200 || status >= 300 {
		return "", false, providerHTTPError("get branch", status)
	}
	var observed struct {
		Name   string `json:"name"`
		Commit struct {
			ID  string `json:"id"`
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &observed); err != nil {
		return "", false, fmt.Errorf("decode branch: invalid provider response")
	}
	sha := firstNonEmpty(observed.Commit.ID, observed.Commit.SHA)
	if observed.Name != branch || sha != strings.TrimSpace(sha) || !isFullGitObjectID(sha) || (observed.Commit.ID != "" && observed.Commit.SHA != "" && !strings.EqualFold(observed.Commit.ID, observed.Commit.SHA)) {
		return "", false, fmt.Errorf("provider branch identity mismatch")
	}
	return strings.ToLower(sha), true, nil
}

func (i *Integration) ExactBranchSHA(ctx context.Context, repoFullName, externalRepo, branch string) (string, bool, error) {
	target, err := i.ConfiguredTargetFullName(repoFullName)
	if err != nil {
		return "", false, err
	}
	if target != externalRepo {
		return "", false, fmt.Errorf("Forgejo branch target does not match configured repository")
	}
	getter, ok := i.client.(branchSHAGetter)
	if !ok {
		return "", false, fmt.Errorf("Forgejo client cannot inspect an exact branch")
	}
	configured, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return "", false, err
	}
	return getter.GetBranchSHA(ctx, configured.Owner, configured.Repo, branch)
}
