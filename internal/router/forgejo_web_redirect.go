package router

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/service"
)

// Intercept browser reads without registering new routes: chi must retain the
// historical 404/405 distinction for every other method and excluded request.
func forgejoWebRedirectMiddleware(svc *service.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if (r.Method != http.MethodGet && r.Method != http.MethodHead) || isGitWebRequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
			if len(parts) < 2 {
				next.ServeHTTP(w, r)
				return
			}
			// Decode and validate each segment once; encoded separators and nested
			// escapes are rejected before a browser/provider can reinterpret them.
			for j, part := range parts {
				decoded, err := url.PathUnescape(part)
				if err != nil || (decoded == "" && j != len(parts)-1) || (decoded != "" && !forgejointegration.ValidBrowserPathSegment(decoded)) {
					next.ServeHTTP(w, r)
					return
				}
				parts[j] = decoded
			}
			// The entire /api namespace is reserved, irrespective of registration.
			if parts[0] == "api" || parts[0] == "" || parts[1] == "" || strings.HasSuffix(parts[1], ".git") {
				next.ServeHTTP(w, r)
				return
			}
			repoFullName := parts[0] + "/" + parts[1]
			webPath, prNumber, ok := forgejoBrowserPath(parts[2:])
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			var location string
			if prNumber > 0 {
				location, ok = svc.ForgejoPullRequestBrowserURL(r.Context(), repoFullName, prNumber)
			} else {
				location, ok = svc.ForgejoIntegration.BrowserURL(repoFullName, webPath)
			}
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			// Never forward an AGS query string (which may contain credentials).
			http.Redirect(w, r, location, http.StatusFound)
		})
	}
}

func isGitWebRequest(r *http.Request) bool {
	ua := strings.ToLower(r.UserAgent())
	if strings.Contains(ua, "git/") || strings.Contains(ua, "jgit") || strings.Contains(ua, "libgit2") || strings.Contains(ua, "go-git") || strings.Contains(ua, "git-lfs") {
		return true
	}
	for _, header := range []string{"Accept", "Content-Type"} {
		for _, value := range r.Header.Values(header) {
			value = strings.ToLower(value)
			if strings.Contains(value, "application/x-git-") || strings.Contains(value, "application/vnd.git-") {
				return true
			}
		}
	}
	return false
}

// Actions are always the third path segment, even for repositories named tree,
// blob, pull, or commit. The remaining source tail retains segment boundaries.
func forgejoBrowserPath(tail []string) (string, int, bool) {
	if len(tail) == 0 || (len(tail) == 1 && tail[0] == "") {
		return "", 0, true
	}
	if len(tail) == 1 && (tail[0] == "pulls" || tail[0] == "issues") {
		return "/" + tail[0], 0, true
	}
	if len(tail) < 2 {
		return "", 0, false
	}
	switch tail[0] {
	case "pull", "pulls", "issues":
		if len(tail) != 2 || !isPositiveDecimal(tail[1]) {
			return "", 0, false
		}
		number, err := strconv.Atoi(tail[1])
		if err != nil || number <= 0 {
			return "", 0, false
		}
		if tail[0] != "issues" {
			return "", number, true
		}
		return "/issues/" + tail[1], 0, true
	case "commit", "commits":
		if len(tail) == 2 && isHexObjectIDPrefix(tail[1]) {
			return "/commit/" + tail[1], 0, true
		}
	case "tree", "blob":
		if tail[1] == "" {
			return "", 0, false
		}
		escaped := make([]string, len(tail)-1)
		for j, part := range tail[1:] {
			escaped[j] = url.PathEscape(part)
		}
		return "/src/" + strings.Join(escaped, "/"), 0, true
	}
	return "", 0, false
}

func isPositiveDecimal(value string) bool {
	if value == "" || len(value) > 18 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return value != "0"
}

func isHexObjectIDPrefix(value string) bool {
	if len(value) < 4 || len(value) > 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}
