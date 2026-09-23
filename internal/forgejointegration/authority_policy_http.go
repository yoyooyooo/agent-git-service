package forgejointegration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func (c *HTTPClient) InspectRepositoryAuthority(ctx context.Context, owner, repo, baseBranch, webhookURL, integrationBot string) (RepositoryAuthorityState, error) {
	state := RepositoryAuthorityState{Labels: map[string]bool{}}
	status, response, err := c.request(ctx, http.MethodGet, apiPath("/api/v1/repos/%s/%s", owner, repo), nil)
	if err != nil {
		return state, err
	}
	if status < 200 || status >= 300 {
		return state, fmt.Errorf("lookup repository authority returned status %d: %s", status, string(response))
	}
	var repository struct {
		AllowRebaseUpdate bool `json:"allow_rebase_update"`
	}
	if err := json.Unmarshal(response, &repository); err != nil {
		return state, fmt.Errorf("decode repository authority: %w", err)
	}
	state.AllowRebaseUpdate = repository.AllowRebaseUpdate

	collaboratorPath := apiPath("/api/v1/repos/%s/%s/collaborators/%s/permission", owner, repo, integrationBot)
	status, response, err = c.request(ctx, http.MethodGet, collaboratorPath, nil)
	if err != nil {
		return state, err
	}
	if status >= 200 && status < 300 {
		var permission struct {
			Permission string `json:"permission"`
			RoleName   string `json:"role_name"`
		}
		if err := json.Unmarshal(response, &permission); err != nil {
			return state, fmt.Errorf("decode integration bot permission: %w", err)
		}
		effective := strings.ToLower(strings.TrimSpace(permission.Permission))
		if effective == "" {
			effective = strings.ToLower(strings.TrimSpace(permission.RoleName))
		}
		state.IntegrationBotCollaborator = effective == "write" || effective == "admin" || effective == "owner"
	} else if status != http.StatusNotFound {
		return state, fmt.Errorf("lookup integration bot permission returned status %d: %s", status, string(response))
	}

	protectionPath := apiPath("/api/v1/repos/%s/%s/branch_protections/%s", owner, repo, baseBranch)
	status, response, err = c.request(ctx, http.MethodGet, protectionPath, nil)
	if err != nil {
		return state, err
	}
	if status >= 200 && status < 300 {
		var protection struct {
			EnablePush              bool     `json:"enable_push"`
			EnableForcePush         bool     `json:"enable_force_push"`
			EnablePushWhitelist     bool     `json:"enable_push_whitelist"`
			PushWhitelistUsernames  []string `json:"push_whitelist_usernames"`
			EnableMergeWhitelist    bool     `json:"enable_merge_whitelist"`
			MergeWhitelistUsernames []string `json:"merge_whitelist_usernames"`
			ApplyToAdmins           bool     `json:"apply_to_admins"`
		}
		if err := json.Unmarshal(response, &protection); err != nil {
			return state, fmt.Errorf("decode branch protection: %w", err)
		}
		state.BaseBranchProtected = true
		state.DirectPushBlocked = protection.EnablePush && protection.EnablePushWhitelist
		state.ForcePushBlocked = !protection.EnableForcePush
		botMatched := false
		for _, username := range protection.PushWhitelistUsernames {
			if strings.TrimSpace(username) == "" {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(username), strings.TrimSpace(integrationBot)) {
				botMatched = true
			}
		}
		state.IntegrationBotAuthorized = !protection.EnablePushWhitelist || botMatched
		mergeBotMatched := false
		for _, username := range protection.MergeWhitelistUsernames {
			username = strings.TrimSpace(username)
			if username == "" {
				continue
			}
			state.MergeWhitelistUsernames = append(state.MergeWhitelistUsernames, username)
			if strings.EqualFold(username, strings.TrimSpace(integrationBot)) {
				mergeBotMatched = true
			}
		}
		// Open push/merge whitelists are owner policy: the bot can write as a
		// collaborator. A closed whitelist still has to include the bot.
		state.IntegrationBotMergeAuthorized = !protection.EnableMergeWhitelist || mergeBotMatched
	} else if status != http.StatusNotFound {
		return state, fmt.Errorf("lookup branch protection returned status %d: %s", status, string(response))
	}

	const pageSize = 50
	for page := 1; page <= 100; page++ {
		labelsPath := apiPath("/api/v1/repos/%s/%s/labels", owner, repo) + fmt.Sprintf("?limit=%d&page=%d", pageSize, page)
		status, response, err = c.request(ctx, http.MethodGet, labelsPath, nil)
		if err != nil {
			return state, err
		}
		if status < 200 || status >= 300 {
			return state, fmt.Errorf("list authority labels returned status %d: %s", status, string(response))
		}
		var labels []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(response, &labels); err != nil {
			return state, fmt.Errorf("decode authority labels: %w", err)
		}
		for _, label := range labels {
			state.Labels[strings.TrimSpace(label.Name)] = true
		}
		if len(labels) < pageSize {
			break
		}
	}

	status, response, err = c.request(ctx, http.MethodGet, apiPath("/api/v1/repos/%s/%s/hooks", owner, repo)+"?limit=100", nil)
	if err != nil {
		return state, err
	}
	if status < 200 || status >= 300 {
		return state, fmt.Errorf("list authority webhooks returned status %d: %s", status, string(response))
	}
	var hooks []struct {
		ID     int64             `json:"id"`
		Active bool              `json:"active"`
		Events []string          `json:"events"`
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal(response, &hooks); err != nil {
		return state, fmt.Errorf("decode authority webhooks: %w", err)
	}
	for _, hook := range hooks {
		if strings.TrimSpace(hook.Config["url"]) != strings.TrimSpace(webhookURL) {
			continue
		}
		state.WebhookID = hook.ID
		state.WebhookActive = hook.Active
		for _, event := range hook.Events {
			switch strings.TrimSpace(event) {
			case "pull_request":
				state.WebhookPullRequests = true
			case "issues":
				state.WebhookIssues = true
			case "delete":
				state.WebhookDelete = true
			}
		}
		break
	}
	return state, nil
}

func (c *HTTPClient) ApplyRepositoryAuthority(ctx context.Context, owner, repo, baseBranch, webhookURL, webhookSecret, integrationBot string, current RepositoryAuthorityState) error {
	if current.AllowRebaseUpdate {
		if err := c.authorityRequest(ctx, http.MethodPatch, apiPath("/api/v1/repos/%s/%s", owner, repo), map[string]any{"allow_rebase_update": false}); err != nil {
			return err
		}
	}
	if !current.IntegrationBotCollaborator {
		if err := c.authorityRequest(ctx, http.MethodPut, apiPath("/api/v1/repos/%s/%s/collaborators/%s", owner, repo, integrationBot), map[string]any{"permission": "write"}); err != nil {
			return err
		}
	}
	if !current.BaseBranchProtected || !current.ForcePushBlocked || current.DirectPushBlocked {
		body := map[string]any{
			"branch_name":               baseBranch,
			"enable_push":               true,
			"enable_force_push":         false,
			"enable_push_whitelist":     false,
			"push_whitelist_usernames":  []string{},
			"enable_merge_whitelist":    false,
			"merge_whitelist_usernames": []string{},
			"apply_to_admins":           false,
		}
		method := http.MethodPost
		requestPath := apiPath("/api/v1/repos/%s/%s/branch_protections", owner, repo)
		if current.BaseBranchProtected {
			method = http.MethodPatch
			requestPath = apiPath("/api/v1/repos/%s/%s/branch_protections/%s", owner, repo, baseBranch)
			delete(body, "branch_name")
		}
		if err := c.authorityRequest(ctx, method, requestPath, body); err != nil {
			return err
		}
	}
	for _, label := range authorityWorkflowLabels {
		if current.Labels[label] {
			continue
		}
		if err := c.authorityRequest(ctx, http.MethodPost, apiPath("/api/v1/repos/%s/%s/labels", owner, repo), map[string]any{
			"name": label, "color": authorityLabelColor(label),
		}); err != nil {
			return err
		}
	}
	if !current.WebhookActive || !current.WebhookPullRequests || !current.WebhookIssues || !current.WebhookDelete {
		body := map[string]any{
			"type": "forgejo", "active": true,
			"events": []string{"pull_request", "issues", "delete"},
			"config": map[string]string{"url": webhookURL, "content_type": "json", "secret": webhookSecret},
		}
		method := http.MethodPost
		requestPath := apiPath("/api/v1/repos/%s/%s/hooks", owner, repo)
		if current.WebhookID > 0 {
			method = http.MethodPatch
			requestPath = apiPath("/api/v1/repos/%s/%s/hooks/%s", owner, repo, fmt.Sprint(current.WebhookID))
			delete(body, "type")
		}
		if err := c.authorityRequest(ctx, method, requestPath, body); err != nil {
			return err
		}
	}
	return nil
}

func mergeWhitelistWithIntegrationBot(existing []string, integrationBot string) []string {
	result := make([]string, 0, len(existing)+1)
	seen := make(map[string]struct{}, len(existing)+1)
	for _, username := range append(existing, integrationBot) {
		username = strings.TrimSpace(username)
		key := strings.ToLower(username)
		if username == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, username)
	}
	return result
}

func (c *HTTPClient) authorityRequest(ctx context.Context, method, requestPath string, body any) error {
	status, response, err := c.request(ctx, method, requestPath, body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("Forgejo authority %s %s returned status %d: %s", method, requestPath, status, string(response))
	}
	return nil
}

func authorityLabelColor(label string) string {
	if strings.Contains(label, "action-") {
		return "1d76db"
	}
	if strings.Contains(label, "drift") || strings.Contains(label, "conflict") || strings.Contains(label, "blocked") {
		return "d73a4a"
	}
	return "fbca04"
}
