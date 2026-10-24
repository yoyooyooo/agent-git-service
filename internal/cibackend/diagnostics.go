package cibackend

import (
	"context"
	"errors"
	"net/http"
)

// DiagnosticStatus is a bounded public vocabulary, never provider error text.
func DiagnosticStatus(err error) string {
	switch {
	case errors.Is(err, ErrUnsupported):
		return "unsupported"
	case errors.Is(err, ErrNotFound):
		return "not-found"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrInvalid):
		return "invalid"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "unavailable"
	}
}

func bridgeStatusError(status int) error {
	switch status {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrForbidden
	case http.StatusConflict, http.StatusGone:
		return ErrConflict
	case http.StatusNotImplemented:
		return ErrUnsupported
	default:
		return ErrUnavailable
	}
}

func (f *Forgejo) currentJobs(ctx context.Context, repo string, run Run) (map[int64]string, error) {
	data, err := f.runBridgeRequest(ctx, repo, run, "jobs", "")
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
