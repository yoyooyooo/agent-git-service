package providerlogbridge

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Forgejo's public Actions API reports pull_request for a synchronized PR,
// while action_run.event stores pull_request_sync. Exercise the real bridge,
// SQL binding and log file instead of assuming both representations are equal.
func TestHandlerNormalizesPullRequestSyncWithExactBinding(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutation   string
		providerPR string
		token      string
		want       int
	}{
		{name: "synchronized", want: http.StatusOK},
		{name: "wrong_pr_same_commit", providerPR: "99", want: http.StatusConflict},
		{name: "wrong_run_commit", mutation: `UPDATE action_run SET commit_sha = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id = 4001`, want: http.StatusConflict},
		{name: "wrong_job_commit", mutation: `UPDATE action_run_job SET commit_sha = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id = 6763`, want: http.StatusConflict},
		{name: "wrong_task_commit", mutation: `UPDATE action_task SET commit_sha = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id = 6949`, want: http.StatusConflict},
		{name: "wrong_ref", mutation: `UPDATE action_run SET ref = 'refs/heads/fix/log-fixture' WHERE id = 4001`, want: http.StatusConflict},
		{name: "unknown_event", mutation: `UPDATE action_run SET event = 'pull_request_unknown' WHERE id = 4001`, want: http.StatusConflict},
		{name: "target_event", mutation: `UPDATE action_run SET event = 'pull_request_target' WHERE id = 4001`, want: http.StatusConflict},
		{name: "wrong_token", token: "wrong", want: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := setupBridge(t, 1024)
			if err := h.db.Exec(`UPDATE action_run SET event = 'pull_request_sync' WHERE id = 4001`).Error; err != nil {
				t.Fatal(err)
			}
			if tc.mutation != "" {
				if err := h.db.Exec(tc.mutation).Error; err != nil {
					t.Fatal(err)
				}
			}
			pr, token := tc.providerPR, tc.token
			if pr == "" {
				pr = "98"
			}
			if token == "" {
				token = "bridge-secret"
			}
			got := requestBinding(t, h, fixtureOwner, fixtureRepo, "6949", token, pr, fixtureRef, fixtureSHA)
			if got.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", got.Code, tc.want, got.Body.String())
			}
			if tc.want != http.StatusOK {
				if strings.Contains(got.Body.String(), "checkout git fetch") {
					t.Fatal("rejected binding exposed log text")
				}
				return
			}
			var log response
			if err := json.Unmarshal(got.Body.Bytes(), &log); err != nil {
				t.Fatal(err)
			}
			if log.Event != "pull_request" || log.ProviderPR != 98 || log.ProviderRef != "refs/pull/98/head" || log.HeadSHA != fixtureSHA || log.Text != "checkout git fetch timed out after 15m\n" {
				t.Fatalf("unexpected normalized log: %#v", log)
			}
			binding, _, _, err := h.binding(t.Context(), fixtureOwner, fixtureRepo, 6949)
			if err != nil || binding.Event != "pull_request_sync" {
				t.Fatalf("normalizing the response changed stored provenance: %#v %v", binding, err)
			}
		})
	}
}
