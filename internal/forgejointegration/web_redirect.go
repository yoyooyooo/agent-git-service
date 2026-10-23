// Read-only browser redirects onto the configured Forgejo web UI.
//
// AGS serves no web UI, so the human-facing URLs it hands out (for example a
// pull request link) are not openable in a browser. When the optional Forgejo
// integration is enabled and has a browser-reachable base URL, the router can
// answer GET/HEAD on those web-style paths with a redirect to the matching
// Forgejo page.
//
// Nothing here participates in git smart HTTP, the REST/GraphQL API, projection
// pushes or authority decisions: callers only obtain a redirect target.
package forgejointegration

import (
	"errors"
	"net/url"
	"strings"
)

var errEmptyBrowserBaseURL = errors.New("empty browser base URL")

// ValidateWebBaseURL rejects a configured browser base URL that cannot be used
// for a redirect. An empty value is valid and means "not configured".
func ValidateWebBaseURL(raw string) error {
	_, err := normalizeBrowserBaseURL(raw)
	if errors.Is(err, errEmptyBrowserBaseURL) {
		return nil
	}
	return err
}

// normalizeBrowserBaseURL accepts only a credential-free absolute HTTP(S)
// origin/path prefix with no query or fragment, so a redirect target can never
// carry credentials, a pre-set query or a fragment.
func normalizeBrowserBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errEmptyBrowserBaseURL
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("browser base URL must be a credential-free absolute HTTP(S) URL without query or fragment")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("browser base URL must use http or https")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// WebRedirectBaseURL returns the normalized browser-reachable Forgejo origin, or
// "" when the integration is disabled or has no explicit browser base URL.
//
// There is deliberately no fallback to BaseURL: BaseURL is the API/git endpoint
// and is often an address browsers cannot reach, and an implicit fallback would
// silently change existing deployments' browser redirects.
func (i *Integration) WebRedirectBaseURL() string {
	if i == nil || !i.cfg.Enabled {
		return ""
	}
	normalized, err := normalizeBrowserBaseURL(i.cfg.WebBaseURL)
	if err != nil {
		return ""
	}
	return normalized
}

// BrowserURL maps one AGS repository and a Forgejo-relative web path onto the
// configured Forgejo web origin. The owner/repo coordinates come from the same
// RepoMap/default_owner resolution the projection path uses, so a mapped
// repository never assumes AGS and Forgejo share names.
//
// It returns false when the integration is disabled, no browser base URL is
// configured, or the repository is not a resolvable/enabled projection target.
// A false result is never a partial URL.
func (i *Integration) BrowserURL(repoFullName, webPath string) (string, bool) {
	base := i.WebRedirectBaseURL()
	if base == "" {
		return "", false
	}
	repoFullName = strings.TrimSpace(repoFullName)
	if owner, repo, ok := splitRepoFullName(repoFullName); !ok || owner == "" || repo == "" {
		return "", false
	}
	target, ok, err := i.cfg.resolveTarget(repoFullName)
	if err != nil || !ok || !target.Enabled {
		return "", false
	}
	owner := strings.TrimSpace(target.Owner)
	repo := strings.TrimSpace(target.Repo)
	if owner == "" || repo == "" {
		return "", false
	}
	webPath = strings.TrimSpace(webPath)
	if webPath != "" && !strings.HasPrefix(webPath, "/") {
		return "", false
	}
	if strings.ContainsAny(webPath, "\r\n\x00?#") {
		return "", false
	}
	return base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + webPath, true
}
