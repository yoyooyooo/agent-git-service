package cibackend

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// currentJobs observes the run's current job-to-task associations. The public
// task collection also contains previous attempts, which are not current jobs.
func (f *Forgejo) currentJobs(ctx context.Context, repo string, run Run) (map[int64]string, error) {
	if f.config.LogBridge == nil {
		return nil, ErrUnsupported
	}
	if !ValidRepository(repo) || len(run.HeadSHA) != 40 {
		return nil, ErrInvalid
	}
	n, err := strconv.ParseInt(run.Key, 10, 64)
	if err != nil || n <= 0 {
		return nil, ErrInvalid
	}
	query := url.Values{"head_sha": {run.HeadSHA}, "run_number": {strconv.FormatInt(run.Number, 10)}}
	endpoint := strings.TrimRight(f.config.LogBridge.URL, "/") + "/api/internal/provider-logs/repos/" + repo + "/runs/" + run.Key + "/jobs?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+f.bridgeToken)
	response, err := f.client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrForbidden
	case http.StatusConflict, http.StatusGone:
		return nil, ErrConflict
	case http.StatusNotImplemented:
		return nil, ErrUnsupported
	default:
		return nil, ErrUnavailable
	}
	data, err := bounded(response.Body)
	if err != nil {
		return nil, err
	}
	var body struct {
		Schema, Repo, Run string
		Number            int64  `json:"run_number"`
		HeadSHA           string `json:"head_sha"`
		Jobs              []struct {
			TaskID int64  `json:"task_id"`
			Name   string `json:"name"`
		}
	}
	if err := decode(data, &body); err != nil {
		return nil, err
	}
	if body.Schema != "ags.internal-provider-jobs.v1" || body.Repo != repo || body.Run != run.Key || body.Number != run.Number || body.HeadSHA != run.HeadSHA || body.Jobs == nil || len(body.Jobs) > 1000 {
		return nil, ErrInvalid
	}
	items := map[int64]string{}
	for _, job := range body.Jobs {
		if job.TaskID <= 0 || job.Name == "" {
			return nil, ErrInvalid
		}
		if _, exists := items[job.TaskID]; exists {
			return nil, ErrInvalid
		}
		items[job.TaskID] = job.Name
	}
	return items, nil
}
