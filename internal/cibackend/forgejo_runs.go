package cibackend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type forgejoRunPage struct {
	Schema string       `json:"schema"`
	Repo   string       `json:"repo"`
	Total  int          `json:"total_count"`
	Items  []forgejoRun `json:"workflow_runs"`
}

func (f *Forgejo) queryRuns(ctx context.Context, repo string, q Query) (Runs, error) {
	if q.Page == 0 {
		q.Page = 1
	}
	if q.PerPage == 0 {
		q.PerPage = 30
	}
	if !ValidRepository(repo) || q.Page < 1 || q.Page > 1000 || q.PerPage < 1 || q.PerPage > 100 {
		return Runs{}, ErrInvalid
	}
	// New bridges can perform a single indexed, ordered query. During a rolling
	// upgrade an absent resource is the only reason to use the old public API.
	if f.config.LogBridge != nil {
		found, err := f.indexedRuns(ctx, repo, q)
		if err == nil {
			return found, nil
		}
		if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrUnsupported) {
			return Runs{}, err
		}
	}
	items, err := f.observeRuns(ctx, repo, q)
	if err != nil {
		return Runs{}, err
	}
	filtered := make([]Run, 0, len(items))
	for _, run := range items {
		if MatchesRunQuery(run, q) {
			filtered = append(filtered, run)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Number > filtered[j].Number })
	total := len(filtered)
	start := min((q.Page-1)*q.PerPage, total)
	end := min(start+q.PerPage, total)
	return Runs{Items: filtered[start:end], Total: total, Complete: end == total}, nil
}

// runPage is the provider page, not the page returned by the GitHub-shaped API.
// Keeping these layers separate also avoids a full inventory scan for each page
// in the old single-run lookup fallback.
func (f *Forgejo) runPage(ctx context.Context, repo string, query url.Values) (Runs, error) {
	prefix, err := f.prefix(repo)
	if err != nil {
		return Runs{}, err
	}
	data, err := f.request(ctx, http.MethodGet, prefix+"runs?"+query.Encode(), false)
	if err != nil {
		return Runs{}, err
	}
	var body forgejoRunPage
	if err = decode(data, &body); err != nil {
		return Runs{}, err
	}
	page, _ := strconv.Atoi(query.Get("page"))
	size, _ := strconv.Atoi(query.Get("limit"))
	if page < 1 || size < 1 || body.Total < 0 || body.Items == nil || len(body.Items) > size {
		return Runs{}, ErrInvalid
	}
	result := Runs{Items: make([]Run, 0, len(body.Items)), Total: body.Total, Complete: page*size >= body.Total || len(body.Items) < size}
	for _, item := range body.Items {
		run, err := item.run()
		if err != nil {
			return Runs{}, err
		}
		result.Items = append(result.Items, run)
	}
	return result, nil
}

func (f *Forgejo) observeRuns(ctx context.Context, repo string, q Query) ([]Run, error) {
	limit := f.config.TaskPageLimit
	if limit == 0 {
		limit = 100
	}
	query := url.Values{"limit": {"50"}}
	if q.HeadSHA != "" {
		query.Set("head_sha", q.HeadSHA)
	}
	if q.Event != "" {
		query.Set("event", q.Event)
		if q.Event == "pull_request" {
			query.Add("event", "pull_request_sync")
		}
	}
	// These are Forgejo's documented public filters. Branch and workflow must
	// be applied only after every matching provider page has been observed.
	switch q.Status {
	case "queued":
		query["status"] = []string{"waiting", "blocked"}
	case "in_progress":
		query.Set("status", "running")
	case "completed":
		query["status"] = []string{"success", "failure", "cancelled", "skipped"}
	case "success", "failure", "cancelled", "skipped":
		query.Set("status", q.Status)
	}
	seen := map[string]bool{}
	numbers := map[int64]bool{}
	items := []Run{}
	total := -1
	for page := 1; page <= limit; page++ {
		query.Set("page", strconv.Itoa(page))
		found, err := f.runPage(ctx, repo, query)
		if err != nil {
			return nil, err
		}
		if total >= 0 && total != found.Total {
			return nil, fmt.Errorf("%w: CI run inventory changed during observation", ErrUnavailable)
		}
		total = found.Total
		if total > limit*50 || len(found.Items) != min(50, total-len(items)) {
			return nil, fmt.Errorf("%w: incomplete or oversized CI run inventory", ErrUnavailable)
		}
		for _, run := range found.Items {
			if seen[run.Key] || numbers[run.Number] {
				return nil, fmt.Errorf("%w: repeated CI run identity", ErrUnavailable)
			}
			seen[run.Key], numbers[run.Number] = true, true
			items = append(items, run)
		}
		if len(items) == total {
			return items, nil
		}
	}
	return nil, fmt.Errorf("%w: Forgejo run inventory exceeds bounded observation", ErrUnavailable)
}

func (f *Forgejo) indexedRuns(ctx context.Context, repo string, q Query) (Runs, error) {
	query := url.Values{"page": {strconv.Itoa(q.Page)}, "per_page": {strconv.Itoa(q.PerPage)}}
	for name, value := range map[string]string{"head_sha": q.HeadSHA, "branch": q.Branch, "event": q.Event, "status": q.Status, "workflow": q.Workflow} {
		if value != "" {
			query.Set(name, value)
		}
	}
	endpoint := strings.TrimRight(f.config.LogBridge.URL, "/") + "/api/internal/provider-logs/repos/" + repo + "/runs?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Runs{}, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+f.bridgeToken)
	res, err := f.client.Do(req)
	if err != nil {
		return Runs{}, ErrUnavailable
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return Runs{}, ErrNotFound
	case http.StatusNotImplemented:
		return Runs{}, ErrUnsupported
	case http.StatusUnauthorized, http.StatusForbidden:
		return Runs{}, ErrForbidden
	default:
		return Runs{}, ErrUnavailable
	}
	data, err := bounded(res.Body)
	if err != nil {
		return Runs{}, err
	}
	var body forgejoRunPage
	if err = decode(data, &body); err != nil {
		return Runs{}, err
	}
	if body.Schema != "ags.internal-provider-runs.v1" || body.Repo != repo || body.Total < 0 || body.Items == nil {
		return Runs{}, ErrInvalid
	}
	start := min((q.Page-1)*q.PerPage, body.Total)
	if len(body.Items) != min(q.PerPage, body.Total-start) {
		return Runs{}, ErrInvalid
	}
	out := Runs{Items: make([]Run, 0, len(body.Items)), Total: body.Total, Complete: start+len(body.Items) == body.Total}
	seen := map[string]bool{}
	var previous int64
	for _, item := range body.Items {
		item.URL = f.runWebURL(repo, item.Number)
		run, err := item.run()
		if err != nil || seen[run.Key] || previous > 0 && run.Number >= previous || !MatchesRunQuery(run, q) {
			return Runs{}, ErrInvalid
		}
		previous, seen[run.Key] = run.Number, true
		out.Items = append(out.Items, run)
	}
	return out, nil
}
