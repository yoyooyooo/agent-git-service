package rest

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

func (d *Deps) ciRepositoryVariables(w http.ResponseWriter, r *http.Request, repository string) {
	page, perPage, ok := variablePagination(r)
	if !ok {
		respond.ValidationFailed(w, "invalid variable pagination")
		return
	}
	variables, err := d.Svc.CIRepositoryVariables(r.Context(), repository, page, perPage)
	if err != nil {
		variableHTTPError(r, w, err)
		return
	}
	rows := make([]map[string]string, 0, len(variables.Items))
	for _, variable := range variables.Items {
		rows = append(rows, map[string]string{"name": variable.Name, "value": variable.Value})
	}
	// Forgejo has no variable timestamps. Build links only in the AGS API namespace.
	last := 1
	if variables.Total > 0 {
		last += (variables.Total - 1) / perPage
	}
	var links []string
	link := func(target int, relation string) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(target))
		q.Set("per_page", strconv.Itoa(perPage))
		path := *r.URL
		path.RawQuery = q.Encode()
		links = append(links, fmt.Sprintf("<%s%s>; rel=%q", d.Svc.APIBaseURL(), path.RequestURI(), relation))
	}
	if page < last {
		link(page+1, "next")
		link(last, "last")
	}
	if page > 1 {
		link(1, "first")
		previous := page - 1
		if previous > last {
			previous = last
		}
		link(previous, "prev")
	}
	if len(links) != 0 {
		w.Header().Set("Link", strings.Join(links, ", "))
	}
	respond.JSON(w, http.StatusOK, map[string]any{"total_count": variables.Total, "variables": rows})
}

func (d *Deps) ciRepositoryVariable(w http.ResponseWriter, r *http.Request, repository string) {
	variable, err := d.Svc.CIRepositoryVariable(r.Context(), repository, pathParam(r, "name"))
	if err != nil {
		variableHTTPError(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]string{"name": variable.Name, "value": variable.Value})
}

func variablePagination(r *http.Request) (page, perPage int, ok bool) {
	page, perPage = 1, 30
	for _, param := range []struct {
		name string
		max  int
		out  *int
	}{{"page", int(^uint(0) >> 1), &page}, {"per_page", 100, &perPage}} {
		values, present := r.URL.Query()[param.name]
		if !present {
			continue
		}
		if len(values) != 1 {
			return 0, 0, false
		}
		value, err := strconv.Atoi(values[0])
		if err != nil || value < 1 || value > param.max || strconv.Itoa(value) != values[0] {
			return 0, 0, false
		}
		*param.out = value
	}
	if page-1 > (int(^uint(0)>>1)-perPage)/perPage {
		return 0, 0, false
	}
	return page, perPage, true
}

// Variable errors distinguish caller authorization from server credentials.
// Provider response bodies and values never enter the response.
func variableHTTPError(r *http.Request, w http.ResponseWriter, err error) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case errors.Is(err, cibackend.ErrUnsupported):
		respond.Error(w, http.StatusNotImplemented, "Selected CI scope does not support this variable operation")
	case errors.Is(err, cibackend.ErrNotFound):
		respond.NotFound(w)
	case errors.Is(err, cibackend.ErrUnavailable), errors.Is(err, cibackend.ErrInvalid), errors.Is(err, cibackend.ErrForbidden):
		respond.Error(w, http.StatusBadGateway, "Selected CI backend could not confirm repository variables")
	case errors.Is(err, service.ErrValidation):
		respond.ValidationFailed(w, "invalid variable request")
	default:
		respond.ServiceErrorRequest(r, w, err)
	}
}
