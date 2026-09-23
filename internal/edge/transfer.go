package edge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

// IncrementalSnapshotSource is optional for test/adaptor compatibility. Real
// PeerClient supports v2. A nominated base is an exact pinned local descriptor.
// A nil response Base declares a complete target pack, never a stale target.
type IncrementalSnapshotSource interface {
	OpenTransfer(context.Context, edgeprotocol.RepositorySnapshot, *edgeprotocol.RepositorySnapshot) (edgeprotocol.TransferHeader, io.ReadCloser, error)
}

func (c *PeerClient) OpenTransfer(ctx context.Context, target edgeprotocol.RepositorySnapshot, base *edgeprotocol.RepositorySnapshot) (edgeprotocol.TransferHeader, io.ReadCloser, error) {
	data, err := edgeprotocol.EncodeTransferRequest(edgeprotocol.TransferRequest{Version: edgeprotocol.TransferVersion, Target: target, Base: base})
	if err != nil {
		return edgeprotocol.TransferHeader{}, nil, err
	}
	endpoint := strings.TrimSuffix(c.endpoint, edgeprotocol.ExportPath) + edgeprotocol.TransferPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return edgeprotocol.TransferHeader{}, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return edgeprotocol.TransferHeader{}, nil, errors.New("incremental peer transport unavailable")
	}
	fail := func(err error) (edgeprotocol.TransferHeader, io.ReadCloser, error) {
		_ = resp.Body.Close()
		return edgeprotocol.TransferHeader{}, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("incremental peer rejected export (HTTP %d)", resp.StatusCode))
	}
	if resp.Header.Get("Content-Type") != edgeprotocol.TransferContentType || resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Cache-Control") != "no-store" {
		return fail(errors.New("unexpected incremental export representation"))
	}
	header, err := edgeprotocol.ReadTransferHeader(resp.Body)
	if err != nil {
		return fail(err)
	}
	if header.Manifest.Snapshot != target || header.Base != nil && (base == nil || *header.Base != *base) {
		return fail(errors.New("peer substituted incremental target or base"))
	}
	return header, resp.Body, nil
}
