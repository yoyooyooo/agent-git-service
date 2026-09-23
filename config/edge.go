package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// EdgeConfig is intentionally independent of Config: an Edge must never load
// the primary DB, seed users, migrate schemas, or start integration workers.
// The optional read file assembles the real local reader; performance/live
// rollout gates remain separate from functional read-path availability.
type EdgeConfig struct {
	ID           string
	ListenAddr   string
	PrimaryURL   string
	CanonicalURL string
	// CanonicalAliases are explicit alternative origins for the SAME AGS.
	// They never select an upstream or grant access to another repository.
	CanonicalAliases         []string
	AllowInsecurePrimaryHTTP bool
	RequestTimeout           time.Duration
	// ReadConfigFile explicitly enables the real local read runtime. It is
	// not loaded implicitly from the primary's .env or runtime directory.
	ReadConfigFile string
	// UnboundReads is "reject" (restricted canary) or "primary" (host-wide
	// gateway). It is a static routing decision, never a fallback after denial.
	UnboundReads string
	// DiagnosticsAddr is an optional separate loopback-only operator listener.
	DiagnosticsAddr string
}

// NewEdge loads only AGS_EDGE_* variables. DB_DSN and the primary .env are not
// needed. The Edge executable deliberately does not auto-load .env files.
func NewEdge() (EdgeConfig, error) {
	cfg := EdgeConfig{
		ID:              os.Getenv("AGS_EDGE_ID"),
		ListenAddr:      os.Getenv("AGS_EDGE_LISTEN_ADDR"),
		PrimaryURL:      os.Getenv("AGS_EDGE_PRIMARY_URL"),
		CanonicalURL:    os.Getenv("AGS_EDGE_CANONICAL_URL"),
		ReadConfigFile:  os.Getenv("AGS_EDGE_READ_CONFIG_FILE"),
		UnboundReads:    os.Getenv("AGS_EDGE_UNBOUND_READS"),
		DiagnosticsAddr: os.Getenv("AGS_EDGE_DIAGNOSTICS_ADDR"),
	}
	if value := os.Getenv("AGS_EDGE_CANONICAL_ALIASES"); value != "" {
		cfg.CanonicalAliases = strings.Split(value, ",")
	}
	if value := os.Getenv("AGS_EDGE_ALLOW_INSECURE_PRIMARY_HTTP"); value != "" {
		allowed, err := strconv.ParseBool(value)
		if err != nil {
			return EdgeConfig{}, fmt.Errorf("invalid AGS_EDGE_ALLOW_INSECURE_PRIMARY_HTTP")
		}
		cfg.AllowInsecurePrimaryHTTP = allowed
	}
	if value := os.Getenv("AGS_EDGE_REQUEST_TIMEOUT"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return EdgeConfig{}, fmt.Errorf("invalid AGS_EDGE_REQUEST_TIMEOUT")
		}
		cfg.RequestTimeout = duration
	}
	return NormalizeEdge(cfg)
}

func NormalizeEdge(cfg EdgeConfig) (EdgeConfig, error) {
	if cfg.ID == "" || len(cfg.ID) > 64 || strings.ContainsAny(cfg.ID, "\r\n\t /\\") {
		return EdgeConfig{}, fmt.Errorf("AGS_EDGE_ID must be a non-empty node identifier")
	}
	for _, c := range cfg.ID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return EdgeConfig{}, fmt.Errorf("AGS_EDGE_ID contains an invalid character")
		}
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:6667"
	}
	_, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return EdgeConfig{}, fmt.Errorf("AGS_EDGE_LISTEN_ADDR must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return EdgeConfig{}, fmt.Errorf("invalid AGS_EDGE_LISTEN_ADDR port")
	}
	primary, err := edgeOrigin(cfg.PrimaryURL)
	if err != nil {
		return EdgeConfig{}, fmt.Errorf("AGS_EDGE_PRIMARY_URL must be an HTTP(S) origin without credentials, path, query, or fragment")
	}
	if primary.Scheme == "http" && !cfg.AllowInsecurePrimaryHTTP {
		ip := net.ParseIP(primary.Hostname())
		if primary.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return EdgeConfig{}, fmt.Errorf("non-loopback HTTP primary requires AGS_EDGE_ALLOW_INSECURE_PRIMARY_HTTP=true on a separately secured network")
		}
	}
	cfg.PrimaryURL = strings.TrimSuffix(primary.String(), "/")
	if cfg.CanonicalURL == "" {
		cfg.CanonicalURL = cfg.PrimaryURL
	}
	canonical, err := edgeOrigin(cfg.CanonicalURL)
	if err != nil {
		return EdgeConfig{}, fmt.Errorf("AGS_EDGE_CANONICAL_URL must be an HTTP(S) origin")
	}
	cfg.CanonicalURL = strings.TrimSuffix(canonical.String(), "/")
	if len(cfg.CanonicalAliases) > 16 {
		return EdgeConfig{}, fmt.Errorf("too many AGS_EDGE_CANONICAL_ALIASES")
	}
	seen := map[string]bool{strings.ToLower(canonical.Host): true}
	aliases := make([]string, 0, len(cfg.CanonicalAliases))
	for _, value := range cfg.CanonicalAliases {
		alias, err := edgeOrigin(value)
		if err != nil || alias.Scheme != canonical.Scheme || alias.Port() != canonical.Port() || strings.ContainsAny(alias.Host, "*? ,\\\t\r\n") {
			return EdgeConfig{}, fmt.Errorf("AGS_EDGE_CANONICAL_ALIASES require exact origins with the canonical scheme and port")
		}
		key := strings.ToLower(alias.Host)
		if seen[key] {
			return EdgeConfig{}, fmt.Errorf("duplicate AGS_EDGE_CANONICAL_ALIASES origin")
		}
		seen[key] = true
		aliases = append(aliases, strings.TrimSuffix(alias.String(), "/"))
	}
	cfg.CanonicalAliases = aliases
	if cfg.UnboundReads == "" {
		cfg.UnboundReads = "reject"
	}
	if cfg.UnboundReads != "reject" && cfg.UnboundReads != "primary" {
		return EdgeConfig{}, fmt.Errorf("AGS_EDGE_UNBOUND_READS must be reject or primary")
	}
	if cfg.DiagnosticsAddr != "" {
		host, port, err := net.SplitHostPort(cfg.DiagnosticsAddr)
		ip := net.ParseIP(host)
		n, portErr := strconv.Atoi(port)
		if err != nil || portErr != nil || ip == nil || !ip.IsLoopback() || n < 1 || n > 65535 || cfg.DiagnosticsAddr == cfg.ListenAddr {
			return EdgeConfig{}, fmt.Errorf("AGS_EDGE_DIAGNOSTICS_ADDR requires a separate explicit loopback IP:port")
		}
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 15 * time.Minute
	}
	if cfg.RequestTimeout < 0 {
		return EdgeConfig{}, fmt.Errorf("Edge request timeout must be positive")
	}
	return cfg, nil
}

func edgeOrigin(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.TrimSpace(value) != value {
		return nil, fmt.Errorf("invalid origin")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("invalid origin port")
		}
	}
	return u, nil
}
