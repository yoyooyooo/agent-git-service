package gitbackend

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackendAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, method, query string
		change              func(*Request)
		valid               bool
	}{
		{"read advertisement", "GET", "service=git-upload-pack", func(r *Request) {}, true},
		{"read RPC", "POST", "", func(r *Request) { r.Advertise = false }, true},
		{"isolated read", "GET", "service=git-upload-pack", func(r *Request) { r.IsolatedRead = true }, true},
		{"isolated receive forbidden", "GET", "service=git-receive-pack", func(r *Request) { r.IsolatedRead = true; r.Service = ReceivePack; r.AllowReceive = true }, false},
		{"isolated write opt-in forbidden", "GET", "service=git-upload-pack", func(r *Request) { r.IsolatedRead = true; r.AllowReceive = true }, false},
		{"receive disabled", "GET", "service=git-receive-pack", func(r *Request) { r.Service = ReceivePack }, false},
		{"receive explicit", "GET", "service=git-receive-pack", func(r *Request) { r.Service = ReceivePack; r.AllowReceive = true }, true},
		{"mismatched service", "GET", "service=git-receive-pack", func(r *Request) {}, false},
		{"duplicate service", "GET", "service=git-upload-pack&service=git-receive-pack", func(r *Request) {}, false},
		{"bad query", "GET", "service=git-upload-pack;service=git-receive-pack", func(r *Request) {}, false},
		{"missing service", "GET", "", func(r *Request) {}, false},
		{"unknown service", "GET", "service=unknown", func(r *Request) { r.Service = "unknown" }, false},
		{"RPC service smuggling", "POST", "service=git-receive-pack", func(r *Request) { r.Advertise = false }, false},
		{"invalid method", "DELETE", "service=git-upload-pack", func(r *Request) {}, false},
		{"traversal", "GET", "service=git-upload-pack", func(r *Request) { r.Repository = "../repo" }, false},
		{"extra segment", "GET", "service=git-upload-pack", func(r *Request) { r.Repository = "owner/a/b" }, false},
		{"no root", "GET", "service=git-upload-pack", func(r *Request) { r.ProjectRoot = "" }, false},
		{"literal percent", "GET", "service=git-upload-pack", func(r *Request) { r.Repository = "owner/percent%2Frepo" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := Request{ProjectRoot: t.TempDir(), Repository: "owner/repo", Service: UploadPack, Advertise: true}
			tc.change(&req)
			r := httptest.NewRequest(tc.method, "http://ags.test/owner/repo.git/info/refs?"+tc.query, nil)
			if err := req.validate(r); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestBackendCGIKeepsProtocolAndPolicyButDropsCredentials(t *testing.T) {
	dir := t.TempDir()
	// A controlled CGI fixture lets us verify exactly what the child receives,
	// without reading any environment secret or starting an AGS database.
	gitScript := "#!/bin/sh\nprintf '%s\\n' \"${0%/*}\"\n"
	backendScript := `#!/bin/sh
printf 'Content-Type: text/plain\r\n\r\n'
printf 'auth=%s\ncookie=%s\nproxy_auth=%s\n' "$HTTP_AUTHORIZATION" "$HTTP_COOKIE" "$HTTP_PROXY_AUTHORIZATION"
printf 'protocol=%s\npath=%s\nroot=%s\n' "$HTTP_GIT_PROTOCOL" "$PATH_INFO" "$GIT_PROJECT_ROOT"
printf 'no_system=%s\nglobal=%s\nno_replace=%s\n' "$GIT_CONFIG_NOSYSTEM" "$GIT_CONFIG_GLOBAL" "$GIT_NO_REPLACE_OBJECTS"
printf 'receive=%s\nprotected=%s\ndelegated=%s\ndelegated_refs=%s\n' "$AGS_GIT_HTTP_RECEIVE_PACK" "$AGS_GIT_HTTP_PROTECTED_REFS" "$AGS_DELEGATED_SESSION" "$AGS_DELEGATED_PROTECTED_REFS"
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(gitScript), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "git-http-backend"), []byte(backendScript), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := httptest.NewRequest("GET", "http://ags.test/owner/repo.git/info/refs?service=git-receive-pack", nil)
	r.Header.Set("Authorization", "Bearer test-only-sensitive")
	r.Header.Set("Cookie", "session=test-only-sensitive")
	r.Header.Set("Proxy-Authorization", "Basic test-only-sensitive")
	r.Header.Set("Git-Protocol", "version=2")
	rr := httptest.NewRecorder()
	err := Serve(rr, r, Request{ProjectRoot: dir, Repository: "owner/repo", Service: ReceivePack, Advertise: true, AllowReceive: true, ProtectedRefs: []string{"refs/heads/main"}, Delegated: true, DelegatedProtectedRefs: []string{"refs/heads/main", "refs/heads/release"}})
	if err != nil {
		t.Fatal(err)
	}
	body := rr.Body.String()
	if rr.Code != http.StatusOK || strings.Contains(body, "test-only-sensitive") {
		t.Fatalf("CGI credential leak or failure: %d %s", rr.Code, body)
	}
	for _, expected := range []string{"protocol=version=2", "path=/owner/repo.git/info/refs", "receive=1", "protected=refs/heads/main", "delegated=1", "delegated_refs=refs/heads/main:refs/heads/release"} {
		if !strings.Contains(body, expected) {
			t.Errorf("missing %q in %s", expected, body)
		}
	}
	if r.Header.Get("Authorization") == "" || r.Header.Get("Cookie") == "" {
		t.Fatal("mutated caller's request headers")
	}
	isolated := httptest.NewRequest("GET", "http://ags.test/owner/repo.git/info/refs?service=git-upload-pack", nil)
	isolatedResponse := httptest.NewRecorder()
	if err := Serve(isolatedResponse, isolated, Request{ProjectRoot: dir, Repository: "owner/repo", Service: UploadPack, Advertise: true, IsolatedRead: true}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"no_system=1", "global=" + os.DevNull, "no_replace=1"} {
		if !strings.Contains(isolatedResponse.Body.String(), expected) {
			t.Fatalf("missing isolation %q", expected)
		}
	}
}

func TestBackendDefaultRejectsReceiveBeforeExecution(t *testing.T) {
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "http://ags.test/owner/repo.git/git-receive-pack", strings.NewReader("0000"))
	err := Serve(rr, r, Request{ProjectRoot: t.TempDir(), Repository: "owner/repo", Service: ReceivePack})
	if err == nil || !strings.Contains(err.Error(), "disabled") || rr.Body.Len() != 0 {
		t.Fatalf("write reached backend: err=%v body=%s", err, rr.Body.String())
	}
}
