package edge

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/snapshotstore"
)

const ReadConfigVersion = "ags.edge.read-config.v1"

// ReadFileConfig is explicit opt-in to the functional local read path. Paths
// are resolved relative to this owner-only config file, never a request. It
// contains no business DB DSN, user token or provider integration credential.
// Published-view budgets and incremental transfers are enabled. Low-pause
// capture, total-filesystem headroom and live rollout remain readiness gates.
type ReadFileConfig struct {
	Version             string        `json:"version"`
	PeerURL             string        `json:"peer_url"`
	CAFile              string        `json:"ca_file"`
	CertificateFile     string        `json:"certificate_file"`
	PrivateKeyFile      string        `json:"private_key_file"`
	CacheRoot           string        `json:"cache_root"`
	Bindings            []ReadBinding `json:"bindings"`
	ReadConcurrency     int           `json:"read_concurrency"`
	SyncConcurrency     int           `json:"sync_concurrency"`
	MaxPending          int           `json:"max_pending"`
	MaxPackBytes        int64         `json:"max_pack_bytes"`
	SyncTimeout         string        `json:"sync_timeout"`
	PeerTimeout         string        `json:"peer_timeout"`
	RecentViews         int           `json:"recent_views"`
	RecentWindow        string        `json:"recent_window"`
	MaxCacheBytes       int64         `json:"max_cache_bytes,omitempty"`
	MaxCacheViews       int           `json:"max_cache_views,omitempty"`
	ProtectRecent       string        `json:"protect_recent,omitempty"`
	ExpireIdle          string        `json:"expire_idle,omitempty"`
	MaintenanceInterval string        `json:"maintenance_interval,omitempty"`
	MinFreeBytes        uint64        `json:"min_free_bytes,omitempty"`
	PrewarmInterval     string        `json:"prewarm_interval,omitempty"`
}

// ReadResources must outlive every request and be closed after the server
// drains. A caller must not start a second owner for the same cache root.
type ReadResources struct {
	Runtime           *ReadRuntime
	mirror            *Mirror
	peer              *PeerClient
	prewarmer         *Prewarmer
	cacheRoot         string
	minimumFree       uint64
	certificateExpiry time.Time
}

