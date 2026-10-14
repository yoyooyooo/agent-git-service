package githubappmanifest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const StartPath = "/api/ext/v1/integrations/github/app-manifest/start"
const StatusPath = "/api/ext/v1/integrations/github/app-manifest/app"

// Registrar composes the file-backed one-time flow with the browser manifest
// and a fresh App-authenticated API probe. It does not enable projections.
type Registrar struct {
	Flow        *Flow
	RedirectURL string
	APIBaseURL  string
	Client      *http.Client
}

func NewRegistrar(flow *Flow, redirect, api string, client *http.Client) (*Registrar, error) {
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".ts.net") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != CallbackPath {
		return nil, errors.New("registration requires an HTTPS tailnet callback")
	}
	if flow == nil || flow.StatePath == "" || flow.Paths.AppID == "" || flow.Paths.PrivateKey == "" || flow.Paths.WebhookSecret == "" {
		return nil, errors.New("registration requires all owner-only file paths")
	}
	seen := map[string]bool{}
	for _, path := range []string{flow.StatePath, flow.Paths.AppID, flow.Paths.PrivateKey, flow.Paths.WebhookSecret} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || seen[path] {
			return nil, errors.New("registration paths must be distinct absolute paths")
		}
		seen[path] = true
		info, err := os.Lstat(filepath.Dir(path))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			return nil, errors.New("registration requires private existing parent directories")
		}
		var stat unix.Stat_t
		if unix.Lstat(filepath.Dir(path), &stat) != nil || stat.Uid != uint32(os.Getuid()) {
			return nil, errors.New("registration directory owner mismatch")
		}
	}
	if api == "" {
		api = "https://api.github.com"
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Registrar{Flow: flow, RedirectURL: redirect, APIBaseURL: strings.TrimRight(api, "/"), Client: client}, nil
}

// Begin returns a form contract. Only the authenticated site administrator may
// invoke it through the owning transport. The browser posts this manifest to GitHub.
func (r *Registrar) Begin(name string) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if len(name) < 3 || len(name) > 100 || strings.ContainsAny(name, "\r\n") {
		return nil, errors.New("invalid app name")
	}
	state, err := r.Flow.Begin(r.RedirectURL)
	if err != nil {
		return nil, err
	}
	return map[string]any{"registration_url": "https://github.com/settings/apps/new?state=" + url.QueryEscape(state.Value),
		"manifest": map[string]any{"name": name, "url": strings.TrimSuffix(r.RedirectURL, CallbackPath), "redirect_url": r.RedirectURL,
			"public": false, "hook_attributes": map[string]any{"active": false, "url": strings.TrimSuffix(r.RedirectURL, CallbackPath) + "/api/ext/v1/integrations/github/events"},
			"default_permissions": map[string]string{"metadata": "read", "contents": "write", "pull_requests": "write", "issues": "write", "checks": "write", "statuses": "write"},
			"default_events":      []string{}}, "expires_at": state.ExpiresAt}, nil
}

func (r *Registrar) Callback(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	q := req.URL.Query()
	if len(q["code"]) != 1 || len(q["state"]) != 1 || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`).MatchString(q.Get("code")) || len(q.Get("state")) > 128 {
		http.Error(w, "invalid registration callback", http.StatusBadRequest)
		return
	}
	registration, err := r.Flow.Complete(q.Get("code"), q.Get("state"))
	if err != nil {
		http.Error(w, "registration was not completed; inspect private state before retrying", http.StatusBadRequest)
		return
	}
	// Successful conversion is durable even if the provider observation fails.
	// Never invite a second exchange after credentials were committed.
	app, err := r.App(req)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "credentials_saved_app_probe_pending", "app_id": registration.AppID})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "registered", "app": app})
}

// App reads the current credential files and probes GET /app using a short-lived
// RS256 JWT. Responses include only identity fields, never credentials or tokens.
func (r *Registrar) App(req *http.Request) (map[string]any, error) {
	id, err := readPrivate(r.Flow.Paths.AppID)
	if err != nil {
		return nil, err
	}
	appID, err := strconv.ParseInt(strings.TrimSpace(string(id)), 10, 64)
	if err != nil || appID <= 0 {
		return nil, errors.New("invalid app id")
	}
	key, err := readPrivate(r.Flow.Paths.PrivateKey)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(key)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("invalid private key")
	}
	private, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, e := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e != nil {
			return nil, errors.New("invalid private key")
		}
		var ok bool
		private, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("GitHub App JWT requires RSA")
		}
	}
	now := time.Now().Unix()
	claims, _ := json.Marshal(map[string]any{"iat": now - 60, "exp": now + 540, "iss": strconv.FormatInt(appID, 10)})
	enc := base64.RawURLEncoding.EncodeToString
	unsigned := enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, private, crypto.SHA256, digest[:])
	if err != nil {
		return nil, errors.New("app JWT signing failed")
	}
	outbound, err := http.NewRequestWithContext(req.Context(), http.MethodGet, r.APIBaseURL+"/app", nil)
	if err != nil {
		return nil, errors.New("invalid app API URL")
	}
	outbound.Header.Set("Authorization", "Bearer "+unsigned+"."+enc(signature))
	outbound.Header.Set("Accept", "application/vnd.github+json")
	outbound.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := r.Client.Do(outbound)
	if err != nil {
		return nil, errors.New("app API unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("app API status %d", response.StatusCode)
	}
	var body struct {
		ID      int64  `json:"id"`
		Slug    string `json:"slug"`
		HTMLURL string `json:"html_url"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body) != nil || body.ID != appID {
		return nil, errors.New("app identity mismatch")
	}
	return map[string]any{"id": body.ID, "slug": body.Slug, "html_url": body.HTMLURL, "installation_url": "https://github.com/apps/" + url.PathEscape(body.Slug) + "/installations/new"}, nil
}

func readPrivate(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("credential file unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("credential file must be mode 0600 regular file")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("credential file unavailable")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Uid != uint32(os.Getuid()) || stat.Mode&0777 != 0600 || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("credential file owner or mode mismatch")
	}
	return io.ReadAll(io.LimitReader(f, 1<<20))
}
