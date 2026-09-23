package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

// ReadControlError preserves only the primary status, never a raw response,
// endpoint or credential. The public Git adapter uses 401 to challenge a
// credential helper and keeps operational failures distinct from denials.
type ReadControlError struct{ Status int }

func (e *ReadControlError) Error() string {
	return fmt.Sprintf("read control rejected (HTTP %d)", e.Status)
}

// Original Authorization is request-scoped: never put it in PeerClient, a
// mirror job, a descriptor, or the background export client. Empty is allowed
// only so the primary can decide its existing anonymous/public Git semantics.
func (c *PeerClient) PrepareRead(ctx context.Context, edgeID, authorization string, request edgeprotocol.PrepareRead) (edgeprotocol.ReadPlan, error) {
	if err := request.Validate(); err != nil {
		return edgeprotocol.ReadPlan{}, err
	}
	plan, err := c.readControl(ctx, edgeprotocol.PreparePath, authorization, request)
	if err != nil {
		return edgeprotocol.ReadPlan{}, err
	}
	if plan.ValidateFor(edgeID, time.Now().UTC()) != nil || plan.RequestID != request.RequestID || plan.Phase != request.Phase || plan.Snapshot.Identity != request.Identity {
		return edgeprotocol.ReadPlan{}, errors.New("primary read plan does not match request")
	}
	if request.Snapshot != nil && *request.Snapshot != plan.Snapshot {
		return edgeprotocol.ReadPlan{}, errors.New("primary substituted a different fetch view")
	}
	return plan, nil
}

func (c *PeerClient) RevalidateRead(ctx context.Context, edgeID, authorization string, plan edgeprotocol.ReadPlan) error {
	if err := plan.ValidateFor(edgeID, time.Now().UTC()); err != nil {
		return err
	}
	verified, err := c.readControl(ctx, edgeprotocol.RevalidatePath, authorization, plan)
	if err != nil {
		return err
	}
	if verified.ValidateFor(edgeID, time.Now().UTC()) != nil || verified.Version != plan.Version || verified.RequestID != plan.RequestID || verified.EdgeID != plan.EdgeID || verified.Operation != plan.Operation || verified.Phase != plan.Phase || verified.Snapshot != plan.Snapshot || !verified.AuthorizedAt.Equal(plan.AuthorizedAt) || !verified.ExpiresAt.Equal(plan.ExpiresAt) {
		return errors.New("primary changed the revalidated read plan")
	}
	return nil
}

func (c *PeerClient) readControl(ctx context.Context, path, authorization string, body any) (edgeprotocol.ReadPlan, error) {
	if strings.ContainsAny(authorization, "\r\n\x00") || len(authorization) > 16<<10 {
		return edgeprotocol.ReadPlan{}, errors.New("invalid original authorization")
	}
	data, err := json.Marshal(body)
	if err != nil || len(data) > edgeprotocol.MaxControlBytes {
		return edgeprotocol.ReadPlan{}, errors.New("invalid read control request")
	}
	url := strings.TrimSuffix(c.endpoint, edgeprotocol.ExportPath) + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return edgeprotocol.ReadPlan{}, errors.New("invalid fixed read endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return edgeprotocol.ReadPlan{}, errors.New("read control transport unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return edgeprotocol.ReadPlan{}, &ReadControlError{Status: resp.StatusCode}
	}
	if resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Cache-Control") != "no-store" {
		return edgeprotocol.ReadPlan{}, errors.New("invalid read control representation")
	}
	var plan edgeprotocol.ReadPlan
	if err := edgeprotocol.DecodeControl(resp.Body, &plan); err != nil {
		return edgeprotocol.ReadPlan{}, err
	}
	return plan, nil
}
