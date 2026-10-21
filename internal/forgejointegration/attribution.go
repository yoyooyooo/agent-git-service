package forgejointegration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/attribution"
)

// ValidateAttribution performs only startup reads. Invalid mappings never become
// a half-active table; disappearance after startup uses request-local fallback.
func ValidateAttribution(ctx context.Context, cfg Config) (Config, error) {
	if err := cfg.Attribution.Validate(); err != nil {
		return cfg, err
	}
	if len(cfg.Attribution.Principals) == 0 {
		return cfg, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := NewHTTPClient(cfg.BaseURL, cfg.Token)
	status, data, err := c.request(ctx, http.MethodGet, "/api/v1/user", nil)
	if err != nil || status != http.StatusOK {
		return cfg, fmt.Errorf("validate Forgejo attribution credential: status %d", status)
	}
	var user struct {
		Login string `json:"login"`
		Admin bool   `json:"is_admin"`
	}
	if json.Unmarshal(data, &user) != nil || user.Login == "" || !user.Admin {
		return cfg, fmt.Errorf("Forgejo attribution requires the existing site-admin projection credential")
	}
	cfg.TokenOwner = user.Login
	for _, login := range cfg.Attribution.ForgejoLogins() {
		status, data, err = c.request(ctx, http.MethodGet, apiPath("/api/v1/users/%s", login), nil)
		var target struct {
			Login string `json:"login"`
		}
		if err != nil || status != http.StatusOK || json.Unmarshal(data, &target) != nil || !strings.EqualFold(target.Login, login) {
			return cfg, fmt.Errorf("invalid Forgejo attribution user %s: status %d", login, status)
		}
	}
	return cfg, nil
}

func (c *HTTPClient) configureAttribution(cfg Config) {
	c.attribution = cfg.Attribution
	c.tokenOwner = cfg.TokenOwner
	c.attributionRepos = map[string]string{}
	for ags, mapping := range cfg.RepoMap {
		if mapping.Enabled != nil && !*mapping.Enabled {
			continue
		}
		if mapping.Owner != "" && mapping.Repo != "" {
			c.attributionRepos[mapping.Owner+"/"+mapping.Repo] = ags
		}
	}
}

// Only these user-effect endpoints can use Sudo. Repository provisioning,
// protection/hooks/authority maintenance and every read remain bot-owned.
var actorWritePath = regexp.MustCompile(`^/api/v1/repos/([^/]+/[^/]+)/(pulls|issues)(?:/([0-9]+))?(?:/(merge|comments|labels)(?:/[^/]+)?)?$`)

func actorWrite(method, path string, body any) (repo, op string, number int) {
	if method == http.MethodGet || method == http.MethodHead {
		return
	}
	m := actorWritePath.FindStringSubmatch(path)
	if m == nil {
		return
	}
	repo = m[1]
	number, _ = strconv.Atoi(m[3])
	switch {
	case m[2] == "pulls" && m[3] == "" && method == http.MethodPost:
		op = "create_pr"
	case m[2] == "pulls" && m[3] != "" && m[4] == "merge" && method == http.MethodPost:
		op = "merge_pr"
	case m[2] == "pulls" && m[3] != "" && m[4] == "" && method == http.MethodPatch:
		op = "edit_pr"
		if b, ok := body.(map[string]any); ok {
			if state, _ := b["state"].(string); state != "" {
				if state == "closed" {
					op = "close_pr"
				} else if state == "open" {
					op = "reopen_pr"
				}
			}
		}
	case m[2] == "issues" && m[3] != "" && m[4] == "comments" && method == http.MethodPost:
		op = "comment"
	case m[2] == "issues" && m[3] != "" && m[4] == "labels":
		if method == http.MethodPost {
			op = "label_add"
		} else if method == http.MethodDelete {
			op = "label_remove"
		}
	}
	return
}

func markedWrite(body any, actor attribution.Actor, reason string) any {
	b, ok := body.(map[string]any)
	if !ok {
		return body
	}
	out := make(map[string]any, len(b)+1)
	for k, v := range b {
		out[k] = v
	}
	for _, key := range []string{"body", "MergeMessageField"} {
		if text, ok := b[key].(string); ok {
			out[key] = attribution.Mark(text, actor, "forgejo", "", reason)
		}
	}
	return out
}

func (c *HTTPClient) attributedRequest(ctx context.Context, method, path string, body any) (int, []byte, error) {
	repo, op, number := actorWrite(method, path, body)
	if op == "" {
		return c.rawRequest(ctx, method, path, body, "")
	}
	actor := attribution.FromContext(ctx)
	if actor.RequestID == "" {
		ctx = attribution.WithActor(ctx, actor)
		actor = attribution.FromContext(ctx)
	}
	if actor.Repo == "" {
		actor.Repo = c.attributionRepos[repo]
		ctx = attribution.WithActor(ctx, actor)
	}
	login := c.attribution.Principals[actor.Login].Forgejo
	if actor.UserID == 0 {
		login = ""
	}
	mode, reason := "sudo", ""
	if login == "" {
		mode, reason = "fallback_unmapped", "unmapped"
		if actor.Login == "" {
			mode, reason = "bot_only", ""
		}
	}
	attempt := func(sudo, mode, reason string, grant bool) (int, []byte, error) {
		payload := body
		if sudo == "" && reason != "" {
			payload = markedWrite(body, actor, reason)
		}
		status, data, err := c.rawRequest(ctx, method, path, payload, sudo)
		loggedNumber := number
		if loggedNumber == 0 && status >= 200 && status < 300 {
			if pr, ok := decodePullRequestResult(data); ok {
				loggedNumber = pr.Number
			}
		}
		attribution.Log(ctx, c.attribution, "forgejo", op, repo, c.tokenOwner, sudo, mode, loggedNumber, status, grant, reason)
		return status, data, err
	}
	status, data, err := attempt(login, mode, reason, false)
	if err != nil {
		return status, data, err
	}
	if login == "" {
		c.fallbackNote(ctx, repo, op, number, body, status, mode, reason, actor)
		return status, data, nil
	}
	// These are definitive denials, not unknown outcomes. Never replay on a
	// transport error, 5xx or a merge conflict (409).
	if status != http.StatusForbidden && status != http.StatusNotFound && !(op == "merge_pr" && status == http.StatusMethodNotAllowed) {
		return status, data, nil
	}
	reason = "forbidden"
	if status == http.StatusNotFound {
		reason = "sudo_404"
	}
	granted := false
	if op != "merge_pr" && (status == http.StatusForbidden || status == http.StatusNotFound) && c.attributionRepos[repo] != "" && actor.UserID != 0 {
		// A private repo may hide a permission denial behind 404. Validate the user
		// still exists before granting; never create a user in this path.
		userStatus, _, userErr := c.rawRequest(ctx, http.MethodGet, apiPath("/api/v1/users/%s", login), nil, "")
		if userErr == nil && userStatus == http.StatusOK {
			permission, permissionErr := c.CollaboratorPermission(ctx, strings.SplitN(repo, "/", 2)[0], strings.SplitN(repo, "/", 2)[1], login)
			if permissionErr != nil {
				reason = "grant_failed"
			}
			if permissionErr == nil && !forgejoPermissionAtLeastWrite(permission) && !strings.EqualFold(permission, "maintain") {
				grantStatus, _, grantErr := c.rawRequest(ctx, http.MethodPut, "/api/v1/repos/"+repo+"/collaborators/"+login, map[string]any{"permission": "write"}, "")
				granted = grantErr == nil && grantStatus >= 200 && grantStatus < 300
				attribution.Log(ctx, c.attribution, "forgejo", "collaborator_grant", repo, c.tokenOwner, "", "bot_only", number, grantStatus, granted, "")
				if !granted {
					reason = "grant_failed"
				}
			}
			if granted {
				status, data, err = attempt(login, "sudo", "", true)
				if err != nil {
					return status, data, err
				}
				if status != http.StatusForbidden && status != http.StatusNotFound {
					return status, data, nil
				}
			}
		}
	}
	status, data, err = attempt("", "fallback_forbidden", reason, granted)
	c.fallbackNote(ctx, repo, op, number, body, status, "fallback_forbidden", reason, actor)
	return status, data, err
}

func (i *Integration) AGSLoginForActor(login string) string { return i.cfg.Attribution.AGSLogin(login) }

func (i *Integration) AttributionEnabled() bool {
	return i != nil && len(i.cfg.Attribution.Principals) > 0
}

func (c *HTTPClient) fallbackNote(ctx context.Context, repo, op string, number int, body any, status int, mode, reason string, actor attribution.Actor) {
	if reason == "" || status < 200 || status >= 300 || number <= 0 {
		return
	}
	b, _ := body.(map[string]any)
	if _, hasBody := b["body"]; hasBody {
		return
	}
	note := attribution.Mark("AGS projected "+op+" using its server credential.", actor, "forgejo", "", reason)
	noteStatus, _, _ := c.rawRequest(ctx, http.MethodPost, "/api/v1/repos/"+repo+"/issues/"+strconv.Itoa(number)+"/comments", map[string]any{"body": note}, "")
	attribution.Log(ctx, c.attribution, "forgejo", "comment", repo, c.tokenOwner, "", mode, number, noteStatus, false, reason)
}
