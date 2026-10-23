package router

import (
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/ngaut/agent-git-service/internal/buildinfo"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

// Handle only GET / without registering a route: adding a GET route would turn
// the historical 404 for HEAD and other methods into 405.
func serviceRootMiddleware(integration *forgejointegration.Integration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.EscapedPath() != "/" || isGitWebRequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			if base := integration.WebRedirectBaseURL(); base != "" && acceptsRootHTML(r) {
				// Keep the configured path prefix and drop the AGS query string.
				http.Redirect(w, r, base+"/", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = fmt.Fprintf(w, "agent-git-service (AGS)\nVersion: %s\nGit and pull request service.\nHealth check: /readyz\n", buildinfo.Version)
		})
	}
}

// Require an explicit HTML media type; API clients' */* is not a browser signal.
// Git user agents and media types have already been excluded by the caller.
func acceptsRootHTML(r *http.Request) bool {
	for _, value := range r.Header.Values("Accept") {
		for _, entry := range strings.Split(value, ",") {
			mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(entry))
			if err != nil || (mediaType != "text/html" && mediaType != "application/xhtml+xml") {
				continue
			}
			if raw, ok := params["q"]; ok {
				q, err := strconv.ParseFloat(raw, 64)
				if err != nil || !(q > 0 && q <= 1) {
					continue
				}
			}
			return true
		}
	}
	return false
}
