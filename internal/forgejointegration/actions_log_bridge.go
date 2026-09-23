package forgejointegration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/providerlogprotocol"
)

type actionsLogBridgeResponse struct {
	Schema      string `json:"schema"`
	Repo        string `json:"repo"`
	TaskID      int64  `json:"task_id"`
	RunNumber   int64  `json:"run_number"`
	JobName     string `json:"job_name"`
	HeadSHA     string `json:"head_sha"`
	ProviderPR  int64  `json:"provider_pr"`
	HeadRef     string `json:"head_ref"`
	ProviderRef string `json:"provider_ref"`
	Event       string `json:"event"`
	Text        string `json:"text"`
}

type actionsLogBridgeError struct {
	Code  string
	Cause error
}

func (e *actionsLogBridgeError) Error() string {
	if e == nil || e.Cause == nil {
		return "provider actions log bridge failed"
	}
	return "provider actions log bridge " + e.Code + ": " + e.Cause.Error()
}

func (e *actionsLogBridgeError) Unwrap() error { return e.Cause }

func bridgeError(code string, err error) error {
	return &actionsLogBridgeError{Code: code, Cause: err}
}

func readActionsLogBridge(ctx context.Context, baseURL, token, owner, repo string, taskID int64, providerPR int, headRef, headSHA string) ([]byte, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, bridgeError("invalid_config", fmt.Errorf("URL is invalid"))
	}
	if strings.TrimSpace(token) == "" {
		return nil, bridgeError("invalid_config", fmt.Errorf("token is missing"))
	}
	path := "/api/internal/provider-logs/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/tasks/" + strconv.FormatInt(taskID, 10)
	query := url.Values{
		"provider_pr": {strconv.Itoa(providerPR)},
		"head_ref":    {headRef},
		"head_sha":    {headSHA},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, bridgeError("request", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, bridgeError("request", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, providerlogprotocol.EncodedResponseMaxBytes+1))
	if err != nil {
		return nil, bridgeError("read", err)
	}
	if int64(len(body)) > providerlogprotocol.EncodedResponseMaxBytes {
		return nil, bridgeError("response_too_large", fmt.Errorf("response exceeds limit"))
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, bridgeError("status", fmt.Errorf("typed status %d", res.StatusCode))
	}
	var payload actionsLogBridgeResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, bridgeError("decode", err)
	}
	wantRepo := owner + "/" + repo
	if payload.Schema != "ags.internal-provider-log.v1" || payload.Repo != wantRepo || payload.TaskID != taskID || payload.HeadSHA != headSHA || payload.Text == "" {
		return nil, bridgeError("identity_mismatch", fmt.Errorf("repository, task, or SHA mismatch"))
	}
	pullRef := "refs/pull/" + strconv.Itoa(providerPR) + "/head"
	branchRef := "refs/heads/" + headRef
	pullBound := payload.Event == "pull_request" && payload.ProviderPR == int64(providerPR) && payload.HeadRef == "" && payload.ProviderRef == pullRef
	branchBound := payload.Event == "workflow_dispatch" && payload.ProviderPR == 0 && payload.HeadRef == headRef && payload.ProviderRef == branchRef
	if !pullBound && !branchBound {
		return nil, bridgeError("binding_mismatch", fmt.Errorf("provider PR, ref, or event mismatch"))
	}
	return []byte(payload.Text), nil
}
