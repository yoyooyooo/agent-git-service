package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

// Node RPCs deliberately have no credential argument. These may run in a
// background task; user Authorization only belongs to foreground read control.
func (c *PeerClient) nodeRPC(ctx context.Context, path string, input, output any) error {
	method := http.MethodGet
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return errors.New("invalid node request")
		}
		body = bytes.NewReader(data)
		method = http.MethodPost
	}
	endpoint := strings.TrimSuffix(c.endpoint, edgeprotocol.ExportPath) + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return errors.New("invalid node endpoint")
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(req)
	if err != nil {
		return errors.New("node_transport_unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return &ReadControlError{Status: response.StatusCode}
	}
	if response.Header.Get("Content-Type") != "application/json" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Encoding") != "" {
		return errors.New("invalid_node_response")
	}
	return edgeprotocol.DecodeControl(response.Body, output)
}
func (c *PeerClient) NodeStatus(ctx context.Context, edgeID string) (edgeprotocol.NodeStatus, error) {
	var status edgeprotocol.NodeStatus
	err := c.nodeRPC(ctx, edgeprotocol.NodeStatusPath, nil, &status)
	if err == nil && (status.Version != edgeprotocol.NodeVersion || status.EdgeID != edgeID || status.Stores < 1) {
		err = errors.New("node_status_binding_mismatch")
	}
	return status, err
}
func (c *PeerClient) WarmSnapshot(ctx context.Context, identity edgeprotocol.RepositoryIdentity) (edgeprotocol.RepositorySnapshot, error) {
	var result edgeprotocol.WarmResponse
	if identity.Validate() != nil {
		return result.Snapshot, errors.New("invalid_warm_identity")
	}
	err := c.nodeRPC(ctx, edgeprotocol.WarmPath, edgeprotocol.WarmRequest{Version: edgeprotocol.NodeVersion, Identity: identity}, &result)
	if err == nil && (result.Version != edgeprotocol.NodeVersion || result.Snapshot.Validate() != nil || result.Snapshot.Identity != identity) {
		err = errors.New("warm_snapshot_binding_mismatch")
	}
	return result.Snapshot, err
}

type WarmSource interface {
	WarmSnapshot(context.Context, edgeprotocol.RepositoryIdentity) (edgeprotocol.RepositorySnapshot, error)
}

type WarmState struct {
	Repository          string    `json:"repository"`
	State               string    `json:"state"`
	CheckedAt           time.Time `json:"checked_at,omitempty"`
	SyncedAt            time.Time `json:"synced_at,omitempty"`
	Snapshot            string    `json:"snapshot,omitempty"`
	Error               string    `json:"error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
}

type Prewarmer struct {
	mu                  sync.Mutex
	states              []WarmState
	bindings            []ReadBinding
	source              WarmSource
	reader              SnapshotReader
	cancel              context.CancelFunc
	done                chan struct{}
	interval, timeLimit time.Duration
}

func StartPrewarmer(ctx context.Context, source WarmSource, reader SnapshotReader, bindings []ReadBinding, interval, timeLimit time.Duration) (*Prewarmer, error) {
	if source == nil || reader == nil || len(bindings) == 0 || len(bindings) > 256 || interval < 10*time.Second || interval > time.Hour || timeLimit < time.Second || timeLimit > time.Hour {
		return nil, errors.New("invalid prewarm configuration")
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &Prewarmer{source: source, reader: reader, bindings: append([]ReadBinding{}, bindings...), cancel: cancel, done: make(chan struct{}), interval: interval, timeLimit: timeLimit}
	for _, b := range bindings {
		if b.Identity.Validate() != nil {
			cancel()
			return nil, errors.New("invalid warm binding")
		}
		p.states = append(p.states, WarmState{Repository: b.Repository, State: "pending"})
	}
	go p.run(ctx)
	return p, nil
}
func (p *Prewarmer) run(ctx context.Context) {
	defer close(p.done)
	// Low-priority bounded reconciliation, not an event log or an authority
	// cache. Restart rechecks exact identities and Mirror reuses persisted data.
	next := make([]time.Time, len(p.bindings))
	// Wake cheaply to honor per-repository completion/backoff deadlines;
	// a ticker at interval would skip the first deadline and double cadence.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		for i, b := range p.bindings {
			if ctx.Err() != nil {
				return
			}
			if time.Now().Before(next[i]) {
				continue
			}
			p.mu.Lock()
			p.states[i].State = "synchronizing"
			p.states[i].CheckedAt = time.Now().UTC()
			p.mu.Unlock()
			call, cancel := context.WithTimeout(ctx, p.timeLimit)
			snapshot, err := p.source.WarmSnapshot(call, b.Identity)
			if err == nil {
				var v SnapshotView
				v, err = p.reader.EnsureSnapshot(call, snapshot)
				if v != nil {
					v.Release()
				}
			}
			cancel()
			p.mu.Lock()
			state := &p.states[i]
			backoff := p.interval
			if err == nil {
				state.State = "synchronized"
				state.Error = ""
				state.ConsecutiveFailures = 0
				state.SyncedAt = time.Now().UTC()
				state.Snapshot, _ = edgeprotocol.SnapshotKey(snapshot)
			} else {
				state.State = "failed"
				state.Error = operationError(err)
				state.ConsecutiveFailures++
				for n := 1; n < state.ConsecutiveFailures && backoff < 5*time.Minute; n++ {
					backoff *= 2
				}
				if backoff > 5*time.Minute {
					backoff = 5 * time.Minute
				}
			}
			p.mu.Unlock()
			next[i] = time.Now().Add(backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (p *Prewarmer) Snapshot() []WarmState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]WarmState{}, p.states...)
}
func (p *Prewarmer) Close() { p.cancel(); <-p.done }
