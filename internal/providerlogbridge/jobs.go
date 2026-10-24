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
	var runs []struct{ Number int64 }
	err = h.db.WithContext(r.Context()).Raw(`SELECT ar."index" AS number FROM action_run ar JOIN repository r ON r.id=ar.repo_id WHERE ar.id=? AND ar.commit_sha=? AND lower(r.owner_name)=lower(?) AND lower(r.lower_name)=lower(?)`, run, sha, owner, repo).Scan(&runs).Error
	if err != nil {
		h.fail(w, 502, "database_unavailable", "Provider metadata unavailable")
		return
	}
	if len(runs) != 1 {
		h.fail(w, 404, "not_found", "Provider run not found")
		return
	}
	var rows []struct {
		TaskID       int64
		Name, JobSHA string
		TaskSHA      *string
	}
	err = h.db.WithContext(r.Context()).Raw(`SELECT arj.task_id, arj.name, arj.commit_sha AS job_sha, at.commit_sha AS task_sha FROM action_run_job arj JOIN action_run ar ON ar.id=arj.run_id AND ar.repo_id=arj.repo_id LEFT JOIN action_task at ON at.id=arj.task_id AND at.repo_id=arj.repo_id WHERE ar.id=? ORDER BY arj.id LIMIT 1001`, run).Scan(&rows).Error
	if err != nil || len(rows) > 1000 {
		h.fail(w, 502, "metadata_unavailable", "Provider job inventory unavailable")
		return
	}
	items := make([]map[string]any, 0, len(rows))
	seen := map[int64]bool{}
	for _, row := range rows {
		if row.TaskID <= 0 || row.Name == "" || row.JobSHA != sha || row.TaskSHA == nil || *row.TaskSHA != sha || seen[row.TaskID] {
			h.fail(w, 502, "metadata_invalid", "Provider job inventory is not fully bound")
			return
		}
		seen[row.TaskID] = true
		items = append(items, map[string]any{"task_id": row.TaskID, "name": row.Name})
	}
	h.write(w, 200, map[string]any{"schema": "ags.internal-provider-jobs.v1", "repo": owner + "/" + repo, "run": strconv.FormatInt(run, 10), "run_number": runs[0].Number, "head_sha": sha, "jobs": items})
}
