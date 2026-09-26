package service

import (
	"context"
	"errors"
	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/db"
	"strconv"
)

type CIWorkflow struct {
	ID       uint64
	Workflow cibackend.Workflow
	Backend  string
}
type CIWorkflows struct {
	Items    []CIWorkflow
	Total    int
	Complete bool
	Scope    string
}

func (s *Service) CIWorkflows(ctx context.Context, repository string, page, perPage int) (CIWorkflows, error) {
	repo, selected, e := s.ciScope(ctx, repository)
	if e != nil {
		return CIWorkflows{}, e
	}
	if selected.Name == "none" {
		return CIWorkflows{Items: []CIWorkflow{}, Complete: true}, nil
	}
	found, e := selected.Backend.Workflows(ctx, selected.Binding.Repository, page, perPage)
	if errors.Is(e, cibackend.ErrUnsupported) {
		// Some backends expose runs but no workflow catalogue. The already
		// observed workflow identity map is sufficient for gh run list's name
		// lookup. It is explicitly NOT a catalogue of enabled workflows.
		query := s.DBForCtx(ctx).Model(&db.CIResource{}).Where("repository_id = ? AND namespace = ? AND external_repository = ? AND kind = ?", repo.ID, selected.Namespace, selected.Binding.Repository, "workflow")
		var total int64
		if err := query.Count(&total).Error; err != nil {
			return CIWorkflows{}, err
		}
		var rows []db.CIResource
		if err := query.Order("id ASC").Offset((page - 1) * perPage).Limit(perPage).Find(&rows).Error; err != nil {
			return CIWorkflows{}, err
		}
		out := CIWorkflows{Items: []CIWorkflow{}, Total: int(total), Complete: page*perPage >= int(total), Scope: "observed_run_identities"}
		for _, row := range rows {
			out.Items = append(out.Items, CIWorkflow{ID: CIExternalIDBase + uint64(row.ID), Workflow: cibackend.Workflow{Key: row.ExternalID, Name: row.ExternalID, State: "unknown"}, Backend: selected.Name})
		}
		return out, nil
	}
	if e != nil {
		return CIWorkflows{}, ciError(e)
	}
	out := CIWorkflows{Items: []CIWorkflow{}, Total: found.Total, Complete: found.Complete, Scope: "configured_workflows"}
	for _, workflow := range found.Items {
		id, e := s.mapCI(ctx, repo, selected, "workflow", workflow.Key, "")
		if e != nil {
			return CIWorkflows{}, e
		}
		out.Items = append(out.Items, CIWorkflow{ID: id, Workflow: workflow, Backend: selected.Name})
	}
	return out, nil
}
func (s *Service) CIWorkflow(ctx context.Context, repository string, id string) (CIWorkflow, error) {
	repo, selected, e := s.ciScope(ctx, repository)
	if e != nil {
		return CIWorkflow{}, e
	}
	if selected.Backend == nil {
		return CIWorkflow{}, ErrNotFound
	}
	external := id
	if number, e := strconv.ParseUint(id, 10, 64); e == nil {
		mapped, e := s.resolveCI(ctx, repo, selected, "workflow", number)
		if e != nil {
			return CIWorkflow{}, e
		}
		external = mapped.ExternalID
	}
	// A file name is a valid standard Actions selector; numeric IDs are always
	// AGS mappings. The adapter validates the file name as a single path segment.
	item, e := selected.Backend.Workflow(ctx, selected.Binding.Repository, external)
	if e != nil {
		if !errors.Is(e, cibackend.ErrUnsupported) {
			return CIWorkflow{}, ciError(e)
		}
		var known db.CIResource
		if err := s.DBForCtx(ctx).Where("repository_id = ? AND namespace = ? AND external_repository = ? AND kind = ? AND external_id = ?", repo.ID, selected.Namespace, selected.Binding.Repository, "workflow", external).First(&known).Error; err != nil {
			return CIWorkflow{}, wrapErr(err)
		}
		item = cibackend.Workflow{Key: external, Name: external, State: "unknown"}
	}
	mapped, e := s.mapCI(ctx, repo, selected, "workflow", item.Key, "")
	if e != nil {
		return CIWorkflow{}, e
	}
	return CIWorkflow{ID: mapped, Workflow: item, Backend: selected.Name}, nil
}
