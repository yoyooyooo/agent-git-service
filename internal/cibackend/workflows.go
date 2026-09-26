package cibackend

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

type wireWorkflow struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	State     string    `json:"state"`
	URL       string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (w wireWorkflow) workflow() (Workflow, error) {
	if w.ID <= 0 || w.Name == "" || len(w.Name) > 256 || !safeURL(w.URL) {
		return Workflow{}, ErrInvalid
	}
	return Workflow{Key: strconv.FormatInt(w.ID, 10), Name: w.Name, Path: w.Path, State: w.State, URL: w.URL, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}, nil
}
func (h *HTTP) Workflows(ctx context.Context, repo string, page, perPage int) (Workflows, error) {
	p, e := h.prefix(repo)
	if e != nil {
		return Workflows{}, e
	}
	if page < 1 || page > 1000 || perPage < 1 || perPage > 100 {
		return Workflows{}, ErrInvalid
	}
	data, e := h.request(ctx, "GET", fmt.Sprintf("%sworkflows?page=%d&per_page=%d", p, page, perPage), false)
	if e != nil {
		return Workflows{}, e
	}
	var body struct {
		Total int            `json:"total_count"`
		Items []wireWorkflow `json:"workflows"`
	}
	if e = decode(data, &body); e != nil {
		return Workflows{}, e
	}
	if len(body.Items) > perPage || body.Total < 0 {
		return Workflows{}, ErrInvalid
	}
	result := Workflows{Items: []Workflow{}, Total: body.Total, Complete: len(body.Items) < perPage || body.Total > 0 && page*perPage >= body.Total}
	for _, raw := range body.Items {
		item, e := raw.workflow()
		if e != nil {
			return Workflows{}, e
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}
func (h *HTTP) Workflow(ctx context.Context, repo, id string) (Workflow, error) {
	p, e := h.prefix(repo)
	if e != nil {
		return Workflow{}, e
	}
	encoded, e := key(id)
	if e != nil {
		return Workflow{}, e
	}
	data, e := h.request(ctx, "GET", p+"workflows/"+encoded, false)
	if e != nil {
		return Workflow{}, e
	}
	var raw wireWorkflow
	if e = decode(data, &raw); e != nil {
		return Workflow{}, e
	}
	result, e := raw.workflow()
	if e != nil {
		return Workflow{}, e
	}
	if _, numeric := strconv.ParseInt(id, 10, 64); numeric == nil && result.Key != id {
		return Workflow{}, ErrInvalid
	}
	return result, nil
}
