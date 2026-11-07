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
	for _, mode := range []string{"valid", "orphan", "duplicate_name", "foreign_repo", "foreign_run", "wrong_sha", "wrong_token", "wrong_peer", "duplicate_task", "task_sha", "job_sha", "missing_task", "pending_task", "job_repo", "task_repo", "job_and_task_repo"} {
		t.Run(mode, func(t *testing.T) {
			h, _ := setupBridge(t, 1024)
			owner, repo, run, sha, token, peer := fixtureOwner, fixtureRepo, "4001", fixtureSHA, "bridge-secret", fixturePeer
			count := 1
			switch mode {
			case "orphan":
				require.NoError(t, h.db.Exec(`INSERT INTO action_task VALUES (6951,24,?,'unused.log',true,false)`, fixtureSHA).Error)
			case "duplicate_name":
				require.NoError(t, h.db.Exec(`INSERT INTO action_task VALUES (6951,24,?,'unused.log',true,false)`, fixtureSHA).Error)
				require.NoError(t, h.db.Exec(`INSERT INTO action_run_job (id,run_id,repo_id,task_id,name,commit_sha) VALUES (6765,4001,24,6951,'Backend Smoke',?)`, fixtureSHA).Error)
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
				require.NoError(t, h.db.Exec(`INSERT INTO action_run_job (id,run_id,repo_id,task_id,name,commit_sha) VALUES (6765,4001,24,6949,'other',?)`, fixtureSHA).Error)
			case "task_sha":
				require.NoError(t, h.db.Exec(`UPDATE action_task SET commit_sha=? WHERE id=6949`, strings.Repeat("2", 40)).Error)
			case "job_sha":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET commit_sha=? WHERE id=6763`, strings.Repeat("2", 40)).Error)
			case "missing_task":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET task_id=9999 WHERE id=6763`).Error)
			case "pending_task":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET task_id=0 WHERE id=6763`).Error)
			case "job_repo":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET repo_id=25 WHERE id=6763`).Error)
			case "task_repo":
				require.NoError(t, h.db.Exec(`UPDATE action_task SET repo_id=25 WHERE id=6949`).Error)
			case "job_and_task_repo":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET repo_id=25 WHERE id=6763`).Error)
				require.NoError(t, h.db.Exec(`UPDATE action_task SET repo_id=25 WHERE id=6949`).Error)
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
			case "job_repo", "task_repo", "job_and_task_repo":
				require.Equal(t, http.StatusBadGateway, res.Code, res.Body.String())
				var body map[string]any
				require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
				require.Equal(t, "metadata_invalid", body["code"])
				require.NotContains(t, body, "jobs", "invalid inventory must not return partial jobs")
			default:
				require.Equal(t, 502, res.Code, res.Body.String())
			}
		})
	}
}

func TestCIBridgeCurrentJobsAllowUnstartedSkippedJobsWithoutTasks(t *testing.T) {
	for _, mode := range []string{
		"mixed", "all_skipped", "unknown", "success", "failure", "cancelled", "waiting", "running", "blocked", "future_status",
		"started", "stopped", "negative_started", "null_status", "null_started", "null_stopped",
		"negative_task", "null_task", "missing_task", "task_zero_exists", "task_zero_has_null_binding", "foreign_repo", "wrong_sha", "empty_name",
	} {
		t.Run(mode, func(t *testing.T) {
			h, _ := setupBridge(t, 1024)
			// A failed task, a never-started skipped job and a successful task can
			// coexist in one Forgejo run. Only the two current tasks have logs.
			require.NoError(t, h.db.Exec(`UPDATE action_run_job SET status=2,started=10,stopped=20 WHERE id=6763`).Error)
			require.NoError(t, h.db.Exec(`INSERT INTO action_run_job (id,run_id,repo_id,task_id,name,commit_sha,status,started,stopped) VALUES (6765,4001,24,0,'Conditional Check',?,4,0,0)`, fixtureSHA).Error)
			require.NoError(t, h.db.Exec(`INSERT INTO action_task VALUES (6951,24,?,'unused.log',true,false)`, fixtureSHA).Error)
			require.NoError(t, h.db.Exec(`INSERT INTO action_run_job (id,run_id,repo_id,task_id,name,commit_sha,status,started,stopped) VALUES (6766,4001,24,6951,'Report Result',?,1,21,22)`, fixtureSHA).Error)
			switch mode {
			case "all_skipped":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET task_id=0,status=4,started=0,stopped=0 WHERE run_id=4001`).Error)
			case "unknown", "success", "failure", "cancelled", "waiting", "running", "blocked", "future_status":
				status := map[string]int{"unknown": 0, "success": 1, "failure": 2, "cancelled": 3, "waiting": 5, "running": 6, "blocked": 7, "future_status": 99}[mode]
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET status=? WHERE id=6765`, status).Error)
			case "started":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET started=10 WHERE id=6765`).Error)
			case "stopped":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET stopped=20 WHERE id=6765`).Error)
			case "negative_started":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET started=-1 WHERE id=6765`).Error)
			case "null_status":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET status=NULL WHERE id=6765`).Error)
			case "null_started":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET started=NULL WHERE id=6765`).Error)
			case "null_stopped":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET stopped=NULL WHERE id=6765`).Error)
			case "negative_task":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET task_id=-1 WHERE id=6765`).Error)
			case "null_task":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET task_id=NULL WHERE id=6765`).Error)
			case "missing_task":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET task_id=9999 WHERE id=6765`).Error)
			case "task_zero_exists":
				require.NoError(t, h.db.Exec(`INSERT INTO action_task VALUES (0,24,?,'unused.log',true,false)`, fixtureSHA).Error)
			case "task_zero_has_null_binding":
				require.NoError(t, h.db.Exec(`INSERT INTO action_task VALUES (0,NULL,NULL,'unused.log',true,false)`).Error)
			case "foreign_repo":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET repo_id=25 WHERE id=6765`).Error)
			case "wrong_sha":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET commit_sha=? WHERE id=6765`, strings.Repeat("2", 40)).Error)
			case "empty_name":
				require.NoError(t, h.db.Exec(`UPDATE action_run_job SET name='' WHERE id=6765`).Error)
			}
			router := chi.NewRouter()
			router.Use(CaptureSocketPeer)
			router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/runs/{run}/jobs", h.ServeHTTP)
			req := httptest.NewRequest(http.MethodGet, "/api/internal/provider-logs/repos/"+fixtureOwner+"/"+fixtureRepo+"/runs/4001/jobs?head_sha="+fixtureSHA, nil)
			req.RemoteAddr = fixturePeer
			req.Header.Set("Authorization", "Bearer bridge-secret")
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			if mode != "mixed" && mode != "all_skipped" {
				require.Equal(t, http.StatusBadGateway, res.Code, res.Body.String())
				var body map[string]any
				require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
				require.Equal(t, "metadata_invalid", body["code"])
				require.NotContains(t, body, "jobs", "invalid inventory must not return partial jobs")
				return
			}
			require.Equal(t, http.StatusOK, res.Code, res.Body.String())
			var body struct {
				Jobs []struct {
					TaskID int64  `json:"task_id"`
					Name   string `json:"name"`
				}
			}
			require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
			require.NotNil(t, body.Jobs)
			if mode == "all_skipped" {
				require.Empty(t, body.Jobs, "orphaned historical tasks must not be substituted")
				return
			}
			require.Len(t, body.Jobs, 2)
			require.Equal(t, int64(6949), body.Jobs[0].TaskID)
			require.Equal(t, "Backend Smoke", body.Jobs[0].Name)
			require.Equal(t, int64(6951), body.Jobs[1].TaskID)
			require.Equal(t, "Report Result", body.Jobs[1].Name)
		})
	}
}
