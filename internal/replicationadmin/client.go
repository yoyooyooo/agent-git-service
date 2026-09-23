// Package replicationadmin is an operator client, not a primary/Edge runtime.
// It never opens a business database, performs local Git mutations, starts a
// listener, changes network configuration, or automatically retries a write.
package replicationadmin

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

type Client struct {
	origin string
	http   *http.Client
}

type ClientConfig struct {
	PrimaryURL       string
	AllowPrivateHTTP bool
	RootCAs          *x509.CertPool
	Timeout          time.Duration
}

func NewClient(cfg ClientConfig) (*Client, error) {
	u, err := url.Parse(cfg.PrimaryURL)
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || strings.TrimSpace(cfg.PrimaryURL) != cfg.PrimaryURL {
		return nil, errors.New("primary must be one HTTP(S) origin without credentials or a path")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && !cfg.AllowPrivateHTTP && u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("non-loopback HTTP requires explicit secured-network opt-in")
	}
	if cfg.Timeout <= 0 || cfg.Timeout > 2*time.Minute {
		return nil, errors.New("operator requests need a bounded timeout")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.RootCAs != nil {
		tlsConfig.RootCAs = cfg.RootCAs.Clone()
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true, ForceAttemptHTTP2: false,
		DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, TLSClientConfig: tlsConfig,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: cfg.Timeout}
	return &Client{origin: strings.TrimSuffix(u.String(), "/"), http: &http.Client{Transport: transport, Timeout: cfg.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) Status(ctx context.Context, repository, token string) (edgeprotocol.Registration, error) {
	return c.request(ctx, http.MethodGet, repository, token, nil)
}

// Register sends exactly one explicit mutation, bound to a status receipt.
// On an uncertain result the operator must GET status; never blindly repeat a
// POST or replace the expected receipt with a newly discovered same-name row.
func (c *Client) Register(ctx context.Context, expected edgeprotocol.Registration, token string) (edgeprotocol.Registration, error) {
	if expected.Validate() != nil {
		return edgeprotocol.Registration{}, errors.New("invalid expected registration receipt")
	}
	request := expected.Request()
	got, err := c.request(ctx, http.MethodPost, expected.Repository, token, &request)
	if err != nil {
		return edgeprotocol.Registration{}, err
	}
	if got.AuthorityID != expected.AuthorityID || got.RepositoryID != expected.RepositoryID || !got.CreatedAt.Equal(expected.CreatedAt) || got.Identity == nil || expected.Identity != nil && *expected.Identity != *got.Identity {
		return edgeprotocol.Registration{}, errors.New("primary returned a different registration; inspect before proceeding")
	}
	return got, nil
}

func (c *Client) request(ctx context.Context, method, repository, token string, payload *edgeprotocol.RegisterRepository) (edgeprotocol.Registration, error) {
	if !edgeprotocol.ValidRepositoryLocator(repository) || token == "" || len(token) > 16<<10 || strings.ContainsAny(token, "\r\n\x00 \t") {
		return edgeprotocol.Registration{}, errors.New("valid repository and original administrator credential required")
	}
	parts := strings.Split(repository, "/")
	target := c.origin + "/api/v3/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/replication/identity"
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	r, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return edgeprotocol.Registration{}, errors.New("invalid registration request")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(r)
	if err != nil {
		if payload != nil {
			return edgeprotocol.Registration{}, errors.New("registration result unknown; inspect status without automatic POST retry")
		}
		return edgeprotocol.Registration{}, errors.New("registration status unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return edgeprotocol.Registration{}, fmt.Errorf("primary rejected registration request (HTTP %d)", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") || resp.Header.Get("Content-Encoding") != "" {
		return edgeprotocol.Registration{}, errors.New("invalid registration response representation")
	}
	var result edgeprotocol.Registration
	if edgeprotocol.DecodeControl(resp.Body, &result) != nil || result.Validate() != nil {
		return edgeprotocol.Registration{}, errors.New("invalid registration response")
	}
	return result, nil
}
