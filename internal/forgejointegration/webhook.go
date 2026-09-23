package forgejointegration

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// AGSActionRebaseLabel is the Forgejo PR label that asks AGS to rebase the authoritative PR head.
	AGSActionRebaseLabel = "ags/action-rebase"

	AGSStatusRebasingLabel        = "ags/status-rebasing"
	AGSStatusProjectionDriftLabel = "ags/status-projection-drift"
	AGSStatusRebaseConflictLabel  = "ags/status-rebase-conflict"
	AGSStatusBlockedLabel         = "ags/status-blocked"
	AGSStatusNeedsRebaseLabel     = "ags/status-needs-rebase"
)

// MergedPullRequestEvent is the minimal Forgejo PR-merged event used for merge authority callbacks.
type MergedPullRequestEvent struct {
	RepoFullName string
	PRNumber     int
	PRURL        string
	HeadBranch   string
	BaseBranch   string
}

// ClosedPullRequestEvent is the minimal Forgejo PR-closed event used for lifecycle authority callbacks.
type ClosedPullRequestEvent struct {
	RepoFullName string
	PRNumber     int
	PRURL        string
	HeadBranch   string
	BaseBranch   string
	Merged       bool
}

// DeletedBranchEvent is the minimal Forgejo branch-deleted event used for source-branch cleanup.
type DeletedBranchEvent struct {
	RepoFullName string
	BranchName   string
}

// PullRequestActionLabelEvent is a signed Forgejo PR label event that requests AGS-owned workflow work.
type PullRequestActionLabelEvent struct {
	RepoFullName  string
	PRNumber      int
	PRURL         string
	HeadBranch    string
	HeadSHA       string
	BaseBranch    string
	LabelName     string
	SenderLogin   string
	CorrelationID string
}

// VerifyWebhookSignature validates a Forgejo/Gitea/GitHub-compatible HMAC signature.
func VerifyWebhookSignature(secret, signature string, body []byte) error {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return fmt.Errorf("webhook secret is not configured")
	}
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return fmt.Errorf("webhook signature is missing")
	}
	alg, encoded, ok := strings.Cut(signature, "=")
	if !ok {
		return fmt.Errorf("webhook signature format is invalid")
	}
	got, err := hex.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("webhook signature hex decode: %w", err)
	}
	var expected []byte
	switch strings.ToLower(alg) {
	case "sha256":
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		expected = mac.Sum(nil)
	case "sha1":
		mac := hmac.New(sha1.New, []byte(secret))
		_, _ = mac.Write(body)
		expected = mac.Sum(nil)
	default:
		return fmt.Errorf("unsupported webhook signature algorithm %q", alg)
	}
	if !hmac.Equal(got, expected) {
		return fmt.Errorf("webhook signature mismatch")
	}
	return nil
}

