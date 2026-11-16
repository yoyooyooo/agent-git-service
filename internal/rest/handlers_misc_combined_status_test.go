package rest_test

import (
	"fmt"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

func TestCombinedStatus_EmptyStatusesArePending(t *testing.T) {
	h := testharness.New(t)
	repo, headSHA := seedCheckRunsRepo(t, h, "combined-status-empty")

	for _, ref := range []string{repo.DefaultBranch, headSHA} {
		t.Run(ref, func(t *testing.T) {
			path := fmt.Sprintf("/api/v3/repos/%s/commits/%s/status", repo.FullName, ref)
			w := h.DoREST(t, "GET", path, nil)
			assertStatusCode(t, w, 200)
			body := testharness.DecodeJSON(t, w)
			if got, _ := body["state"].(string); got != "pending" {
				t.Fatalf("empty combined status state: got %q, want pending", got)
			}
			if got := int(body["total_count"].(float64)); got != 0 {
				t.Fatalf("total_count: got %d, want 0", got)
			}
			statuses, _ := body["statuses"].([]any)
			if len(statuses) != 0 {
				t.Fatalf("statuses length: got %d, want 0", len(statuses))
			}
		})
	}
}

func TestCombinedStatus_SuccessfulRunStaysSuccess(t *testing.T) {
	h := testharness.New(t)
	repo, headSHA := seedCheckRunsRepo(t, h, "combined-status-success")
	seedWorkflowCheckRun(t, h, repo, headSHA)

	path := fmt.Sprintf("/api/v3/repos/%s/commits/%s/status", repo.FullName, headSHA)
	w := h.DoREST(t, "GET", path, nil)
	assertStatusCode(t, w, 200)
	body := testharness.DecodeJSON(t, w)
	if got, _ := body["state"].(string); got != "success" {
		t.Fatalf("combined status state: got %q, want success", got)
	}
	if got := int(body["total_count"].(float64)); got != 1 {
		t.Fatalf("total_count: got %d, want 1", got)
	}
}

func TestCombinedStatus_InProgressRunIsPending(t *testing.T) {
	h := testharness.New(t)
	repo, headSHA := seedCheckRunsRepo(t, h, "combined-status-pending")
	run, _ := seedWorkflowCheckRun(t, h, repo, headSHA)
	if err := h.DB.Model(&run).Updates(map[string]any{"status": db.RunInProgress, "conclusion": ""}).Error; err != nil {
		t.Fatalf("mark run in progress: %v", err)
	}

	path := fmt.Sprintf("/api/v3/repos/%s/commits/%s/status", repo.FullName, headSHA)
	w := h.DoREST(t, "GET", path, nil)
	assertStatusCode(t, w, 200)
	body := testharness.DecodeJSON(t, w)
	if got, _ := body["state"].(string); got != "pending" {
		t.Fatalf("combined status state: got %q, want pending", got)
	}
}
