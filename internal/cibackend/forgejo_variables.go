package cibackend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

var _ RepositoryVariableReader = (*Forgejo)(nil)

const variableReadConcurrency = 4

type variableRepository struct {
	ID     int64
	Prefix string
}

type forgejoVariableName struct {
	OwnerID *int64 `json:"owner_id"`
	RepoID  *int64 `json:"repo_id"`
	Name    string `json:"name"`
}

func (v forgejoVariableName) valid(repository variableRepository) bool {
	return v.OwnerID != nil && *v.OwnerID == 0 && v.RepoID != nil && *v.RepoID == repository.ID && ValidVariableName(v.Name)
}

// variableRequest treats server-owned provider credentials independently from
// the already-authorized AGS caller. Provider response bodies stay private.
func (f *Forgejo) variableRequest(ctx context.Context, path string) (httpResponse, error) {
	response, err := f.requestBodyResponse(ctx, http.MethodGet, path, false, nil)
	if errors.Is(err, ErrForbidden) {
		return httpResponse{}, fmt.Errorf("%w: variable provider denied access", ErrUnavailable)
	}
	if err != nil {
		return httpResponse{}, err
	}
	if response.StatusCode != http.StatusOK {
		return httpResponse{}, ErrInvalid
	}
	return response, nil
}

func (f *Forgejo) variableRepository(ctx context.Context, repo string) (variableRepository, error) {
	prefix, err := f.prefix(repo)
	if err != nil {
		return variableRepository{}, err
	}
	response, err := f.variableRequest(ctx, strings.TrimSuffix(prefix, "/actions/"))
	if errors.Is(err, ErrNotFound) {
		return variableRepository{}, fmt.Errorf("%w: bound variable repository is unavailable", ErrUnavailable)
	}
	if err != nil {
		return variableRepository{}, err
	}
	var wire struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	if err := decode(response.Body, &wire); err != nil {
		return variableRepository{}, err
	}
	if wire.ID <= 0 || !strings.EqualFold(wire.FullName, repo) {
		return variableRepository{}, ErrInvalid
	}
	return variableRepository{ID: wire.ID, Prefix: prefix}, nil
}

// RepositoryVariable reads only the stored repository value. A missing value is
// established against a verified repository, not inferred from a failed binding.
func (f *Forgejo) RepositoryVariable(ctx context.Context, repo, name string) (Variable, error) {
	if !ValidVariableName(name) {
		return Variable{}, ErrInvalid
	}
	repository, err := f.variableRepository(ctx, repo)
	if err != nil {
		return Variable{}, err
	}
	return f.repositoryVariable(ctx, repository, strings.ToUpper(name))
}

func (f *Forgejo) repositoryVariable(ctx context.Context, repository variableRepository, name string) (Variable, error) {
	response, err := f.variableRequest(ctx, repository.Prefix+"variables/"+url.PathEscape(name))
	if err != nil {
		return Variable{}, err
	}
	var wire struct {
		forgejoVariableName
		Data *string `json:"data"`
	}
	if err := decode(response.Body, &wire); err != nil {
		return Variable{}, err
	}
	if !wire.valid(repository) || wire.Data == nil || strings.ToUpper(wire.Name) != name {
		return Variable{}, ErrInvalid
	}
	return Variable{Name: strings.ToUpper(wire.Name), Value: *wire.Data}, nil
}

func (f *Forgejo) variablePageLimit(ctx context.Context) (int, error) {
	response, err := f.variableRequest(ctx, "/settings/api")
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnsupported) {
		return 0, fmt.Errorf("%w: variable pagination settings are unavailable", ErrUnavailable)
	}
	if err != nil {
		return 0, err
	}
	var settings struct {
		MaxResponseItems int `json:"max_response_items"`
	}
	if err := decode(response.Body, &settings); err != nil {
		return 0, err
	}
	if settings.MaxResponseItems <= 0 {
		return 0, ErrInvalid
	}
	return settings.MaxResponseItems, nil
}

