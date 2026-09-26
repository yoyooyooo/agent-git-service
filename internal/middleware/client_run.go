package middleware

import (
	"net/http"
	"strings"
)

// The run credential is a short-lived collaboration login, not a route to read
// the parent token or mint a durable replacement. This does not inspect gh argv
// or make ordinary PR/Git work depend on Task/Run association completeness.
func clientRunCredentialManagementAllowed(r *http.Request) bool {
	path := r.URL.Path
	for _, prefix := range []string{"/api/v3/user/tokens", "/api/ext/v1/user/tokens", "/api/v3/authorizations", "/api/v3/applications", "/api/ext/v1/agent-bindings", "/api/ext/v1/agent-invites"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return false
		}
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	for _, prefix := range []string{"/api/v3/user/keys", "/api/v3/user/gpg_keys", "/api/v3/user/ssh_signing_keys"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return false
		}
	}
	// Repository deploy keys also survive a run. Keep their management on the
	// independently authenticated administrator/native-credential path.
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > 5 && parts[0] == "api" && parts[1] == "v3" && parts[2] == "repos" && parts[5] == "keys" {
		return false
	}
	return true
}
