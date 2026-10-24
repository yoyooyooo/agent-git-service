package providerlogbridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

func TestCIBridgeCurrentJobsRequireRepositoryRunTaskAndCommit(t *testing.T) {
	for _, mode := range []string{"valid", "orphan", "duplicate_name", "foreign_repo", "foreign_run", "wrong_sha", "wrong_token", "wrong_peer", "duplicate_task", "task_sha", "job_sha", "missing_task", "pending_task"} {
		t.Run(mode, func(t *testing.T) {
			h, _ := setupBridge(t, 1024)
			owner, repo, run, sha, token, peer := fixtureOwner, fixtureRepo, "4001", fixtureSHA, "bridge-secret", fixturePeer
			count := 1
			switch mode {
			case "orphan":
				require.NoError(t, h.db.Exec(`INSERT INTO action_task VALUES (6951,24,?,'unused.log',true,false)`, fixtureSHA).Error)
			case "duplicate_name":
				require.NoError(t, h.db.Exec(`INSERT INTO action_task VALUES (6951,24,?,'unused.log',true,false)`, fixtureSHA).Error)
				require.NoError(t, h.db.Exec(`INSERT INTO action_run_job VALUES (6765,4001,24,6951,'Backend Smoke',?)`, fixtureSHA).Error)
				count = 2
			case "foreign_repo":
				repo = "other"
			case "foreign_run":
				run = "4003"
			case "wrong_sha":
				sha = strings.Repeat("2", 40)
			case "wrong_token":
				token = "wrong"
			case "wrong_peer":
				peer = "198.51.100.10:1234"
			case "duplicate_task":
				require.NoError(t, h.db.Exec(`INSERT INTO action_run_job VALUES (6765,4001,24,6949,'other',?)`, fixtureSHA).Error)
			case "task_sha":
				require.NoError(t, h.db.Exec(`UPDATE action_task SET commit_sha=? WHERE id=6949`, strings.Repeat("2", 40)).Error)
			case "job_sha":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET commit_sha=? WHERE id=6763`, strings.Repeat("2", 40)).Error)
			case "missing_task":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET task_id=9999 WHERE id=6763`).Error)
			case "pending_task":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET task_id=0 WHERE id=6763`).Error)
			}
			router := chi.NewRouter()
			router.Use(CaptureSocketPeer)
			router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/runs/{run}/jobs", h.ServeHTTP)
			req := httptest.NewRequest("GET", "/api/internal/provider-logs/repos/"+owner+"/"+repo+"/runs/"+run+"/jobs?head_sha="+sha, nil)
			req.RemoteAddr = peer
			req.Header.Set("Authorization", "Bearer "+token)
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			switch mode {
			case "valid", "orphan", "duplicate_name":
				require.Equal(t, http.StatusOK, res.Code, res.Body.String())
				var body struct {
					Jobs []struct {
						TaskID int64 `json:"task_id"`
					}
				}
				require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
				require.Len(t, body.Jobs, count)
				for _, job := range body.Jobs {
					require.NotEqual(t, int64(6950), job.TaskID, "another run must not leak")
				}
			case "wrong_token", "wrong_peer":
				require.Equal(t, 401, res.Code, res.Body.String())
			case "foreign_repo", "foreign_run", "wrong_sha":
				require.Equal(t, 404, res.Code, res.Body.String())
			default:
				require.Equal(t, 502, res.Code, res.Body.String())
			}
		})
	}
}
