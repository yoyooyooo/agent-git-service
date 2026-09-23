package server

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

const ReplicationConfigVersion = "ags.primary.replication-config.v1"

// ReplicationFileConfig is explicit operator opt-in. Only exact provisioned
// identities are accepted. Paths are relative to this owner-only file.
type ReplicationFileConfig struct {
	Version              string                `json:"version"`
	AuthorityID          string                `json:"authority_id"`
	ListenAddr           string                `json:"listen_addr"`
	ServerName           string                `json:"server_name"`
	CAFile               string                `json:"ca_file"`
	CertificateFile      string                `json:"certificate_file"`
	PrivateKeyFile       string                `json:"private_key_file"`
	SnapshotRoot         string                `json:"snapshot_root"`
	ManagedWritesOnly    bool                  `json:"managed_writes_only"`
	MinFreeBytes         uint64                `json:"min_free_bytes"`
	ExportPolicyRevision string                `json:"export_policy_revision"`
	ExportPrefixes       []string              `json:"export_prefixes"`
	Peers                []ReplicationFilePeer `json:"peers"`
	MaxPackBytes         int64                 `json:"max_pack_bytes"`
	PlanTTL              string                `json:"plan_ttl"`
	RequestTimeout       string                `json:"request_timeout"`
	TransferTimeout      string                `json:"transfer_timeout,omitempty"`
	Concurrency          int                   `json:"concurrency"`
	MaxSnapshotBytes     int64                 `json:"max_snapshot_bytes"`
	MaxSnapshotViews     int                   `json:"max_snapshot_views"`
	ProtectRecent        string                `json:"protect_recent"`
	ExpireIdle           string                `json:"expire_idle"`
}

type ReplicationFilePeer struct {
	EdgeID     string                `json:"edge_id"`
	SPKISHA256 string                `json:"spki_sha256"`
	Stores     []ReplicationIdentity `json:"stores"`
}

type loadedReplication struct {
	file            ReplicationFileConfig
	options         ReplicationOptions
	tls             *tls.Config
	transferTimeout time.Duration
}

func loadReplicationConfig(path string) (*loadedReplication, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("invalid replication config path")
	}
	data, err := replicationFile(absolute, true, edgeprotocol.MaxControlBytes)
	if err != nil {
		return nil, errors.New("replication config must be a bounded owner-only file")
	}
	var cfg ReplicationFileConfig
	if err := edgeprotocol.DecodeControl(bytes.NewReader(data), &cfg); err != nil || cfg.Version != ReplicationConfigVersion {
		return nil, errors.New("invalid closed replication configuration")
	}
	if !cfg.ManagedWritesOnly {
		return nil, errors.New("replication requires explicit managed-writes-only topology acknowledgement")
	}
	if cfg.MinFreeBytes == 0 || cfg.MinFreeBytes > 1<<60 {
		return nil, errors.New("replication requires explicit bounded filesystem headroom")
	}
	host, port, err := net.SplitHostPort(cfg.ListenAddr)
	ip := net.ParseIP(host)
	_, shared, _ := net.ParseCIDR("100.64.0.0/10")
	if err != nil || ip == nil || !(ip.IsLoopback() || ip.IsPrivate() || shared.Contains(ip)) {
		return nil, errors.New("replication listener must bind an explicit private or loopback IP")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, errors.New("replication listener requires an explicit valid port")
	}
	if cfg.ServerName == "" || cfg.SnapshotRoot == "" || len(cfg.Peers) == 0 || len(cfg.Peers) > 64 {
		return nil, errors.New("replication requires server identity, snapshot storage and exact peer grants")
	}
	resolve := func(value string) (string, error) {
		if value == "" {
			return "", errors.New("replication requires explicit trust and storage paths")
		}
		if filepath.IsAbs(value) {
			return filepath.Clean(value), nil
		}
		return filepath.Join(filepath.Dir(absolute), value), nil
	}
	tlsConfig, err := replicationTLS(cfg, resolve)
	if err != nil {
		return nil, err
	}
	parseDuration := func(value string, fallback time.Duration) (time.Duration, error) {
		if value == "" {
			return fallback, nil
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return 0, errors.New("invalid replication duration")
		}
		return d, nil
	}
	options := ReplicationOptions{AuthorityID: cfg.AuthorityID, ExportPolicyRevision: cfg.ExportPolicyRevision, ExportPrefixes: cfg.ExportPrefixes, MaxPackBytes: cfg.MaxPackBytes, MinFreeBytes: cfg.MinFreeBytes, Concurrency: cfg.Concurrency}
	options.SnapshotRoot, err = resolve(cfg.SnapshotRoot)
	if err != nil {
		return nil, err
	}
	options.PlanTTL, err = parseDuration(cfg.PlanTTL, time.Minute)
	if err != nil {
		return nil, err
	}
	options.RequestTimeout, err = parseDuration(cfg.RequestTimeout, time.Minute)
	if err != nil {
		return nil, err
	}
	options.Retention = ReplicationRetention{MaxBytes: cfg.MaxSnapshotBytes, MaxViews: cfg.MaxSnapshotViews}
	if options.Retention.MaxBytes == 0 {
		options.Retention.MaxBytes = 8 << 30
	}
	if options.Retention.MaxViews == 0 {
		options.Retention.MaxViews = 256
	}
	options.Retention.MinAge, err = parseDuration(cfg.ProtectRecent, 15*time.Minute)
	if err != nil {
		return nil, err
	}
	options.Retention.MaxAge, err = parseDuration(cfg.ExpireIdle, 24*time.Hour)
	if err != nil {
		return nil, err
	}
	if err := options.Retention.Validate(); err != nil {
		return nil, err
	}
	for _, p := range cfg.Peers {
		if len(p.Stores) == 0 || len(p.Stores) > 256 {
			return nil, errors.New("replication peer needs bounded exact stores")
		}
		for _, store := range p.Stores {
			if store.Validate() != nil || store.AuthorityID != cfg.AuthorityID || store.Kind != "repo" {
				return nil, errors.New("replication store does not match primary identity")
			}
		}
		options.Peers = append(options.Peers, ReplicationPeer{EdgeID: p.EdgeID, SPKISHA256: p.SPKISHA256, Stores: p.Stores})
	}
	transferTimeout, err := parseDuration(cfg.TransferTimeout, 10*time.Minute)
	if err != nil || transferTimeout < options.RequestTimeout || transferTimeout > time.Hour {
		return nil, errors.New("transfer timeout must cover control timeout and not exceed one hour")
	}
	return &loadedReplication{file: cfg, options: options, tls: tlsConfig, transferTimeout: transferTimeout}, nil
}

func replicationFile(path string, private bool, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit || private && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("invalid replication file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("replication file unavailable")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("replication file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("replication file read failed")
	}
	return data, nil
}
