package cibackend

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

var ErrInvalidRunSelector = errors.New("invalid CI run selector")

// RunSelectorBackend resolves an explicitly typed read locator. Numeric Actions
// IDs still resolve through the service's repository/backend identity map.
type RunSelectorBackend interface {
	RunBySelector(context.Context, string, string) (Run, error)
}

func (f *Forgejo) RunBySelector(ctx context.Context, repo, selector string) (Run, error) {
	number, err := f.selectorNumber(repo, selector)
	if err != nil {
		return Run{}, err
	}
	// Forgejo filters its repository index in SQL. A display number is never
	// guessed to be the ActionRun database ID, even when the numbers coincide.
	page, err := f.runPage(ctx, repo, url.Values{"page": {"1"}, "limit": {"2"}, "run_number": {strconv.FormatInt(number, 10)}})
	if err != nil {
		return Run{}, err
	}
	if page.Total == 0 && len(page.Items) == 0 {
		return Run{}, ErrNotFound
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Number != number {
		return Run{}, ErrInvalid
	}
	return page.Items[0], nil
}

func (f *Forgejo) selectorNumber(repo, selector string) (int64, error) {
	if !ValidRepository(repo) || len(selector) > 4096 || selector != strings.TrimSpace(selector) || strings.ContainsAny(selector, "\r\n\x00") {
		return 0, ErrInvalidRunSelector
	}
	value, typed := strings.CutPrefix(selector, "forgejo:")
	if !typed {
		u, err := url.Parse(selector)
		if err != nil || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" {
			return 0, ErrInvalidRunSelector
		}
		matched := false
		for _, base := range []string{f.apiWebBase(), f.config.WebURL} {
			if base == "" {
				continue
			}
			b, err := url.Parse(base)
			if err != nil || u.Scheme != b.Scheme || !strings.EqualFold(u.Host, b.Host) {
				continue
			}
			prefix := strings.TrimRight(b.Path, "/") + "/" + repo + "/actions/runs/"
			if strings.HasPrefix(u.Path, prefix) {
				value, matched = strings.TrimPrefix(u.Path, prefix), true
				break
			}
		}
		if !matched {
			return 0, ErrNotFound
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != value {
		return 0, ErrInvalidRunSelector
	}
	return n, nil
}

func (f *Forgejo) apiWebBase() string {
	return strings.TrimRight(f.config.URL, "/")
}

func (f *Forgejo) runWebURL(repo string, number int64) string {
	base := f.config.WebURL
	if base == "" {
		base = f.apiWebBase()
	}
	return strings.TrimRight(base, "/") + "/" + repo + "/actions/runs/" + strconv.FormatInt(number, 10)
}

// MatchesRunQuery is also used by the service as a defensive check. A status
// filter may name either an execution state or a completed run's conclusion.
func MatchesRunQuery(run Run, q Query) bool {
	return (q.HeadSHA == "" || run.HeadSHA == q.HeadSHA) &&
		(q.Branch == "" || run.Branch == q.Branch) &&
		(q.Event == "" || run.Event == q.Event) &&
		(q.Workflow == "" || run.Workflow == q.Workflow) &&
		(q.Status == "" || run.Status == q.Status || run.Conclusion == q.Status)
}
