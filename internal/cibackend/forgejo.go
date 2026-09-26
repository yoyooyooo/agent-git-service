package cibackend

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Forgejo speaks the real ActionRun / ActionTask API, not a renamed GitHub
// Actions adapter. ActionTask IDs are jobs; ActionRun IDs are runs. The two must
// never be equated. Older Forgejo APIs lack public jobs/log endpoints; an explicit
// authenticated log bridge may supply logs, never an implicit DB/filesystem read.
type Forgejo struct {
	*HTTP
	bridgeToken string
}
type forgejoRun struct {
	ID       int64      `json:"id"`
	Number   int64      `json:"index_in_repo"`
	Workflow string     `json:"workflow_id"`
	Title    string     `json:"title"`
	Ref      string     `json:"prettyref"`
	Head     string     `json:"commit_sha"`
	Event    string     `json:"event"`
	Status   string     `json:"status"`
	URL      string     `json:"html_url"`
	Created  time.Time  `json:"created"`
	Updated  time.Time  `json:"updated"`
	Started  *time.Time `json:"started"`
	Stopped  *time.Time `json:"stopped"`
}

func (r forgejoRun) run() (Run, error) {
	status, conclusion, e := normalized(r.Status, "")
	if e != nil || r.ID <= 0 || r.Number <= 0 || r.Workflow == "" || len(r.Head) != 40 || r.Created.IsZero() || r.Updated.IsZero() || !safeURL(r.URL) {
		return Run{}, ErrInvalid
	}
	branch := strings.TrimPrefix(r.Ref, "refs/heads/")
	prs := []int{}
	if strings.HasPrefix(branch, "#") {
		n, e := strconv.Atoi(strings.TrimPrefix(branch, "#"))
		if e == nil && n > 0 {
			prs = append(prs, n)
		}
	}
	// Forgejo creates another run/task identity for retries; it does not expose
	// GitHub's run_attempt counter. One concrete run is represented as attempt 1.
	return Run{Key: strconv.FormatInt(r.ID, 10), Number: r.Number, Workflow: r.Workflow, Name: r.Workflow, Branch: branch, HeadSHA: r.Head, Event: r.Event, Status: status, Conclusion: conclusion, URL: r.URL, CreatedAt: r.Created, UpdatedAt: r.Updated, StartedAt: r.Started, Attempt: 1, PullRequests: prs}, nil
}
func (f *Forgejo) Runs(ctx context.Context, repo string, q Query) (Runs, error) {
	prefix, e := f.prefix(repo)
	if e != nil {
		return Runs{}, e
	}
	if q.Page == 0 {
		q.Page = 1
	}
	if q.PerPage == 0 || q.PerPage > 50 {
		q.PerPage = 50
	}
	if q.Page < 1 || q.Page > 1000 || q.PerPage < 1 {
		return Runs{}, ErrInvalid
	}
	v := url.Values{"page": {strconv.Itoa(q.Page)}, "limit": {strconv.Itoa(q.PerPage)}}
	if q.HeadSHA != "" {
		v.Set("head_sha", q.HeadSHA)
	}
	if q.Event != "" {
		v.Set("event", q.Event)
	}
	data, e := f.request(ctx, "GET", prefix+"runs?"+v.Encode(), false)
	if e != nil {
		return Runs{}, e
	}
	var body struct {
		Total int          `json:"total_count"`
		Items []forgejoRun `json:"workflow_runs"`
	}
	if e = decode(data, &body); e != nil {
		return Runs{}, e
	}
	if body.Total < 0 || len(body.Items) > q.PerPage {
		return Runs{}, ErrInvalid
	}
	result := Runs{Items: []Run{}, Total: body.Total, Complete: body.Total > 0 && q.Page*q.PerPage >= body.Total || len(body.Items) < q.PerPage}
	for _, item := range body.Items {
		r, e := item.run()
		if e != nil {
			return Runs{}, e
		}
		if q.Workflow != "" && r.Workflow != q.Workflow {
			continue
		}
		result.Items = append(result.Items, r)
	}
	return result, nil
}
func (f *Forgejo) Run(ctx context.Context, repo, id string) (Run, error) {
	prefix, e := f.prefix(repo)
	if e != nil {
		return Run{}, e
	}
	n, e := strconv.ParseInt(id, 10, 64)
	if e != nil || n <= 0 {
		return Run{}, ErrInvalid
	}
	data, e := f.request(ctx, "GET", prefix+"runs/"+id, false)
	if e != nil {
		return Run{}, e
	}
	var raw forgejoRun
	if e = decode(data, &raw); e != nil {
		return Run{}, e
	}
	r, e := raw.run()
	if e == nil && r.Key != id {
		return Run{}, ErrInvalid
	}
	return r, e
}

