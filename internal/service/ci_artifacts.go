package service

import (
	"context"

	"github.com/ngaut/agent-git-service/internal/cibackend"
)

type CIArtifact struct {
	ID       uint64
	RunID    uint64
	Artifact cibackend.Artifact
}

func (s *Service) CIArtifacts(ctx context.Context, repository string, id uint64) ([]CIArtifact, error) {
	run, err := s.CIRun(ctx, repository, id)
	if err != nil {
		return nil, err
	}
	repo, sel, err := s.ciScope(ctx, repository)
	if err != nil {
		return nil, err
	}
	backend, ok := sel.Backend.(cibackend.ArtifactBackend)
	if !ok {
		return nil, cibackend.ErrUnsupported
	}
	artifacts, err := backend.Artifacts(ctx, sel.Binding.Repository, run.Run)

	if err != nil {
		return nil, ciError(err)
	}
	out := make([]CIArtifact, 0, len(artifacts))
	seen := map[string]bool{}
	for _, a := range artifacts {
		if a.Run != run.Run.Key || a.Key == "" || seen[a.Key] {
			return nil, cibackend.ErrInvalid
		}
		seen[a.Key] = true
		mapped, err := s.mapCI(ctx, repo, sel, "artifact", a.Key, a.Run)
		if err != nil {
			return nil, err
		}
		out = append(out, CIArtifact{ID: mapped, RunID: run.ID, Artifact: a})
	}
	return out, nil
}
func (s *Service) CIArtifactDownload(ctx context.Context, repository string, id uint64) ([]byte, error) {
	repo, sel, err := s.ciScope(ctx, repository)
	if err != nil {
		return nil, err
	}
	backend, ok := sel.Backend.(cibackend.ArtifactBackend)
	if !ok {
		return nil, cibackend.ErrUnsupported
	}
	m, err := s.resolveCI(ctx, repo, sel, "artifact", id)
	if err != nil {
		return nil, err
	}
	run, err := sel.Backend.Run(ctx, sel.Binding.Repository, m.ParentID)
	if err != nil {
		return nil, ciError(err)
	}
	if run.Key != m.ParentID {
		return nil, cibackend.ErrInvalid
	}
	// Re-observe the exact artifact in its original run before downloading.
	artifacts, err := backend.Artifacts(ctx, sel.Binding.Repository, run)
	if err != nil {
		return nil, ciError(err)
	}
	for _, a := range artifacts {
		if a.Key == m.ExternalID && a.Run == m.ParentID {
			data, err := backend.ArtifactDownload(ctx, sel.Binding.Repository, run, a.Key)
			return data, ciError(err)
		}
	}
	return nil, ErrNotFound
}