func (f *Forgejo) variableNames(ctx context.Context, repository variableRepository, page, limit int) ([]forgejoVariableName, int, error) {
	query := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}
	response, err := f.variableRequest(ctx, repository.Prefix+"variables?"+query.Encode())
	if errors.Is(err, ErrNotFound) {
		return nil, 0, fmt.Errorf("%w: variable inventory is unavailable", ErrUnavailable)
	}
	if err != nil {
		return nil, 0, err
	}
	counts := response.Header.Values("X-Total-Count")
	if len(counts) != 1 {
		return nil, 0, ErrInvalid
	}
	total, err := strconv.Atoi(strings.TrimSpace(counts[0]))
	if err != nil || total < 0 {
		return nil, 0, ErrInvalid
	}
	var names []forgejoVariableName
	if err := decode(response.Body, &names); err != nil {
		return nil, 0, err
	}
	if names == nil || len(names) != min(limit, max(0, total-(page-1)*limit)) {
		return nil, 0, fmt.Errorf("%w: variable inventory page is incomplete", ErrInvalid)
	}
	for _, item := range names {
		if !item.valid(repository) {
			return nil, 0, ErrInvalid
		}
	}
	return names, total, nil
}

// RepositoryVariables translates the requested GitHub page into the necessary
// Forgejo page window. Forgejo 12 lists names with placeholder data, so only the
// requested page is hydrated through single-variable reads.
func (f *Forgejo) RepositoryVariables(ctx context.Context, repo string, page, perPage int) (Variables, error) {
	maxInt := int(^uint(0) >> 1)
	if page < 1 || perPage < 1 || perPage > 100 || page-1 > (maxInt-perPage)/perPage {
		return Variables{}, ErrInvalid
	}
	repository, err := f.variableRepository(ctx, repo)
	if err != nil {
		return Variables{}, err
	}
	maximum, err := f.variablePageLimit(ctx)
	if err != nil {
		return Variables{}, err
	}
	limit := min(perPage, maximum)
	start, end := (page-1)*perPage, page*perPage
	first := start/limit + 1
	seen := make(map[string]bool)
	names := make([]string, 0, perPage)
	total := -1
	for providerPage := first; ; providerPage++ {
		items, count, err := f.variableNames(ctx, repository, providerPage, limit)
		if err != nil {
			return Variables{}, err
		}
		if total >= 0 && total != count {
			return Variables{}, fmt.Errorf("%w: variable inventory changed during observation", ErrUnavailable)
		}
		total = count
		end = min(end, total)
		offset := (providerPage - 1) * limit
		for index, item := range items {
			name := strings.ToUpper(item.Name)
			if seen[name] {
				return Variables{}, fmt.Errorf("%w: duplicate variable in inventory", ErrInvalid)
			}
			seen[name] = true
			if position := offset + index; position >= start && position < end {
				names = append(names, name)
			}
		}
		if end <= offset+len(items) || start >= total {
			break
		}
	}
	if len(names) != max(0, end-start) {
		return Variables{}, ErrInvalid
	}
	items, err := f.hydrateVariables(ctx, repository, names)
	if err != nil {
		return Variables{}, err
	}
	return Variables{Items: items, Total: total}, nil
}

func (f *Forgejo) hydrateVariables(ctx context.Context, repository variableRepository, names []string) ([]Variable, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	items := make([]Variable, len(names))
	work := make(chan int, len(names))
	for index := range names {
		work <- index
	}
	close(work)
	var wg sync.WaitGroup
	var failed sync.Once
	var firstError error
	for worker := 0; worker < min(variableReadConcurrency, len(names)); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range work {
				if ctx.Err() != nil {
					return
				}
				value, err := f.repositoryVariable(ctx, repository, names[index])
				if err != nil {
					if errors.Is(err, ErrNotFound) {
						err = fmt.Errorf("%w: variable disappeared during observation", ErrUnavailable)
					}
					failed.Do(func() { firstError = err; cancel() })
					return
				}
				items[index] = value
			}
		}()
	}
	wg.Wait()
	if firstError != nil {
		return nil, firstError
	}
	if ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	return items, nil
}
