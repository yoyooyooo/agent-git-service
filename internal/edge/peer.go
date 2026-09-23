package edge

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

// SnapshotSource is a node-authenticated replication source. Its output is
// still untrusted data until manifest/object verification. It says nothing
// about whether a particular end user may read the downloaded repository.
type SnapshotSource interface {
	OpenSnapshot(context.Context, edgeprotocol.RepositorySnapshot) (edgeprotocol.Manifest, io.ReadCloser, error)
}

type PeerClientConfig struct {
	URL         string
	RootCAs     *x509.CertPool
	Certificate tls.Certificate
	Timeout     time.Duration
}

type PeerClient struct {
	endpoint string
	client   *http.Client
}

// NewPeerClient requires explicit mTLS material and a fixed HTTPS authority.
// No environment proxy, redirect, stored user credential or arbitrary URL is
// used. Control RPCs take original Authorization per request; export never does.
func NewPeerClient(cfg PeerClientConfig) (*PeerClient, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("replication requires a fixed HTTPS origin")
	}
	if cfg.RootCAs == nil || len(cfg.Certificate.Certificate) == 0 || cfg.Certificate.PrivateKey == nil || cfg.Timeout <= 0 || cfg.Timeout > time.Hour {
		return nil, errors.New("replication requires explicit trust, client certificate and bounded timeout")
	}
	u.Path = edgeprotocol.ExportPath
	transport := &http.Transport{Proxy: nil, DisableCompression: true,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: cfg.Timeout,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: cfg.RootCAs.Clone(), Certificates: []tls.Certificate{cfg.Certificate}},
	}
	return &PeerClient{endpoint: u.String(), client: &http.Client{Transport: transport, Timeout: cfg.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *PeerClient) OpenSnapshot(ctx context.Context, required edgeprotocol.RepositorySnapshot) (edgeprotocol.Manifest, io.ReadCloser, error) {
	body, err := edgeprotocol.EncodeSnapshot(required)
	if err != nil {
		return edgeprotocol.Manifest{}, nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return edgeprotocol.Manifest{}, nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(r)
	if err != nil {
		return edgeprotocol.Manifest{}, nil, errors.New("snapshot peer transport unavailable")
	}
	fail := func(err error) (edgeprotocol.Manifest, io.ReadCloser, error) {
		_ = resp.Body.Close()
		return edgeprotocol.Manifest{}, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("snapshot peer rejected export (HTTP %d)", resp.StatusCode))
	}
	if resp.Header.Get("Content-Type") != edgeprotocol.ExportContentType || resp.Header.Get("Content-Encoding") != "" {
		return fail(errors.New("unexpected snapshot export representation"))
	}
	manifest, err := edgeprotocol.ReadExportHeader(resp.Body)
	if err != nil {
		return fail(err)
	}
	if manifest.Snapshot != required {
		return fail(errors.New("peer supplied a different snapshot"))
	}
	return manifest, resp.Body, nil
}

func (c *PeerClient) CloseIdleConnections() { c.client.CloseIdleConnections() }
