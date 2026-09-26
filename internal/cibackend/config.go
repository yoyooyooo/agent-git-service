package cibackend

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Config struct {
	Default      string                   `yaml:"default_backend" json:"default_backend"`
	Backends     map[string]BackendConfig `yaml:"backends" json:"backends"`
	Repositories map[string]Binding       `yaml:"repositories" json:"repositories"`
}
type LogBridgeConfig struct {
	URL       string `yaml:"url" json:"url"`
	TokenFile string `yaml:"token_file" json:"-"`
	AllowHTTP bool   `yaml:"allow_http" json:"allow_http"`
}
type BackendConfig struct {
	Kind             string           `yaml:"kind" json:"kind"`
	URL              string           `yaml:"url" json:"url"`
	TokenFile        string           `yaml:"token_file" json:"-"`
	AllowHTTP        bool             `yaml:"allow_http" json:"allow_http"`
	LogDownloadHosts []string         `yaml:"log_download_hosts" json:"log_download_hosts,omitempty"`
	LogBridge        *LogBridgeConfig `yaml:"log_bridge" json:"log_bridge,omitempty"`
}
type Binding struct {
	Backend    string `yaml:"backend" json:"backend"`
	Repository string `yaml:"repository" json:"repository"`
	// nil is unknown; explicit [] is a known empty policy. This is CI policy,
	// never an override of repository review/protection or merge authority.
	RequiredChecks *[]string `yaml:"required_checks" json:"required_checks"`
}
type Selection struct {
	Backend   Backend
	Kind      string
	Origin    string
	Name      string
	Namespace string
	Binding   Binding
}
type Registry struct {
	config     Config
	backends   map[string]Backend
	namespaces map[string]string
}

var slugPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var namePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

