package cibackend

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Artifact struct {
	Key       string     `json:"key"`
	Run       string     `json:"run"`
	Name      string     `json:"name"`
	Size      int64      `json:"size_in_bytes"`
	Expired   bool       `json:"expired"`
	CreatedAt *time.Time `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// Optional capabilities do not require unrelated adapters to fabricate data.
type ArtifactBackend interface {
	Artifacts(context.Context, string, Run) ([]Artifact, error)
	ArtifactDownload(context.Context, string, Run, string) ([]byte, error)
}
type StepBackend interface {
	JobSteps(context.Context, string, Run, Job) ([]Step, error)
}

func (f *Forgejo) JobSteps(ctx context.Context, repo string, run Run, job Job) ([]Step, error) {
	if f.config.LogBridge == nil {
		return []Step{}, nil
	}
	body, err := f.boundJobResource(ctx, repo, run, job, true)
	if err != nil {
		return nil, err
	}
	var steps []Step
	if err = decode(body, &steps); err != nil {
		return nil, err
	}
	if len(steps) > 1000 {
		return nil, ErrInvalid
	}
	previous := 0
	for _, s := range steps {
		if s.Number <= previous || (s.Status != "" && s.Status != "queued" && s.Status != "in_progress" && s.Status != "completed") {
			return nil, ErrInvalid
		}
		previous = s.Number
		if s.Status == "completed" {
			if _, _, err := normalized(s.Status, s.Conclusion); err != nil {
				return nil, err
			}
		} else if s.Conclusion != "" {
			return nil, ErrInvalid
		}
	}
	return steps, nil
}
func (f *Forgejo) artifactRequest(ctx context.Context, repo string, run Run, download string) ([]byte, error) {
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
	query := url.Values{"head_sha": {run.HeadSHA}}
	if download != "" {
		n, err := strconv.ParseInt(download, 10, 64)
		if err != nil || n <= 0 {
			return nil, ErrInvalid
		}
		query.Set("download", download)
	}
	endpoint := strings.TrimRight(f.config.LogBridge.URL, "/") + "/api/internal/provider-logs/repos/" + repo + "/runs/" + run.Key + "/artifacts?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
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
	case 200:
	case 404:
		return nil, ErrNotFound
	case 501:
		return nil, ErrUnsupported
	case 403:
		return nil, ErrForbidden
	default:
		return nil, ErrUnavailable
	}
	return bounded(response.Body)
}
func (f *Forgejo) Artifacts(ctx context.Context, repo string, run Run) ([]Artifact, error) {
	data, err := f.artifactRequest(ctx, repo, run, "")
	if err != nil {
		return nil, err
	}
	var body struct {
		Schema, Repo, Run string
		HeadSHA           string `json:"head_sha"`
		Artifacts         []Artifact
	}
	if err = decode(data, &body); err != nil {
		return nil, err
	}
	if body.Schema != "ags.internal-provider-artifacts.v1" || body.Repo != repo || body.Run != run.Key || body.HeadSHA != run.HeadSHA || body.Artifacts == nil || len(body.Artifacts) > 1000 {
		return nil, ErrInvalid
	}
	seen := map[string]bool{}
	for _, a := range body.Artifacts {
		n, e := strconv.ParseInt(a.Key, 10, 64)
		if e != nil || n <= 0 || a.Run != run.Key || a.Name == "" || a.Size < 0 || seen[a.Key] {
			return nil, ErrInvalid
		}
		seen[a.Key] = true
	}
	return body.Artifacts, nil
}
func (f *Forgejo) ArtifactDownload(ctx context.Context, repo string, run Run, id string) ([]byte, error) {
	data, err := f.artifactRequest(ctx, repo, run, id)
	if err != nil {
		return nil, err
	}
	if err = ValidateLogArchive(data); err != nil {
		return nil, fmt.Errorf("%w: artifact archive", ErrInvalid)
	}
	return data, nil
}
