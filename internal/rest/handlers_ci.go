package rest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

// CI dispatches one standard Actions route to the explicitly selected backend.
// Native fallback is a configuration choice, never an error-recovery strategy.
func (d *Deps) CI(native http.HandlerFunc, operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := d.mustGetRepo(w, r)
		if repo == nil {
			return
		}
		selected, err := d.Svc.CISelection(repo.FullName)
		if err != nil {
			ciHTTPError(w, err)
			return
		}
		if selected.Name == "native" {
			for _, param := range []string{"run_id", "job_id", "workflow_id"} {
				if value := chi.URLParam(r, param); value != "" {
					n, _ := strconv.ParseUint(value, 10, 64)
					if n >= service.CIExternalIDBase {
						respond.Error(w, 404, "CI resource belongs to a different backend")
						return
					}
				}
			}
			native(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		base := strings.TrimRight(d.Svc.HTMLBaseURL(), "/") + "/api/v3/repos/" + repo.FullName
		readID := func(param string) (uint64, bool) {
			id, e := strconv.ParseUint(chi.URLParam(r, param), 10, 64)
			if e != nil || id == 0 {
				respond.Error(w, 422, "invalid CI resource ID")
				return 0, false
			}
			return id, true
		}
		switch operation {
		case "workflows":
			q := r.URL.Query()
			page, _ := strconv.Atoi(q.Get("page"))
			size, _ := strconv.Atoi(q.Get("per_page"))
			if page == 0 {
				page = 1
			}
			if size == 0 {
				size = 30
			}
			if page < 1 || page > 1000 || size < 1 || size > 100 {
				respond.Error(w, 422, "invalid workflow pagination")
				return
			}
			result, e := d.Svc.CIWorkflows(r.Context(), repo.FullName, page, size)
			if e != nil {
				ciHTTPError(w, e)
				return
			}
			rows := make([]any, 0, len(result.Items))
			for _, workflow := range result.Items {
				rows = append(rows, ciWorkflowJSON(workflow, base))
			}
			if !result.Complete {
				q.Set("page", strconv.Itoa(page+1))
				next := *r.URL
				next.RawQuery = q.Encode()
				w.Header().Set("Link", fmt.Sprintf("<%s%s>; rel=\"next\"", strings.TrimRight(d.Svc.HTMLBaseURL(), "/"), next.String()))
			}
			respond.JSON(w, 200, map[string]any{"total_count": result.Total, "workflows": rows, "ags_catalog_scope": result.Scope})
			return
		case "workflow":
			item, e := d.Svc.CIWorkflow(r.Context(), repo.FullName, chi.URLParam(r, "workflow_id"))
			if e != nil {
				ciHTTPError(w, e)
				return
			}
			respond.JSON(w, 200, ciWorkflowJSON(item, base))
			return
		case "runs":
			q := r.URL.Query()
			page, _ := strconv.Atoi(q.Get("page"))
			size, _ := strconv.Atoi(q.Get("per_page"))
			if page == 0 {
				page = 1
			}
			if size == 0 {
				size = 30
			}
			if page < 1 || page > 1000 || size < 1 || size > 100 {
				respond.Error(w, 422, "invalid CI pagination")
				return
			}
			result, e := d.Svc.CIRuns(r.Context(), repo.FullName, cibackend.Query{HeadSHA: q.Get("head_sha"), Branch: q.Get("branch"), Event: q.Get("event"), Status: q.Get("status"), Workflow: chi.URLParam(r, "workflow_id"), Page: page, PerPage: size})
			if e != nil {
				ciHTTPError(w, e)
				return
			}
			rows := make([]any, 0, len(result.Runs))
			for _, run := range result.Runs {
				rows = append(rows, ciRunJSON(run, base))
			}
			if !result.Complete {
				q.Set("page", strconv.Itoa(page+1))
				next := *r.URL
				next.RawQuery = q.Encode()
				w.Header().Set("Link", fmt.Sprintf("<%s%s>; rel=\"next\"", strings.TrimRight(d.Svc.HTMLBaseURL(), "/"), next.String()))
			}
			respond.JSON(w, 200, map[string]any{"total_count": result.Total, "workflow_runs": rows})
			return
		case "run", "run-jobs", "run-logs", "cancel", "rerun", "rerun-failed-jobs":
			id, ok := readID("run_id")
			if !ok {
				return
			}
			run, e := d.Svc.CIRun(r.Context(), repo.FullName, id)
			if e != nil {
				ciHTTPError(w, e)
				return
			}
			if attempt := chi.URLParam(r, "attempt_number"); attempt != "" && attempt != strconv.Itoa(run.Run.Attempt) {
				respond.Error(w, 404, "Only the selected run attempt is available")
				return
			}
			if operation == "run" {
				respond.JSON(w, 200, ciRunJSON(run, base))
				return
			}
			if operation == "run-jobs" {
				jobs, e := d.Svc.CIJobs(r.Context(), repo.FullName, id)
				if e != nil {
					ciHTTPError(w, e)
					return
				}
				rows := make([]any, 0, len(jobs))
				for _, j := range jobs {
					rows = append(rows, ciJobJSON(j, base))
				}
				respond.JSON(w, 200, map[string]any{"total_count": len(rows), "jobs": rows})
				return
			}
			if operation == "run-logs" {
				data, e := d.Svc.CILogs(r.Context(), repo.FullName, id, false)
				if e != nil {
					ciHTTPError(w, e)
					return
				}
				w.Header().Set("Content-Type", "application/zip")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(200)
				_, _ = w.Write(data)
				return
			}
			if e := d.Svc.CIAction(r.Context(), repo.FullName, id, operation); e != nil {
				ciHTTPError(w, e)
				return
			}
			status := http.StatusCreated
			if operation == "cancel" {
				status = http.StatusAccepted
			}
			respond.JSON(w, status, map[string]any{})
			return
		case "job", "job-logs":
			id, ok := readID("job_id")
			if !ok {
				return
			}
			job, e := d.Svc.CIJob(r.Context(), repo.FullName, id)
			if e != nil {
				ciHTTPError(w, e)
				return
			}
			if operation == "job" {
				respond.JSON(w, 200, ciJobJSON(job, base))
				return
			}
			data, e := d.Svc.CILogs(r.Context(), repo.FullName, id, true)
			if e != nil {
				ciHTTPError(w, e)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(data)
			return
		default:
			respond.Error(w, 501, "Selected CI backend does not expose this Actions operation")
			return
		}
	}
}
func ciHTTPError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, service.ErrUnauthorized):
		status = 401
	case errors.Is(err, service.ErrForbidden):
		status = 403
	case errors.Is(err, service.ErrNotFound), errors.Is(err, cibackend.ErrNotFound):
		status = 404
	case errors.Is(err, cibackend.ErrUnsupported):
		status = 501
	case errors.Is(err, service.ErrValidation):
		status = 422
	}
	respond.Error(w, status, service.CIErrorMessage(err))
}
func ciRunJSON(v service.CIRun, base string) map[string]any {
	r := v.Run
	return map[string]any{
		"id": v.ID, "node_id": base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("CIRun:%d", v.ID))), "name": r.Name, "display_title": r.Name, "head_branch": r.Branch, "head_sha": r.HeadSHA, "path": r.Workflow, "run_number": r.Number, "run_attempt": r.Attempt, "event": r.Event, "status": r.Status, "conclusion": nilIfEmpty(r.Conclusion), "workflow_id": v.WorkflowID, "created_at": r.CreatedAt, "updated_at": r.UpdatedAt, "run_started_at": r.StartedAt, "html_url": r.URL,
		"url": fmt.Sprintf("%s/actions/runs/%d", base, v.ID), "jobs_url": fmt.Sprintf("%s/actions/runs/%d/jobs", base, v.ID), "logs_url": fmt.Sprintf("%s/actions/runs/%d/logs", base, v.ID), "check_suite_id": v.ID, "check_suite_node_id": fmt.Sprintf("CISuite:%d", v.ID), "pull_requests": []any{}, "actor": nil, "triggering_actor": nil, "ags_ci_backend": v.Backend,
	}
}
func ciJobJSON(v service.CIJob, base string) map[string]any {
	j := v.Job
	steps := j.Steps
	if steps == nil {
		steps = []cibackend.Step{}
	}
	return map[string]any{"id": v.ID, "run_id": v.RunID, "name": j.Name, "head_sha": j.HeadSHA, "status": j.Status, "conclusion": nilIfEmpty(j.Conclusion), "html_url": j.URL, "url": fmt.Sprintf("%s/actions/jobs/%d", base, v.ID), "run_url": fmt.Sprintf("%s/actions/runs/%d", base, v.RunID), "started_at": j.StartedAt, "completed_at": j.CompletedAt, "steps": steps, "labels": []string{}, "ags_ci_backend": v.Backend}
}
func ciWorkflowJSON(value service.CIWorkflow, base string) map[string]any {
	w := value.Workflow
	return map[string]any{"id": value.ID, "name": w.Name, "path": w.Path, "state": w.State, "html_url": w.URL, "created_at": w.CreatedAt, "updated_at": w.UpdatedAt, "url": fmt.Sprintf("%s/actions/workflows/%d", base, value.ID), "ags_ci_backend": value.Backend}
}
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// HeadRepo retains the same repository visibility check as GET without forcing
// a JSON body. Stock gh uses HEAD for its browse/discovery path.
func (d *Deps) HeadRepo(w http.ResponseWriter, r *http.Request) {
	if d.mustGetRepo(w, r) == nil {
		return
	}
	w.WriteHeader(http.StatusOK)
}
