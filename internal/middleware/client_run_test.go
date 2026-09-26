package middleware

import (
	"net/http/httptest"
	"testing"
)

func TestClientRunDoesNotMintDurableCredentialsOrBlockNormalCollaboration(t *testing.T) {
	for _, test := range []struct {
		method, path string
		allow        bool
	}{
		{"GET", "/api/ext/v1/user/tokens", false}, {"POST", "/api/ext/v1/user/tokens", false},
		{"POST", "/api/v3/user/keys", false}, {"POST", "/api/v3/repos/team/project/keys", false},
		{"POST", "/api/graphql", true}, {"POST", "/api/v3/repos/team/keys/pulls", true},
		{"POST", "/team/project.git/git-receive-pack", true}, {"POST", "/api/v3/repos/team/project/pulls/1/comments", true},
		{"GET", "/api/v3/repos/team/project/actions/runs", true}, {"DELETE", "/api/ext/v1/client-runs/fixture-run", true},
	} {
		if got := clientRunCredentialManagementAllowed(httptest.NewRequest(test.method, test.path, nil)); got != test.allow {
			t.Errorf("%s %s: allowed=%v", test.method, test.path, got)
		}
	}
}