// ParseMergedPullRequestWebhook extracts a merged pull request event from Forgejo payload JSON.
func ParseDeletedBranchWebhook(body []byte) (DeletedBranchEvent, bool, error) {
	var payload struct {
		Ref        string `json:"ref"`
		RefType    string `json:"ref_type"`
		Deleted    bool   `json:"deleted"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return DeletedBranchEvent{}, false, fmt.Errorf("decode forgejo delete webhook: %w", err)
	}
	ref := strings.TrimSpace(payload.Ref)
	refType := strings.TrimSpace(payload.RefType)
	branch := ""
	if strings.EqualFold(refType, "branch") && ref != "" {
		branch = strings.TrimPrefix(ref, "refs/heads/")
	} else if payload.Deleted && strings.HasPrefix(ref, "refs/heads/") {
		branch = strings.TrimPrefix(ref, "refs/heads/")
	}
	if strings.TrimSpace(payload.Repository.FullName) == "" || branch == "" {
		return DeletedBranchEvent{}, false, nil
	}
	return DeletedBranchEvent{RepoFullName: strings.TrimSpace(payload.Repository.FullName), BranchName: branch}, true, nil
}

// ParsePullRequestActionLabelWebhook extracts a supported AGS action-label event from a Forgejo PR webhook.
func ParsePullRequestActionLabelWebhook(body []byte) (PullRequestActionLabelEvent, bool, error) {
	var payload struct {
		Action     string `json:"action"`
		Repository struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
		PullRequest struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
			Head    struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		} `json:"pull_request"`
		Label struct {
			Name string `json:"name"`
		} `json:"label"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return PullRequestActionLabelEvent{}, false, fmt.Errorf("decode forgejo pull_request label webhook: %w", err)
	}
	labelName := strings.TrimSpace(payload.Label.Name)
	if labelName == "" && pullRequestHasLabel(payload.PullRequest.Labels, AGSActionRebaseLabel) {
		labelName = AGSActionRebaseLabel
	}
	if labelName != AGSActionRebaseLabel {
		return PullRequestActionLabelEvent{}, false, nil
	}
	action := strings.ToLower(strings.TrimSpace(payload.Action))
	labelStillPresent := pullRequestHasLabel(payload.PullRequest.Labels, labelName)
	switch action {
	case "labeled":
		// GitHub-style label add event. Some payloads omit the full current label list.
	case "", "label_updated", "label_changed":
		if !labelStillPresent {
			return PullRequestActionLabelEvent{}, false, nil
		}
	default:
		if !labelStillPresent {
			return PullRequestActionLabelEvent{}, false, nil
		}
	}
	if payload.Repository.FullName == "" || payload.PullRequest.Number == 0 {
		return PullRequestActionLabelEvent{}, false, nil
	}
	base := payload.PullRequest.Base.Ref
	if base == "" {
		base = payload.Repository.DefaultBranch
	}
	return PullRequestActionLabelEvent{
		RepoFullName: strings.TrimSpace(payload.Repository.FullName),
		PRNumber:     payload.PullRequest.Number,
		PRURL:        strings.TrimSpace(payload.PullRequest.HTMLURL),
		HeadBranch:   strings.TrimSpace(payload.PullRequest.Head.Ref),
		HeadSHA:      strings.TrimSpace(payload.PullRequest.Head.SHA),
		BaseBranch:   strings.TrimSpace(base),
		LabelName:    labelName,
		SenderLogin:  strings.TrimSpace(payload.Sender.Login),
	}, true, nil
}

func pullRequestHasLabel(labels []struct {
	Name string `json:"name"`
}, name string) bool {
	for _, label := range labels {
		if strings.TrimSpace(label.Name) == name {
			return true
		}
	}
	return false
}

func ParseClosedPullRequestWebhook(body []byte) (ClosedPullRequestEvent, bool, error) {
	var payload struct {
		Action     string `json:"action"`
		Repository struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
		PullRequest struct {
			Number  int    `json:"number"`
			Merged  bool   `json:"merged"`
			HTMLURL string `json:"html_url"`
			Head    struct {
				Ref string `json:"ref"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ClosedPullRequestEvent{}, false, fmt.Errorf("decode forgejo pull_request webhook: %w", err)
	}
	if payload.Action != "closed" || payload.Repository.FullName == "" || payload.PullRequest.Number == 0 {
		return ClosedPullRequestEvent{}, false, nil
	}
	base := payload.PullRequest.Base.Ref
	if base == "" {
		base = payload.Repository.DefaultBranch
	}
	return ClosedPullRequestEvent{
		RepoFullName: payload.Repository.FullName,
		PRNumber:     payload.PullRequest.Number,
		PRURL:        payload.PullRequest.HTMLURL,
		HeadBranch:   payload.PullRequest.Head.Ref,
		BaseBranch:   base,
		Merged:       payload.PullRequest.Merged,
	}, true, nil
}

func ParseMergedPullRequestWebhook(body []byte) (MergedPullRequestEvent, bool, error) {
	event, ok, err := ParseClosedPullRequestWebhook(body)
	if err != nil || !ok || !event.Merged {
		return MergedPullRequestEvent{}, false, err
	}
	return MergedPullRequestEvent{
		RepoFullName: event.RepoFullName,
		PRNumber:     event.PRNumber,
		PRURL:        event.PRURL,
		HeadBranch:   event.HeadBranch,
		BaseBranch:   event.BaseBranch,
	}, true, nil
}