func ValidRepository(s string) bool {
	if !slugPattern.MatchString(s) {
		return false
	}
	for _, p := range strings.Split(s, "/") {
		if p == "." || p == ".." {
			return false
		}
	}
	return true
}
func Validate(c Config) error {
	if c.Default == "" {
		c.Default = "native"
	}
	if c.Default != "native" && c.Default != "none" {
		if _, ok := c.Backends[c.Default]; !ok {
			return fmt.Errorf("ci.default_backend names an unknown backend")
		}
	}
	for name, b := range c.Backends {
		if !namePattern.MatchString(name) || name == "native" || name == "none" {
			return fmt.Errorf("invalid CI backend name")
		}
		if b.Kind != "forgejo" && b.Kind != "github-actions" {
			return fmt.Errorf("CI backend %s has unsupported kind", name)
		}
		u, err := url.Parse(b.URL)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && b.AllowHTTP)) {
			return fmt.Errorf("CI backend %s needs an explicit trusted HTTP(S) API origin", name)
		}
		if u.RawPath != "" || strings.Contains(u.Path, "..") {
			return fmt.Errorf("invalid CI API path")
		}
		if strings.TrimSpace(b.TokenFile) == "" {
			return fmt.Errorf("CI backend %s requires a server token file", name)
		}
		if b.LogBridge != nil {
			v, e := url.Parse(b.LogBridge.URL)
			if b.Kind != "forgejo" || e != nil || v.Hostname() == "" || v.User != nil || v.RawQuery != "" || v.Fragment != "" || strings.TrimSpace(b.LogBridge.TokenFile) == "" || (v.Scheme != "https" && !(v.Scheme == "http" && b.LogBridge.AllowHTTP)) {
				return fmt.Errorf("invalid explicit Forgejo log bridge")
			}
		}
		for _, h := range b.LogDownloadHosts {
			if h == "" || strings.ContainsAny(h, "/:?#@ *") {
				return fmt.Errorf("CI log download hosts must be exact hostnames")
			}
		}
	}
	for repo, b := range c.Repositories {
		if !ValidRepository(repo) {
			return fmt.Errorf("invalid AGS CI repository")
		}
		name := b.Backend
		if name == "" {
			name = c.Default
		}
		if name != "native" && name != "none" {
			if _, ok := c.Backends[name]; !ok {
				return fmt.Errorf("CI repository %s references unknown backend", repo)
			}
			if !ValidRepository(b.Repository) {
				return fmt.Errorf("CI repository %s requires an exact external repository", repo)
			}
		}
		if b.RequiredChecks != nil {
			seen := map[string]bool{}
			if len(*b.RequiredChecks) > 100 {
				return fmt.Errorf("too many required CI checks")
			}
			for _, check := range *b.RequiredChecks {
				if strings.TrimSpace(check) != check || check == "" || len(check) > 128 || strings.ContainsAny(check, "\r\n\x00") || seen[check] {
					return fmt.Errorf("invalid or duplicate required CI check")
				}
				seen[check] = true
			}
		}
	}
	return nil
}
func New(c Config, baseDirectory string) (*Registry, error) {
	if err := Validate(c); err != nil {
		return nil, err
	}
	if c.Default == "" {
		c.Default = "native"
	}
	r := &Registry{config: c, backends: map[string]Backend{}, namespaces: map[string]string{}}
	for name, options := range c.Backends {
		path := options.TokenFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(baseDirectory, path)
		}
		st, err := os.Lstat(path)
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 65536 {
			return nil, fmt.Errorf("CI backend %s token file must be an owner-only regular file", name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("CI backend %s token file unavailable", name)
		}
		token := strings.TrimSpace(string(data))
		if token == "" || strings.ContainsAny(token, "\r\n\x00") {
			return nil, fmt.Errorf("CI backend %s token invalid", name)
		}
		bridgeToken := ""
		if options.LogBridge != nil {
			file := options.LogBridge.TokenFile
			if !filepath.IsAbs(file) {
				file = filepath.Join(baseDirectory, file)
			}
			st, e := os.Lstat(file)
			if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 65536 {
				return nil, fmt.Errorf("CI log bridge token file must be an owner-only regular file")
			}
			data, e := os.ReadFile(file)
			if e != nil {
				return nil, fmt.Errorf("CI log bridge token file unavailable")
			}
			bridgeToken = strings.TrimSpace(string(data))
			if bridgeToken == "" || strings.ContainsAny(bridgeToken, "\r\n\x00") {
				return nil, fmt.Errorf("CI log bridge token invalid")
			}
		}
		b, err := NewHTTP(options, token, bridgeToken)
		if err != nil {
			return nil, fmt.Errorf("CI backend %s: %w", name, err)
		}
		r.backends[name] = b
		// Credentials/required policy can rotate without changing resource identity.
		identity, _ := json.Marshal([]string{name, options.Kind, strings.TrimRight(options.URL, "/")})
		sum := sha256.Sum256(identity)
		r.namespaces[name] = hex.EncodeToString(sum[:])
	}
	return r, nil
}

// WithBackends supports embedded hosts and boundary tests without file/token I/O.
// Production composition uses New and validates all config before service startup.
func WithBackends(c Config, backends map[string]Backend) (*Registry, error) {
	if err := Validate(c); err != nil {
		return nil, err
	}
	if c.Default == "" {
		c.Default = "native"
	}
	r := &Registry{config: c, backends: backends, namespaces: map[string]string{}}
	for name, b := range c.Backends {
		if backends[name] == nil {
			return nil, fmt.Errorf("missing CI implementation")
		}
		v, _ := json.Marshal([]string{name, b.Kind, strings.TrimRight(b.URL, "/")})
		s := sha256.Sum256(v)
		r.namespaces[name] = hex.EncodeToString(s[:])
	}
	return r, nil
}
func (r *Registry) Select(repository string) (Selection, error) {
	if r == nil {
		return Selection{Name: "native", Binding: Binding{Backend: "native"}}, nil
	}
	binding, exists := r.config.Repositories[repository]
	name := binding.Backend
	if name == "" {
		name = r.config.Default
	}
	binding.Backend = name
	if name == "native" || name == "none" {
		return Selection{Name: name, Binding: binding}, nil
	}
	if !exists || binding.Repository == "" {
		return Selection{}, fmt.Errorf("%w: repository has no explicit CI backend binding", ErrUnavailable)
	}
	return Selection{Backend: r.backends[name], Kind: r.config.Backends[name].Kind, Origin: r.config.Backends[name].URL, Name: name, Namespace: r.namespaces[name], Binding: binding}, nil
}
