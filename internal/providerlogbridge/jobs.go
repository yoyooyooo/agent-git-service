package providerlogbridge

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// serveJobs observes the current job-to-task association, not historical tasks.
// Repository, run and commit are mandatory even when logs have expired or the
// event is not supported by the narrower log/step binding contract.
func (h *Handler) serveJobs(w http.ResponseWriter, r *http.Request) {
	peer, ok := r.Context().Value(socketPeerContextKey{}).(string)
	if r.Method != http.MethodGet || !ok || !h.sourceAllowed(peer) || !h.authorized(r.Header.Get("Authorization")) {
		h.fail(w, 401, "unauthorized", "Provider bridge authentication failed")
		return
	}
	owner, repo := chi.URLParam(r, "owner"), chi.URLParam(r, "repo")
	run, err := strconv.ParseInt(chi.URLParam(r, "run"), 10, 64)
	sha := r.URL.Query().Get("head_sha")
	if err != nil || run <= 0 || !safePart(owner) || !safePart(repo) || !canonicalSHA(sha) || len(r.URL.Query()["head_sha"]) != 1 {
		h.fail(w, 404, "not_found", "Provider run not found")
		return
	}
	var runs []struct{ Number, RepoID int64 }
	err = h.db.WithContext(r.Context()).Raw(`SELECT ar."index" AS number, ar.repo_id FROM action_run ar JOIN repository r ON r.id=ar.repo_id WHERE ar.id=? AND ar.commit_sha=? AND lower(r.owner_name)=lower(?) AND lower(r.lower_name)=lower(?)`, run, sha, owner, repo).Scan(&runs).Error
	if err != nil {
		h.fail(w, 502, "database_unavailable", "Provider metadata unavailable")
		return
	}
	if len(runs) != 1 {
		h.fail(w, 404, "not_found", "Provider run not found")
		return
	}
	var rows []struct {
		JobRepoID                   int64
		TaskID                      *int64
		ActualTaskID, TaskRepoID    *int64
		Name, JobSHA                string
		TaskSHA                     *string
		JobStatus, Started, Stopped *int64
		Attempt                     *int64
	}
	// Enumerate by run before validating repository bindings: filtering invalid
	// bindings in a JOIN would silently turn corrupt metadata into missing jobs.
	err = h.db.WithContext(r.Context()).Raw(`SELECT arj.task_id, arj.repo_id AS job_repo_id, arj.name, arj.commit_sha AS job_sha, arj.status AS job_status, arj.started, arj.stopped, arj.attempt, at.id AS actual_task_id, at.repo_id AS task_repo_id, at.commit_sha AS task_sha FROM action_run_job arj LEFT JOIN action_task at ON at.id=arj.task_id WHERE arj.run_id=? ORDER BY arj.id LIMIT 1001`, run).Scan(&rows).Error
	if err != nil || len(rows) > 1000 {
		h.fail(w, 502, "metadata_unavailable", "Provider job inventory unavailable")
		return
	}
	items := make([]map[string]any, 0, len(rows))
	seen := map[int64]bool{}
	for _, row := range rows {
		if row.JobRepoID != runs[0].RepoID || row.Name == "" || row.JobSHA != sha || row.TaskID == nil {
			h.fail(w, 502, "metadata_invalid", "Provider job inventory is not fully bound")
			return
		}
		taskID := *row.TaskID
		if taskID == 0 && unstartedAttemptWithoutTask(row.JobStatus, row.Started, row.Stopped, row.Attempt) &&
			row.ActualTaskID == nil && row.TaskRepoID == nil && row.TaskSHA == nil {
			continue
		}
		if row.TaskRepoID == nil || *row.TaskRepoID != runs[0].RepoID || taskID <= 0 || row.TaskSHA == nil || *row.TaskSHA != sha || seen[taskID] {
			h.fail(w, 502, "metadata_invalid", "Provider job inventory is not fully bound")
			return
		}
		seen[taskID] = true
		items = append(items, map[string]any{"task_id": taskID, "name": row.Name})
	}
	h.write(w, 200, map[string]any{"schema": "ags.internal-provider-jobs.v1", "repo": owner + "/" + repo, "run": strconv.FormatInt(run, 10), "run_number": runs[0].Number, "head_sha": sha, "jobs": items})
}

// Forgejo job states (models/actions/status.go):
// 0 Unknown, 1 Success, 2 Failure, 3 Cancelled, 4 Skipped,
// 5 Waiting, 6 Running, 7 Blocked.
const (
	forgejoStatusCancelled = 3
	forgejoStatusSkipped   = 4
	forgejoStatusWaiting   = 5
	forgejoStatusBlocked   = 7
)

// unstartedAttemptWithoutTask reports a current attempt that has no task.
// Waiting / Blocked jobs have not been claimed by a runner; Skipped and
// Cancelled jobs can finish without being claimed. All keep started=0.
// Cancellation may stamp stopped; other unstarted states keep stopped=0.
//
// Forgejo 15 prepares both the initial attempt and reruns with
// ActionRunJob.PrepareNextAttempt: it increments Attempt while resetting
// TaskID, Started and Stopped. Attempt is a prepared counter, not proof of
// execution. Accept non-negative counters, including legacy attempt=0, while
// requiring the repository/task bindings above. A positive task ID without its
// task, a started attempt, or an invalid waiting/cancelled counter fails closed.
//
// These rows contribute no task or log identity. An entirely unbound run is
// represented by its provider run status, queued with no conclusion while waiting.
func unstartedAttemptWithoutTask(status, started, stopped, attempt *int64) bool {
	if status == nil || started == nil || stopped == nil || *started != 0 {
		return false
	}
	switch *status {
	case forgejoStatusSkipped:
		return *stopped == 0
	case forgejoStatusCancelled:
		return *stopped >= 0 && attempt != nil && *attempt >= 0
	case forgejoStatusWaiting, forgejoStatusBlocked:
		return *stopped == 0 && attempt != nil && *attempt >= 0
	}
	return false
}
