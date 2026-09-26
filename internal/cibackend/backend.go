// Package cibackend defines CI facts independently of Git hosting, PR projection
// and merge authority. Adapters receive server-owned credentials, never user tokens.
package cibackend

import (
	"context"
	"errors"
	"time"
)

var (
	ErrUnsupported = errors.New("CI backend does not support this operation")
	ErrUnavailable = errors.New("CI backend is unavailable")
	ErrInvalid     = errors.New("CI backend returned inconsistent evidence")
	ErrNotFound    = errors.New("CI resource not found")
)

// Keys are opaque provider-local coordinates. The service allocates durable
// GitHub-shaped numeric IDs; neither callers nor adapters guess across backends.
type Run struct {
	Key          string     `json:"key"`
	Workflow     string     `json:"workflow"`
	Name         string     `json:"name"`
	Branch       string     `json:"branch"`
	HeadSHA      string     `json:"head_sha"`
	Event        string     `json:"event"`
	Status       string     `json:"status"`
	Conclusion   string     `json:"conclusion"`
	URL          string     `json:"url"`
	Number       int64      `json:"number"`
	Attempt      int        `json:"attempt"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	PullRequests []int      `json:"pull_requests"`
}
type Step struct {
	Name        string     `json:"name"`
	Number      int        `json:"number"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}
type Job struct {
	Key         string     `json:"key"`
	Run         string     `json:"run"`
	Name        string     `json:"name"`
	HeadSHA     string     `json:"head_sha"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	URL         string     `json:"url"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	Steps       []Step     `json:"steps"`
}
type Workflow struct {
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	State     string    `json:"state"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
type Workflows struct {
	Items    []Workflow
	Total    int
	Complete bool
}

type Query struct {
	HeadSHA, Branch, Event, Status, Workflow string
	Page, PerPage                            int
}
type Runs struct {
	Items    []Run
	Total    int
	Complete bool
}
type Jobs struct {
	Items    []Job
	Total    int
	Complete bool
}

type Backend interface {
	Workflows(context.Context, string, int, int) (Workflows, error)
	Workflow(context.Context, string, string) (Workflow, error)
	Runs(context.Context, string, Query) (Runs, error)
	Run(context.Context, string, string) (Run, error)
	Jobs(context.Context, string, string) (Jobs, error)
	Job(context.Context, string, string) (Job, error)
	JobLogs(context.Context, string, string) ([]byte, error)
	RunLogs(context.Context, string, string) ([]byte, error)
	Action(context.Context, string, string, string) error
}
