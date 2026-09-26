package cibackend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const MaxBody = 8 << 20

// HTTP implements the actual public Actions APIs; no provider database or
// filesystem side channel. Unsupported endpoints remain explicitly unsupported.
type HTTP struct {
	config BackendConfig
	token  string
	client *http.Client
}

func NewHTTP(config BackendConfig, token string, bridgeTokens ...string) (Backend, error) {
	if err := Validate(Config{Backends: map[string]BackendConfig{"selected": config}}); err != nil {
		return nil, err
	}
	client := &HTTP{config: config, token: token, client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if config.Kind == "forgejo" {
		token := ""
		if len(bridgeTokens) > 0 {
			token = bridgeTokens[0]
		}
		if config.LogBridge != nil && token == "" {
			return nil, fmt.Errorf("explicit log bridge credential missing")
		}
		return &Forgejo{HTTP: client, bridgeToken: token}, nil
	}
	return client, nil
}
func (h *HTTP) prefix(repo string) (string, error) {
	if !ValidRepository(repo) {
		return "", ErrInvalid
	}
	parts := strings.Split(repo, "/")
	return "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/actions/", nil
}
func key(value string) (string, error) {
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "/\\?#\r\n\x00") || value == "." || value == ".." {
		return "", ErrInvalid
	}
	return url.PathEscape(value), nil
}
func (h *HTTP) request(ctx context.Context, method, path string, download bool) ([]byte, error) {
	base := strings.TrimRight(h.config.URL, "/")
	api := "/api/v1"
	if h.config.Kind == "github-actions" {
		api = ""
	}
	req, err := http.NewRequestWithContext(ctx, method, base+api+path, nil)
	if err != nil {
		return nil, ErrInvalid
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ags-ci-backend/1")
	if h.config.Kind == "forgejo" {
		req.Header.Set("Authorization", "token "+h.token)
	} else {
		req.Header.Set("Authorization", "Bearer "+h.token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	response, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: transport did not confirm %s", ErrUnavailable, method)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 && download && method == http.MethodGet {
		location, e := url.Parse(response.Header.Get("Location"))
		allowed := false
		if e == nil && location.Scheme == "https" && location.User == nil && location.Hostname() != "" && location.Fragment == "" && (location.Port() == "" || location.Port() == "443") {
			for _, host := range h.config.LogDownloadHosts {
				if strings.EqualFold(host, location.Hostname()) {
					allowed = true
				}
			}
		}
		if !allowed {
			return nil, fmt.Errorf("%w: log redirect destination is not configured", ErrUnsupported)
		}
		// One explicit GET to an allowed signed URL; never forward provider auth,
		// cookies or an AGS user credential, and never follow another redirect.
		next, _ := http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
		other, e := h.client.Do(next)
		if e != nil {
			return nil, ErrUnavailable
		}
		defer other.Body.Close()
		if other.StatusCode != 200 {
			return nil, ErrUnavailable
		}
		return bounded(other.Body)
	}
	switch response.StatusCode {
	case 200, 201, 202, 204:
		return bounded(response.Body)
	case 404:
		return nil, ErrNotFound
	case 405, 501:
		return nil, ErrUnsupported
	default:
		return nil, fmt.Errorf("%w: HTTP %d", ErrUnavailable, response.StatusCode)
	}
}
func bounded(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxBody+1))
	if err != nil || len(b) > MaxBody {
		return nil, ErrUnavailable
	}
	return b, nil
}
func decode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(v); err != nil {
		return ErrInvalid
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ErrInvalid
	}
	return nil
}

