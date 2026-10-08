package api

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cli/cli/v2/pkg/httpmock"
	"github.com/cli/cli/v2/pkg/iostreams"
	ghauth "github.com/cli/go-gh/v2/pkg/auth"
	"github.com/stretchr/testify/assert"
)

func newTestClient(reg *httpmock.Registry) *Client {
	client := &http.Client{}
	httpmock.ReplaceTripper(client, reg)
	return NewClientFromHTTP(client)
}

func TestGraphQL(t *testing.T) {
	http := &httpmock.Registry{}
	client := newTestClient(http)

	vars := map[string]interface{}{"name": "Mona"}
	response := struct {
		Viewer struct {
			Login string
		}
	}{}

	http.Register(
		httpmock.GraphQL("QUERY"),
		httpmock.StringResponse(`{"data":{"viewer":{"login":"hubot"}}}`),
	)

	err := client.GraphQL("github.com", "QUERY", vars, &response)
	assert.NoError(t, err)
	assert.Equal(t, "hubot", response.Viewer.Login)

	req := http.Requests[0]
	reqBody, _ := io.ReadAll(req.Body)
	assert.Equal(t, `{"query":"QUERY","variables":{"name":"Mona"}}`, string(reqBody))
}

func TestGraphQLError(t *testing.T) {
	reg := &httpmock.Registry{}
	client := newTestClient(reg)

	response := struct{}{}

	reg.Register(
		httpmock.GraphQL(""),
		httpmock.StringResponse(`
			{ "errors": [
				{
					"type": "NOT_FOUND",
					"message": "OH NO",
					"path": ["repository", "issue"]
				},
				{
					"type": "ACTUALLY_ITS_FINE",
					"message": "this is fine",
					"path": ["repository", "issues", 0, "comments"]
				}
			  ]
			}
		`),
	)

	err := client.GraphQL("github.com", "", nil, &response)
	if err == nil || err.Error() != "GraphQL: OH NO (repository.issue), this is fine (repository.issues.0.comments)" {
		t.Fatalf("got %q", err.Error())
	}
}

func TestRESTGetDelete(t *testing.T) {
	http := &httpmock.Registry{}
	client := newTestClient(http)

	http.Register(
		httpmock.REST("DELETE", "applications/CLIENTID/grant"),
		httpmock.StatusStringResponse(204, "{}"),
	)

	r := bytes.NewReader([]byte(`{}`))
	err := client.REST("github.com", "DELETE", "applications/CLIENTID/grant", r, nil)
	assert.NoError(t, err)
}

func TestRESTWithFullURL(t *testing.T) {
	http := &httpmock.Registry{}
	client := newTestClient(http)

	http.Register(
		httpmock.REST("GET", "api/v3/user/repos"),
		httpmock.StatusStringResponse(200, "{}"))
	http.Register(
		httpmock.REST("GET", "user/repos"),
		httpmock.StatusStringResponse(200, "{}"))

	err := client.REST("example.com", "GET", "user/repos", nil, nil)
	assert.NoError(t, err)
	err = client.REST("example.com", "GET", "https://another.net/user/repos", nil, nil)
	assert.NoError(t, err)

	assert.Equal(t, "example.com", http.Requests[0].URL.Hostname())
	assert.Equal(t, "another.net", http.Requests[1].URL.Hostname())
}

func TestRESTError(t *testing.T) {
	fakehttp := &httpmock.Registry{}
	client := newTestClient(fakehttp)

	fakehttp.Register(httpmock.MatchAny, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			Request:    req,
			StatusCode: 422,
			Body:       io.NopCloser(bytes.NewBufferString(`{"message": "OH NO"}`)),
			Header: map[string][]string{
				"Content-Type": {"application/json; charset=utf-8"},
			},
		}, nil
	})

	var httpErr HTTPError
	err := client.REST("github.com", "DELETE", "repos/branch", nil, nil)
	if err == nil || !errors.As(err, &httpErr) {
		t.Fatalf("got %v", err)
	}

	if httpErr.StatusCode != 422 {
		t.Errorf("expected status code 422, got %d", httpErr.StatusCode)
	}
	if httpErr.Error() != "HTTP 422: OH NO (https://api.github.com/repos/branch)" {
		t.Errorf("got %q", httpErr.Error())
	}
}