func OpenReadResources(ctx context.Context, edgeID, configPath string) (*ReadResources, error) {
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		return nil, errors.New("invalid read config path")
	}
	data, err := readConfigFile(absolute, true, edgeprotocol.MaxControlBytes)
	if err != nil {
		return nil, errors.New("read config must be a bounded owner-only regular file")
	}
	var cfg ReadFileConfig
	if err := edgeprotocol.DecodeControl(bytes.NewReader(data), &cfg); err != nil || cfg.Version != ReadConfigVersion {
		return nil, errors.New("invalid closed read configuration")
	}
	resolve := func(value string) (string, error) {
		if value == "" {
			return "", errors.New("read configuration requires explicit certificate and cache paths")
		}
		if filepath.IsAbs(value) {
			return filepath.Clean(value), nil
		}
		return filepath.Join(filepath.Dir(absolute), value), nil
	}
	caPath, err := resolve(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	certPath, err := resolve(cfg.CertificateFile)
	if err != nil {
		return nil, err
	}
	keyPath, err := resolve(cfg.PrivateKeyFile)
	if err != nil {
		return nil, err
	}
	cacheRoot, err := resolve(cfg.CacheRoot)
	if err != nil {
		return nil, err
	}
	caPEM, err := readConfigFile(caPath, false, 1<<20)
	if err != nil {
		return nil, errors.New("cannot read explicit peer CA")
	}
	certPEM, err := readConfigFile(certPath, false, 1<<20)
	if err != nil {
		return nil, errors.New("cannot read explicit node certificate")
	}
	keyPEM, err := readConfigFile(keyPath, true, 1<<20)
	if err != nil {
		return nil, errors.New("node private key must be an owner-only regular file")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid peer CA")
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, errors.New("invalid node certificate/key pair")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, errors.New("invalid node leaf certificate")
	}
	if cfg.MinFreeBytes == 0 {
		cfg.MinFreeBytes = 1 << 30
	}
	if cfg.MinFreeBytes > 1<<60 {
		return nil, errors.New("invalid filesystem headroom")
	}
	var prewarmInterval time.Duration
	if cfg.PrewarmInterval != "" {
		prewarmInterval, err = time.ParseDuration(cfg.PrewarmInterval)
		if err != nil || prewarmInterval < 10*time.Second || prewarmInterval > time.Hour {
			return nil, errors.New("invalid prewarm interval")
		}
	}
	syncTimeout, err := time.ParseDuration(cfg.SyncTimeout)
	if err != nil {
		return nil, errors.New("invalid sync timeout")
	}
	peerTimeout, err := time.ParseDuration(cfg.PeerTimeout)
	if err != nil {
		return nil, errors.New("invalid peer timeout")
	}
	recentWindow, err := time.ParseDuration(cfg.RecentWindow)
	if err != nil {
		return nil, errors.New("invalid recent view window")
	}
	retention := snapshotstore.DefaultRetentionPolicy()
	if cfg.MaxCacheBytes != 0 {
		retention.MaxBytes = cfg.MaxCacheBytes
	}
	if cfg.MaxCacheViews != 0 {
		retention.MaxViews = cfg.MaxCacheViews
	}
	if cfg.ProtectRecent != "" {
		retention.MinAge, err = time.ParseDuration(cfg.ProtectRecent)
		if err != nil {
			return nil, errors.New("invalid recent snapshot protection window")
		}
	}
	if cfg.ExpireIdle != "" {
		retention.MaxAge, err = time.ParseDuration(cfg.ExpireIdle)
		if err != nil {
			return nil, errors.New("invalid snapshot idle expiration")
		}
	}
	if err := retention.Validate(); err != nil {
		return nil, err
	}
	if retention.MinAge < recentWindow {
		return nil, errors.New("retention must protect the entire Git negotiation window")
	}
	maintenanceInterval := time.Minute
	if cfg.MaintenanceInterval != "" {
		maintenanceInterval, err = time.ParseDuration(cfg.MaintenanceInterval)
		if err != nil || maintenanceInterval <= 0 {
			return nil, errors.New("invalid maintenance interval")
		}
	}
	peer, err := NewPeerClient(PeerClientConfig{URL: cfg.PeerURL, RootCAs: pool, Certificate: cert, Timeout: peerTimeout})
	if err != nil {
		return nil, err
	}
	mirror, err := NewMirror(ctx, MirrorConfig{Root: cacheRoot, MaxPackBytes: cfg.MaxPackBytes, Concurrency: cfg.SyncConcurrency, MaxPending: cfg.MaxPending, SyncTimeout: syncTimeout, Retention: retention, MaintenanceInterval: maintenanceInterval, MinFreeBytes: cfg.MinFreeBytes}, peer)
	if err != nil {
		peer.CloseIdleConnections()
		return nil, err
	}
	rt, err := NewReadRuntime(ReadRuntimeConfig{EdgeID: edgeID, Bindings: cfg.Bindings, Concurrency: cfg.ReadConcurrency, RecentViews: cfg.RecentViews, RecentWindow: recentWindow}, peer, mirror)
	if err != nil {
		_ = mirror.Close()
		peer.CloseIdleConnections()
		return nil, err
	}
	resources := &ReadResources{Runtime: rt, mirror: mirror, peer: peer, cacheRoot: cacheRoot, minimumFree: cfg.MinFreeBytes, certificateExpiry: leaf.NotAfter}
	if prewarmInterval > 0 {
		resources.prewarmer, err = StartPrewarmer(ctx, peer, mirror, cfg.Bindings, prewarmInterval, syncTimeout)
		if err != nil {
			_ = resources.Close()
			return nil, err
		}
	}
	return resources, nil
}

func (r *ReadResources) Close() error {
	if r.prewarmer != nil {
		r.prewarmer.Close()
	}
	err := r.mirror.Close()
	r.peer.CloseIdleConnections()
	return err
}

func readConfigFile(path string, private bool, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit || private && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("invalid configuration file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("configuration file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("configuration file exceeds limit")
	}
	return data, nil
}