type wireRun struct {
	ID           int64           `json:"id"`
	Name         string          `json:"name"`
	DisplayTitle string          `json:"display_title"`
	WorkflowID   json.RawMessage `json:"workflow_id"`
	HeadBranch   string          `json:"head_branch"`
	HeadSHA      string          `json:"head_sha"`
	Event        string          `json:"event"`
	Status       string          `json:"status"`
	Conclusion   string          `json:"conclusion"`
	HTMLURL      string          `json:"html_url"`
	RunNumber    int64           `json:"run_number"`
	RunAttempt   int             `json:"run_attempt"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	RunStartedAt *time.Time      `json:"run_started_at"`
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

func normalized(status, conclusion string) (string, string, error) {
	switch strings.ToLower(status) {
	case "queued", "waiting", "pending", "requested", "blocked":
		return "queued", "", nil
	case "running", "in_progress":
		return "in_progress", "", nil
	case "success", "failure", "cancelled", "skipped", "neutral", "timed_out", "action_required", "stale":
		return "completed", strings.ToLower(status), nil
	case "completed":
		switch conclusion {
		case "success", "failure", "cancelled", "skipped", "neutral", "timed_out", "action_required", "stale":
			return "completed", conclusion, nil
		}
	}
	return "", "", ErrInvalid
}
func (w wireRun) run() (Run, error) {
	s, c, e := normalized(w.Status, w.Conclusion)
	if e != nil || w.ID <= 0 || len(w.HeadSHA) != 40 || w.CreatedAt.IsZero() || w.UpdatedAt.IsZero() {
		return Run{}, ErrInvalid
	}
	workflow := strings.Trim(string(w.WorkflowID), "\"")
	if workflow == "null" || workflow == "" {
		workflow = "unknown"
	}
	name := w.Name
	if name == "" {
		name = w.DisplayTitle
	}
	if name == "" || len(name) > 256 {
		return Run{}, ErrInvalid
	}
	if !safeURL(w.HTMLURL) {
		return Run{}, ErrInvalid
	}
	attempt := w.RunAttempt
	if attempt == 0 {
		attempt = 1
	}
	pulls := make([]int, 0, len(w.PullRequests))
	for _, p := range w.PullRequests {
		if p.Number > 0 {
			pulls = append(pulls, p.Number)
		}
	}
	return Run{Key: strconv.FormatInt(w.ID, 10), Workflow: workflow, Name: name, Branch: w.HeadBranch, HeadSHA: w.HeadSHA, Event: w.Event, Status: s, Conclusion: c, URL: w.HTMLURL, Number: w.RunNumber, Attempt: attempt, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, StartedAt: w.RunStartedAt, PullRequests: pulls}, nil
}
func safeURL(raw string) bool {
	if raw == "" {
		return true
	}
	u, e := url.Parse(raw)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func (h *HTTP) Runs(ctx context.Context, repo string, q Query) (Runs, error) {
	p, e := h.prefix(repo)
	if e != nil {
		return Runs{}, e
	}
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PerPage < 1 {
		q.PerPage = 30
	}
	if q.PerPage > 100 || q.Page > 1000 {
		return Runs{}, ErrInvalid
	}
	v := url.Values{"page": {strconv.Itoa(q.Page)}, "per_page": {strconv.Itoa(q.PerPage)}}
	for k, s := range map[string]string{"head_sha": q.HeadSHA, "branch": q.Branch, "event": q.Event, "status": q.Status} {
		if s != "" {
			v.Set(k, s)
		}
	}
	path := p + "runs"
	if q.Workflow != "" {
		id, e := key(q.Workflow)
		if e != nil {
			return Runs{}, e
		}
		path = p + "workflows/" + id + "/runs"
	}
	b, e := h.request(ctx, "GET", path+"?"+v.Encode(), false)
	if e != nil {
		return Runs{}, e
	}
	var d struct {
		Total int       `json:"total_count"`
		Runs  []wireRun `json:"workflow_runs"`
	}
	if e = decode(b, &d); e != nil {
		return Runs{}, e
	}
	if len(d.Runs) > q.PerPage || d.Total < 0 {
		return Runs{}, ErrInvalid
	}
	out := Runs{Items: make([]Run, 0, len(d.Runs)), Total: d.Total, Complete: len(d.Runs) < q.PerPage || q.Page*q.PerPage >= d.Total && d.Total > 0}
	for _, w := range d.Runs {
		r, e := w.run()
		if e != nil {
			return Runs{}, e
		}
		out.Items = append(out.Items, r)
	}
	return out, nil
}
func (h *HTTP) Run(ctx context.Context, repo, id string) (Run, error) {
	p, e := h.prefix(repo)
	if e != nil {
		return Run{}, e
	}
	id, e = key(id)
	if e != nil {
		return Run{}, e
	}
	b, e := h.request(ctx, "GET", p+"runs/"+id, false)
	if e != nil {
		return Run{}, e
	}
	var w wireRun
	if e = decode(b, &w); e != nil {
		return Run{}, e
	}
	r, e := w.run()
	if e == nil && url.PathEscape(r.Key) != id {
		return Run{}, ErrInvalid
	}
	return r, e
}

type wireJob struct {
	ID          int64      `json:"id"`
	RunID       int64      `json:"run_id"`
	Name        string     `json:"name"`
	HeadSHA     string     `json:"head_sha"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	HTMLURL     string     `json:"html_url"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	Steps       []Step     `json:"steps"`
}

func (w wireJob) job() (Job, error) {
	s, c, e := normalized(w.Status, w.Conclusion)
	if e != nil || w.ID <= 0 || w.RunID <= 0 || w.Name == "" || len(w.Name) > 256 || !safeURL(w.HTMLURL) {
		return Job{}, ErrInvalid
	}
	for i, step := range w.Steps {
		ss, cc, e := normalized(step.Status, step.Conclusion)
		if e != nil {
			return Job{}, e
		}
		w.Steps[i].Status = ss
		w.Steps[i].Conclusion = cc
	}
	return Job{Key: strconv.FormatInt(w.ID, 10), Run: strconv.FormatInt(w.RunID, 10), Name: w.Name, HeadSHA: w.HeadSHA, Status: s, Conclusion: c, URL: w.HTMLURL, StartedAt: w.StartedAt, CompletedAt: w.CompletedAt, Steps: w.Steps}, nil
}
func (h *HTTP) Jobs(ctx context.Context, repo, run string) (Jobs, error) {
	p, e := h.prefix(repo)
	if e != nil {
		return Jobs{}, e
	}
	id, e := key(run)
	if e != nil {
		return Jobs{}, e
	}
	out := Jobs{Items: []Job{}}
	for page := 1; page <= 10; page++ {
		b, e := h.request(ctx, "GET", p+"runs/"+id+"/jobs?per_page=100&page="+strconv.Itoa(page), false)
		if e != nil {
			return Jobs{}, e
		}
		var d struct {
			Total int       `json:"total_count"`
			Jobs  []wireJob `json:"jobs"`
		}
		if e = decode(b, &d); e != nil {
			return Jobs{}, e
		}
		if len(d.Jobs) > 100 {
			return Jobs{}, ErrInvalid
		}
		for _, w := range d.Jobs {
			j, e := w.job()
			if e != nil || j.Run != run {
				return Jobs{}, ErrInvalid
			}
			out.Items = append(out.Items, j)
		}
		out.Total = d.Total
		if len(d.Jobs) < 100 || d.Total > 0 && len(out.Items) >= d.Total {
			out.Complete = true
			return out, nil
		}
	}
	return Jobs{}, fmt.Errorf("%w: job page bound exceeded", ErrUnavailable)
}
func (h *HTTP) Job(ctx context.Context, repo, id string) (Job, error) {
	p, e := h.prefix(repo)
	if e != nil {
		return Job{}, e
	}
	escaped, e := key(id)
	if e != nil {
		return Job{}, e
	}
	b, e := h.request(ctx, "GET", p+"jobs/"+escaped, false)
	if e != nil {
		return Job{}, e
	}
	var w wireJob
	if e = decode(b, &w); e != nil {
		return Job{}, e
	}
	j, e := w.job()
	if e == nil && j.Key != id {
		return Job{}, ErrInvalid
	}
	return j, e
}
func (h *HTTP) JobLogs(ctx context.Context, repo, id string) ([]byte, error) {
	p, e := h.prefix(repo)
	if e != nil {
		return nil, e
	}
	id, e = key(id)
	if e != nil {
		return nil, e
	}
	return h.request(ctx, "GET", p+"jobs/"+id+"/logs", true)
}
func (h *HTTP) RunLogs(ctx context.Context, repo, id string) ([]byte, error) {
	p, e := h.prefix(repo)
	if e != nil {
		return nil, e
	}
	id, e = key(id)
	if e != nil {
		return nil, e
	}
	return h.request(ctx, "GET", p+"runs/"+id+"/logs", true)
}
func (h *HTTP) Action(ctx context.Context, repo, id, action string) error {
	if action != "cancel" && action != "rerun" && action != "rerun-failed-jobs" {
		return ErrUnsupported
	}
	p, e := h.prefix(repo)
	if e != nil {
		return e
	}
	id, e = key(id)
	if e != nil {
		return e
	}
	_, e = h.request(ctx, "POST", p+"runs/"+id+"/"+action, false)
	return e
}