func TestHandleHTTPError_GraphQL502(t *testing.T) {
	req, err := http.NewRequest("GET", "https://api.github.com/user", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{
		Request:    req,
		StatusCode: 502,
		Body:       io.NopCloser(bytes.NewBufferString(`{ "data": null, "errors": [{ "message": "Something went wrong" }] }`)),
		Header:     map[string][]string{"Content-Type": {"application/json"}},
	}
	err = HandleHTTPError(resp)
	if err == nil || err.Error() != "HTTP 502: Something went wrong (https://api.github.com/user)" {
		t.Errorf("got error: %v", err)
	}
}

func TestHTTPError_ScopesSuggestion(t *testing.T) {
	makeResponse := func(s int, u, haveScopes, needScopes string) *http.Response {
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			Request:    req,
			StatusCode: s,
			Body:       io.NopCloser(bytes.NewBufferString(`{}`)),
			Header: map[string][]string{
				"Content-Type":            {"application/json"},
				"X-Oauth-Scopes":          {haveScopes},
				"X-Accepted-Oauth-Scopes": {needScopes},
			},
		}
	}

	tests := []struct {
		name string
		resp *http.Response
		want string
	}{
		{
			name: "has necessary scopes",
			resp: makeResponse(404, "https://api.github.com/gists", "repo, gist, read:org", "gist"),
			want: ``,
		},
		{
			name: "normalizes scopes",
			resp: makeResponse(404, "https://api.github.com/orgs/ORG/discussions", "admin:org, write:discussion", "read:org, read:discussion"),
			want: ``,
		},
		{
			name: "no scopes on endpoint",
			resp: makeResponse(404, "https://api.github.com/user", "repo", ""),
			want: ``,
		},
		{
			name: "missing a scope",
			resp: makeResponse(404, "https://api.github.com/gists", "repo, read:org", "gist, delete_repo"),
			want: `This API operation needs the "gist" scope. To request it, run:  gh auth refresh -h github.com -s gist`,
		},
		{
			name: "server error",
			resp: makeResponse(500, "https://api.github.com/gists", "repo", "gist"),
			want: ``,
		},
		{
			name: "no scopes on token",
			resp: makeResponse(404, "https://api.github.com/gists", "", "gist, delete_repo"),
			want: ``,
		},
		{
			name: "http code is 422",
			resp: makeResponse(422, "https://api.github.com/gists", "", "gist"),
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			httpError := HandleHTTPError(tt.resp)
			if got := httpError.(HTTPError).ScopesSuggestion(); got != tt.want {
				t.Errorf("HTTPError.ScopesSuggestion() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHTTPHeaders(t *testing.T) {
	var gotReq *http.Request
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	ios, _, _, stderr := iostreams.Test()
	httpClient, err := NewHTTPClient(HTTPClientOptions{
		AppVersion: "v1.2.3",
		Config:     tinyConfig{ts.URL[7:] + ":oauth_token": "MYTOKEN"},
		Log:        ios.ErrOut,
	})
	assert.NoError(t, err)
	client := NewClientFromHTTP(httpClient)

	err = client.REST(ts.URL, "GET", ts.URL+"/user/repos", nil, nil)
	assert.NoError(t, err)

	wantHeader := map[string]string{
		"Accept":               "application/vnd.github.merge-info-preview+json, application/vnd.github.nebula-preview",
		"Authorization":        "token MYTOKEN",
		"Content-Type":         "application/json; charset=utf-8",
		"User-Agent":           "GitHub CLI v1.2.3",
		"X-GitHub-Api-Version": "2022-11-28",
	}
	for name, value := range wantHeader {
		assert.Equal(t, value, gotReq.Header.Get(name), name)
	}
	assert.Equal(t, "", stderr.String())
}

func TestGraphQLRewritesExplicitAGSURL(t *testing.T) {
	for _, hostname := range []string{"localhost", "localhost:6666"} {
		t.Run(hostname, func(t *testing.T) {
			t.Setenv("AGS_URL", "http://localhost:6666")
			var gotURL string
			client := NewClientFromHTTP(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				gotURL = req.URL.String()
				return &http.Response{
					StatusCode: 200,
					Header:     make(http.Header),
					Body:       io.NopCloser(bytes.NewBufferString(`{"data":{"viewer":{"login":"ags"}}}`)),
					Request:    req,
				}, nil
			})})
			var response struct {
				Viewer struct {
					Login string
				}
			}
			if err := client.GraphQL(hostname, "QUERY", nil, &response); err != nil {
				t.Fatalf("GraphQL: %v", err)
			}
			if gotURL != "http://localhost:6666/api/graphql" {
				t.Fatalf("GraphQL URL = %q, want AGS URL", gotURL)
			}
		})
	}
}

func TestGraphQLHTTPProxyKeepsNonDefaultPortOnHost(t *testing.T) {
	original := proxyForRequest
	proxyForRequest = func(req *http.Request) (*url.URL, error) {
		return url.Parse("http://127.0.0.1:7890")
	}
	t.Cleanup(func() { proxyForRequest = original })

	t.Setenv("AGS_URL", "http://mini:6666")
	var gotHost string
	var gotURL string
	client := NewClientFromHTTP(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotHost = req.Host
		gotURL = req.URL.String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewBufferString(`{"data":{"viewer":{"login":"ags"}}}`)),
			Request:    req,
		}, nil
	})})
	var response struct {
		Viewer struct {
			Login string
		}
	}
	if err := client.GraphQL("mini", "query { viewer { login } }", nil, &response); err != nil {
		t.Fatalf("GraphQL: %v", err)
	}
	if gotHost != "mini:6666" {
		t.Fatalf("Host = %q, want mini:6666 so an HTTP proxy does not dial port 80 (url %s)", gotHost, gotURL)
	}
	if !strings.Contains(gotURL, "http://mini:6666/api/graphql") {
		t.Fatalf("URL = %q, want http://mini:6666/api/graphql", gotURL)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// recordingStoredTokenConfig resolves tokens through the production
// config lookup and records the host key it was given.
type recordingStoredTokenConfig struct{ seen *string }

func (c recordingStoredTokenConfig) ActiveToken(host string) (string, string) {
	*c.seen = host
	return ghauth.TokenFromEnvOrConfig(host)
}

// TestGraphQLHTTPSProxyAuthenticatesStoredBareHost is the auth regression
// for a non-default AGS port behind an HTTP proxy: credentials saved for
// the bare hostname must still be found, and the proxy must still be
// aimed at host:port.
//
// The lookup uses go-gh's process-wide config cache, so the body runs in
// a child process whose GH_CONFIG_DIR is set before that cache is filled.
func TestGraphQLHTTPSProxyAuthenticatesStoredBareHost(t *testing.T) {
	if os.Getenv("AGS_STORED_AUTH_CHILD") != "1" {
		cfgDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(cfgDir, "hosts.yml"), []byte("review.example:\n    oauth_token: SYNTHETIC_REVIEW_ONLY\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestGraphQLHTTPSProxyAuthenticatesStoredBareHost$", "-test.v", "-test.count=1")
		cmd.Env = storedCredentialProxyEnv(cfgDir)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "stored bare-host credential authenticated") {
			t.Fatalf("stored-credential child failed: %v\n%s", err, out)
		}
		return
	}

	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		os.Unsetenv(key)
	}
	token, source := ghauth.TokenFromEnvOrConfig("review.example")
	if token != "SYNTHETIC_REVIEW_ONLY" || source != "oauth_token" {
		t.Fatal("synthetic hosts.yml was not the credential source")
	}

	originHost := make(chan string, 1)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHost <- r.Host
		if r.Header.Get("Authorization") != "token SYNTHETIC_REVIEW_ONLY" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"message":"Bad credentials"}`)
			return
		}
		io.WriteString(w, `{"data":{"viewer":{"login":"review"}}}`)
	}))
	t.Cleanup(origin.Close)
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	seenConnect := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "expected CONNECT", http.StatusBadRequest)
			return
		}
		seenConnect <- r.RequestURI
		dst, dialErr := net.Dial("tcp", originURL.Host)
		if dialErr != nil {
			http.Error(w, dialErr.Error(), http.StatusBadGateway)
			return
		}
		src, _, hijackErr := w.(http.Hijacker).Hijack()
		if hijackErr != nil {
			dst.Close()
			return
		}
		io.WriteString(src, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() {
			io.Copy(dst, src)
			dst.Close()
		}()
		io.Copy(src, dst)
		src.Close()
	}))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	originalProxy := proxyForRequest
	proxyForRequest = http.ProxyURL(proxyURL)
	t.Cleanup(func() { proxyForRequest = originalProxy })

	// Loopback test server only; the process does not use a real credential.
	transport := &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	t.Cleanup(transport.CloseIdleConnections)

	var lookup string
	client := NewClientFromHTTP(&http.Client{Transport: AddAuthTokenHeader(transport, recordingStoredTokenConfig{seen: &lookup})})
	t.Setenv("AGS_URL", "https://review.example:8443")
	var data struct {
		Viewer struct {
			Login string
		}
	}
	err = client.GraphQL("review.example", "query { viewer { login } }", nil, &data)
	connect := ""
	select {
	case connect = <-seenConnect:
	default:
	}
	wireHost := ""
	select {
	case wireHost = <-originHost:
	default:
	}
	if connect != "review.example:8443" {
		t.Fatalf("CONNECT=%q, want review.example:8443", connect)
	}
	if wireHost != "review.example:8443" {
		t.Fatalf("origin Host=%q, want review.example:8443", wireHost)
	}
	if err != nil {
		t.Fatalf("stored credential lookup=%q; GraphQL failed: %v", lookup, err)
	}
	if lookup != "review.example" || data.Viewer.Login != "review" {
		t.Fatalf("lookup=%q login=%q, want bare host review.example", lookup, data.Viewer.Login)
	}
	t.Log("stored bare-host credential authenticated")
}

func storedCredentialProxyEnv(cfgDir string) []string {
	env := []string{
		"AGS_STORED_AUTH_CHILD=1",
		"GH_CONFIG_DIR=" + cfgDir,
	}
	for _, key := range []string{"PATH", "TMPDIR", "TMP", "TEMP", "HOME", "LANG", "LC_ALL", "TZ", "SYSTEMROOT", "WINDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}