type forgejoTask struct {
	ID        int64      `json:"id"`
	RunNumber int64      `json:"run_number"`
	Name      string     `json:"name"`
	Workflow  string     `json:"workflow_id"`
	Branch    string     `json:"head_branch"`
	Head      string     `json:"head_sha"`
	Status    string     `json:"status"`
	URL       string     `json:"url"`
	Created   time.Time  `json:"created_at"`
	Updated   time.Time  `json:"updated_at"`
	Started   *time.Time `json:"run_started_at"`
}

func (f *Forgejo) Jobs(ctx context.Context, repo, id string) (Jobs, error) {
	run, e := f.Run(ctx, repo, id)
	if e != nil {
		return Jobs{}, e
	}
	prefix, e := f.prefix(repo)
	if e != nil {
		return Jobs{}, e
	}
	result := Jobs{Items: []Job{}}
	limit := f.config.TaskPageLimit
	if limit == 0 {
		limit = 100
	}
	seen := map[int64]bool{}
	expectedTotal := -1
	// The native task listing has no run filter. Exhaust its bounded pages before
	// claiming job completeness; a truncated or moving inventory is never empty
	// success. The explicit work budget can be tuned without changing CI identity.
	for page := 1; page <= limit; page++ {
		data, e := f.request(ctx, "GET", fmt.Sprintf("%stasks?page=%d&limit=50", prefix, page), false)
		if e != nil {
			return Jobs{}, e
		}
		var body struct {
			Total int           `json:"total_count"`
			Items []forgejoTask `json:"workflow_runs"`
		}
		if e = decode(data, &body); e != nil {
			return Jobs{}, e
		}
		if len(body.Items) > 50 || body.Total < 0 {
			return Jobs{}, ErrInvalid
		}
		if expectedTotal < 0 {
			expectedTotal = body.Total
		} else if expectedTotal != body.Total {
			return Jobs{}, fmt.Errorf("%w: CI task inventory changed during observation", ErrUnavailable)
		}
		for _, task := range body.Items {
			if task.ID <= 0 || seen[task.ID] {
				return Jobs{}, fmt.Errorf("%w: repeated task in paged CI inventory", ErrUnavailable)
			}
			seen[task.ID] = true
			if task.RunNumber != run.Number {
				continue
			}
			if task.ID <= 0 || task.Head != run.HeadSHA || task.Workflow != run.Workflow || task.Name == "" || task.Created.IsZero() || !safeURL(task.URL) {
				return Jobs{}, ErrInvalid
			}
			status, conclusion, e := normalized(task.Status, "")
			if e != nil {
				return Jobs{}, e
			}
			var stopped *time.Time
			if status == "completed" {
				t := task.Updated
				stopped = &t
			}
			result.Items = append(result.Items, Job{Key: id + ":" + strconv.FormatInt(task.ID, 10), Run: id, Name: task.Name, HeadSHA: run.HeadSHA, Status: status, Conclusion: conclusion, URL: task.URL, StartedAt: task.Started, CompletedAt: stopped, Steps: []Step{}})
		}
		if len(body.Items) < 50 || body.Total > 0 && page*50 >= body.Total {
			if len(seen) != expectedTotal {
				return Jobs{}, fmt.Errorf("%w: incomplete CI task inventory", ErrUnavailable)
			}
			result.Complete = true
			result.Total = len(result.Items)
			return result, nil
		}
	}
	return Jobs{}, fmt.Errorf("%w: Forgejo task inventory exceeds bounded observation", ErrUnavailable)
}
func forgejoJobParts(id string) (string, string, error) {
	parts := strings.Split(id, ":")
	if len(parts) != 2 {
		return "", "", ErrInvalid
	}
	for _, p := range parts {
		n, e := strconv.ParseInt(p, 10, 64)
		if e != nil || n <= 0 {
			return "", "", ErrInvalid
		}
	}
	return parts[0], parts[1], nil
}
func (f *Forgejo) Job(ctx context.Context, repo, id string) (Job, error) {
	run, _, e := forgejoJobParts(id)
	if e != nil {
		return Job{}, e
	}
	jobs, e := f.Jobs(ctx, repo, run)
	if e != nil {
		return Job{}, e
	}
	for _, j := range jobs.Items {
		if j.Key == id {
			return j, nil
		}
	}
	return Job{}, ErrNotFound
}
func (f *Forgejo) Workflows(context.Context, string, int, int) (Workflows, error) {
	return Workflows{}, ErrUnsupported
}
func (f *Forgejo) Workflow(ctx context.Context, repo, id string) (Workflow, error) {
	// The API exposes filenames in run observations but no workflow resource.
	// The service may return an explicitly labelled prior observed identity;
	// this adapter must not fabricate a workflow from an arbitrary filename.
	return Workflow{}, ErrUnsupported
}
func (f *Forgejo) Action(context.Context, string, string, string) error { return ErrUnsupported }
func (f *Forgejo) JobLogs(ctx context.Context, repo, id string) ([]byte, error) {
	if f.config.LogBridge == nil {
		return nil, fmt.Errorf("%w: Forgejo log bridge is not configured", ErrUnsupported)
	}
	runID, taskID, e := forgejoJobParts(id)
	if e != nil {
		return nil, e
	}
	job, e := f.Job(ctx, repo, id)
	if e != nil {
		return nil, e
	}
	run, e := f.Run(ctx, repo, runID)
	if e != nil {
		return nil, e
	}
	if job.HeadSHA != run.HeadSHA {
		return nil, ErrInvalid
	}
	query := url.Values{"provider_pr": {"0"}, "head_sha": {run.HeadSHA}}
	if len(run.PullRequests) == 1 {
		query.Set("provider_pr", strconv.Itoa(run.PullRequests[0]))
		query.Set("head_ref", "")
	} else {
		query.Set("head_ref", run.Branch)
	}
	parts := strings.Split(repo, "/")
	endpoint := strings.TrimRight(f.config.LogBridge.URL, "/") + "/api/internal/provider-logs/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/tasks/" + taskID + "?" + query.Encode()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if e != nil {
		return nil, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+f.bridgeToken)
	response, e := f.client.Do(req)
	if e != nil {
		return nil, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, ErrUnavailable
	}
	body, e := bounded(response.Body)
	if e != nil {
		return nil, e
	}
	var log struct {
		Schema      string `json:"schema"`
		Repo        string `json:"repo"`
		TaskID      int64  `json:"task_id"`
		RunNumber   int64  `json:"run_number"`
		JobName     string `json:"job_name"`
		Head        string `json:"head_sha"`
		ProviderPR  int    `json:"provider_pr"`
		ProviderRef string `json:"provider_ref"`
		Event       string `json:"event"`
		Text        string `json:"text"`
	}
	if e = decode(body, &log); e != nil {
		return nil, e
	}
	task, _ := strconv.ParseInt(taskID, 10, 64)
	expectedRef := "refs/heads/" + run.Branch
	pr := 0
	if len(run.PullRequests) == 1 {
		pr = run.PullRequests[0]
		expectedRef = fmt.Sprintf("refs/pull/%d/head", pr)
	}
	if log.Schema != "ags.internal-provider-log.v1" || log.Repo != repo || log.TaskID != task || log.RunNumber != run.Number || log.Head != run.HeadSHA || log.JobName != job.Name || log.ProviderPR != pr || log.ProviderRef != expectedRef || log.Event != run.Event {
		return nil, ErrInvalid
	}
	return []byte(log.Text), nil
}
func (f *Forgejo) RunLogs(ctx context.Context, repo, id string) ([]byte, error) {
	jobs, e := f.Jobs(ctx, repo, id)
	if e != nil {
		return nil, e
	}
	var b bytes.Buffer
	writer := zip.NewWriter(&b)
	total := 0
	for index, j := range jobs.Items {
		body, e := f.JobLogs(ctx, repo, j.Key)
		if e != nil {
			return nil, e
		}
		total += len(body)
		if total > MaxBody {
			return nil, ErrUnavailable
		}
		// A whole-job segment is honest about missing step metadata. gh may present
		// UNKNOWN STEP; it must never fabricate successful steps or drop the log.
		name := strings.Map(func(r rune) rune {
			if r == '/' || r == ':' {
				return -1
			}
			if r == '\\' || r < 32 {
				return '_'
			}
			return r
		}, j.Name)
		// Official gh recognizes whole-job logs as top-level <ordinal>_<job>.txt.
		entry, e := writer.Create(strconv.Itoa(index) + "_" + name + ".txt")
		if e != nil {
			return nil, e
		}
		if _, e = entry.Write(body); e != nil {
			return nil, e
		}
	}
	if e = writer.Close(); e != nil {
		return nil, e
	}
	return b.Bytes(), nil
}

var _ Backend = (*Forgejo)(nil)
