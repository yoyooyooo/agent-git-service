package router

import (
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/ngaut/agent-git-service/internal/buildinfo"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

// Handle only GET / without registering a route: adding a GET route would turn
// the historical 404 for HEAD and other methods into 405.
func serviceRootMiddleware(integration *forgejointegration.Integration, routes chi.Router) func(http.Handler) http.Handler {
	// Inspect the completed route table on first use so composition can register
	// a root handler after RegisterRoutes. Any method at / owns its 405 response.
	hasRootRoute := sync.OnceValue(func() bool {
		found := false
		_ = chi.Walk(routes, func(method, _ string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			if routes.Find(chi.NewRouteContext(), method, "/") != "" {
				found = true
			}
			return nil
		})
		return found
	})
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.EscapedPath() != "/" || isGitWebRequest(r) || !acceptsRootHTML(r) || hasRootRoute() {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			if base := integration.WebRedirectBaseURL(); base != "" {
				// Keep the configured path prefix and drop the AGS query string.
				http.Redirect(w, r, base+"/", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = fmt.Fprintf(w, "agent-git-service (AGS)\nVersion: %s\nGit and pull request service.\nHealth check: /readyz\n", buildinfo.Version)
		})
	}
}

// Require explicit HTML acceptance without JSON in any Accept or Content-Type
// value. Mixed API/browser requests retain their historical routing semantics.
func acceptsRootHTML(r *http.Request) bool {
	acceptsHTML := false
	for _, header := range []string{"Accept", "Content-Type"} {
		for _, value := range r.Header.Values(header) {
			for _, entry := range strings.Split(value, ",") {
				mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(entry))
				if err != nil {
					continue
				}
				if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
					return false
				}
				if header != "Accept" || (mediaType != "text/html" && mediaType != "application/xhtml+xml") {
					continue
				}
				if raw, ok := params["q"]; ok {
					q, err := strconv.ParseFloat(raw, 64)
					if err != nil || !(q > 0 && q <= 1) {
						continue
					}
				}
				acceptsHTML = true
			}
		}
	}
	return acceptsHTML
}
