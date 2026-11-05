package providerlogbridge

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
)

// runInventoryRow is the small read projection of Forgejo's ActionRun model.
// Event payloads, actors, credentials and storage paths never leave the bridge.
type runInventoryRow struct {
	ID, Number                       int64
	Workflow, Title, Ref, SHA, Event string
	Status                           int
	NeedApproval, IsForkPullRequest  bool
	Created, Updated, Started        int64
}

const runBranchExpression = `CASE WHEN ref LIKE 'refs/heads/%' THEN substr(ref, 12) WHEN ref LIKE 'refs/tags/%' THEN substr(ref, 11) WHEN ref LIKE 'refs/pull/%/head' THEN '#' || substr(ref, 11, length(ref)-15) ELSE ref END`
const runEventExpression = `CASE WHEN event='pull_request_sync' AND ref LIKE 'refs/pull/%/head' THEN 'pull_request' ELSE event END`

func runQuery(q url.Values) (int, int, error) {
	for key, values := range q {
		switch key {
		case "page", "per_page", "branch", "head_sha", "event", "status", "workflow":
		default:
			return 0, 0, errors.New("unsupported run query")
		}
		if len(values) != 1 || len(values[0]) > 1024 || strings.ContainsAny(values[0], "\x00\r\n") {
			return 0, 0, errors.New("invalid run query")
		}
	}
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 || page > 1000 || strconv.Itoa(page) != q.Get("page") {
		return 0, 0, errors.New("invalid run page")
	}
	size, err := strconv.Atoi(q.Get("per_page"))
	if err != nil || size < 1 || size > 100 || strconv.Itoa(size) != q.Get("per_page") {
		return 0, 0, errors.New("invalid run page size")
	}
	if q.Get("head_sha") != "" && !canonicalSHA(q.Get("head_sha")) {
		return 0, 0, errors.New("invalid run commit")
	}
	return page, size, nil
}

func (h *Handler) serveRuns(w http.ResponseWriter, r *http.Request) {
	peer, ok := r.Context().Value(socketPeerContextKey{}).(string)
	if r.Method != http.MethodGet || !ok || !h.sourceAllowed(peer) || !h.authorized(r.Header.Get("Authorization")) {
		h.fail(w, 401, "unauthorized", "Provider bridge authentication failed")
		return
	}
	owner, repo := chi.URLParam(r, "owner"), chi.URLParam(r, "repo")
	q := r.URL.Query()
	page, size, err := runQuery(q)
	if !safePart(owner) || !safePart(repo) || err != nil {
		h.fail(w, 422, "invalid_query", "Invalid provider run query")
		return
	}
	var rows []runInventoryRow
	var total int64
	missing := errors.New("missing repository")
	err = h.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		var repositories []struct{ ID int64 }
		if err := tx.Raw(`SELECT id FROM repository WHERE lower(owner_name)=lower(?) AND lower(lower_name)=lower(?)`, owner, repo).Scan(&repositories).Error; err != nil {
			return err
		}
		if len(repositories) != 1 || repositories[0].ID <= 0 {
			return missing
		}
		query := tx.Table("action_run").Where("repo_id = ?", repositories[0].ID)
		if sha := q.Get("head_sha"); sha != "" {
			query = query.Where("commit_sha = ?", sha)
		}
		if workflow := q.Get("workflow"); workflow != "" {
			query = query.Where("workflow_id = ?", workflow)
		}
		if branch := q.Get("branch"); branch != "" {
			query = query.Where("("+runBranchExpression+") = ?", branch)
		}
		if event := q.Get("event"); event != "" {
			query = query.Where("("+runEventExpression+") = ?", event)
		}
		if status := q.Get("status"); status != "" {
			statuses := map[string][]int{"queued": {5, 7}, "in_progress": {6}, "completed": {1, 2, 3, 4}, "success": {1}, "failure": {2}, "cancelled": {3}, "skipped": {4}}
			ids := statuses[status]
			if len(ids) == 0 {
				query = query.Where("1=0")
			} else {
				query = query.Where("status IN ?", ids)
			}
		}
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		return query.Select(`id, "index" AS number, workflow_id AS workflow, title, ref, commit_sha AS sha, event, status, need_approval, is_fork_pull_request, created, updated, started`).Order(`"index" DESC`).Order("id DESC").Limit(size).Offset((page - 1) * size).Scan(&rows).Error
	}, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if errors.Is(err, missing) {
		h.fail(w, 404, "not_found", "Provider repository not found")
		return
	}
	if err != nil {
		h.fail(w, 502, "metadata_unavailable", "Provider run inventory unavailable")
		return
	}
	items := make([]map[string]any, 0, len(rows))
	var previous int64
	seen := map[int64]bool{}
	statuses := map[int]string{1: "success", 2: "failure", 3: "cancelled", 4: "skipped", 5: "waiting", 6: "running", 7: "blocked"}
	for _, row := range rows {
		status := statuses[row.Status]
		if row.ID <= 0 || row.Number <= 0 || seen[row.ID] || previous > 0 && row.Number >= previous || !canonicalSHA(row.SHA) || row.Workflow == "" || row.Created <= 0 || row.Updated <= 0 || status == "" {
			h.fail(w, 502, "metadata_invalid", "Invalid provider run inventory")
			return
		}
		seen[row.ID], previous = true, row.Number
		ref := row.Ref
		if branch, head := strings.CutPrefix(row.Ref, "refs/heads/"); head {
			ref = branch
		} else if tag, tagged := strings.CutPrefix(row.Ref, "refs/tags/"); tagged {
			ref = tag
		} else if number, pull := strings.CutPrefix(row.Ref, "refs/pull/"); pull && strings.HasSuffix(number, "/head") {
			n, err := strconv.ParseInt(strings.TrimSuffix(number, "/head"), 10, 64)
			if err != nil || n <= 0 || number != strconv.FormatInt(n, 10)+"/head" {
				h.fail(w, 502, "metadata_invalid", "Invalid provider run ref")
				return
			}
			ref = "#" + strconv.FormatInt(n, 10)
		}
		items = append(items, map[string]any{"id": row.ID, "index_in_repo": row.Number, "workflow_id": row.Workflow, "title": row.Title, "prettyref": ref, "commit_sha": row.SHA, "event": row.Event, "status": status, "need_approval": row.NeedApproval, "is_fork_pull_request": row.IsForkPullRequest, "created": time.Unix(row.Created, 0).UTC(), "updated": time.Unix(row.Updated, 0).UTC(), "started": timestamp(row.Started)})
	}
	h.write(w, 200, map[string]any{"schema": "ags.internal-provider-runs.v1", "repo": owner + "/" + repo, "total_count": total, "workflow_runs": items})
}
