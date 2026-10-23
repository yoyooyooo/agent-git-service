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
	"strconv"
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
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("browser base URL must be a credential-free absolute HTTP(S) URL without query or fragment")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("browser base URL must use http or https")
	}
	if _, ok := escapedBrowserPath(parsed.EscapedPath()); !ok {
		return "", errors.New("browser base URL contains unsafe path segments")
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
	if owner, repo, ok := splitRepoFullName(repoFullName); !ok || !ValidBrowserPathSegment(owner) || !ValidBrowserPathSegment(repo) {
		return "", false
	}
	target, ok, err := i.cfg.resolveTarget(repoFullName)
	if err != nil || !ok || !target.Enabled {
		return "", false
	}
	return i.browserURLForTarget(target.Owner, target.Repo, webPath)
}

// PullRequestBrowserURL uses the recorded provider coordinates rather than
// remapping them as AGS coordinates. ExternalURL is intentionally never trusted.
func (i *Integration) PullRequestBrowserURL(repoFullName, externalRepo string, number int) (string, bool) {
	if _, ok := i.BrowserURL(repoFullName, ""); !ok || number <= 0 {
		return "", false
	}
	parts := strings.Split(externalRepo, "/")
	if len(parts) != 2 {
		return "", false
	}
	return i.browserURLForTarget(parts[0], parts[1], "/pulls/"+strconv.Itoa(number))
}

func (i *Integration) browserURLForTarget(owner, repo, webPath string) (string, bool) {
	if !ValidBrowserPathSegment(owner) || !ValidBrowserPathSegment(repo) {
		return "", false
	}
	escaped, ok := escapedBrowserPath(webPath)
	if !ok {
		return "", false
	}
	base := i.WebRedirectBaseURL()
	if base == "" {
		return "", false
	}
	return base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + escaped, true
}

// ValidBrowserPathSegment accepts one decoded segment. Reject percent signs to
// prevent nested escapes being reinterpreted by a downstream proxy or browser.
func ValidBrowserPathSegment(value string) bool {
	if value == "" || value == "." || value == ".." || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f || strings.ContainsRune("/\\%?#", char) {
			return false
		}
	}
	return true
}

func escapedBrowserPath(value string) (string, bool) {
	if value == "" {
		return "", true
	}
	if !strings.HasPrefix(value, "/") {
		return "", false
	}
	parts := strings.Split(value[1:], "/")
	for j, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err != nil || (decoded == "" && j != len(parts)-1) || (decoded != "" && !ValidBrowserPathSegment(decoded)) {
			return "", false
		}
		parts[j] = url.PathEscape(decoded)
	}
	return "/" + strings.Join(parts, "/"), true
}
