package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/stretchr/testify/require"
)

type optionalMetadataBackend struct {
	*ciFixtureBackend
	stepsErr     error
	artifactsErr error
}

func (b *optionalMetadataBackend) JobSteps(context.Context, string, cibackend.Run, cibackend.Job) ([]cibackend.Step, error) {
	// Even an adapter returning data alongside an error must not leak that data.
	return []cibackend.Step{{Name: "untrusted", Number: 1}}, b.stepsErr
}
func (b *optionalMetadataBackend) Artifacts(context.Context, string, cibackend.Run) ([]cibackend.Artifact, error) {
	return nil, b.artifactsErr
}
func (b *optionalMetadataBackend) ArtifactDownload(context.Context, string, cibackend.Run, string) ([]byte, error) {
	return nil, cibackend.ErrUnsupported
}

func TestCIOptionalStepErrorsDiscardDataWithoutHidingJobErrors(t *testing.T) {
	for _, stepErr := range []error{cibackend.ErrUnavailable, cibackend.ErrForbidden, cibackend.ErrNotFound, cibackend.ErrUnsupported, cibackend.ErrInvalid, context.DeadlineExceeded} {
		t.Run(stepErr.Error(), func(t *testing.T) {
			s, ctx, repo, _, fixture, config := ciFixture(t)
			backend := &optionalMetadataBackend{ciFixtureBackend: fixture, stepsErr: stepErr}
			registry, err := cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
			require.NoError(t, err)
			s.CI = registry
			runs, err := s.CIRuns(ctx, repo.FullName, cibackend.Query{})
			require.NoError(t, err)
			jobs, err := s.CIJobs(ctx, repo.FullName, runs.Runs[0].ID)
			require.NoError(t, err)
			require.Len(t, jobs, 1)
			require.Empty(t, jobs[0].Job.Steps)
			job, err := s.CIJob(ctx, repo.FullName, jobs[0].ID)
			require.NoError(t, err)
			require.Empty(t, job.Job.Steps)
			fixture.jobs[0].HeadSHA = "foreign"
			_, err = s.CIJobs(ctx, repo.FullName, runs.Runs[0].ID)
			require.ErrorIs(t, err, cibackend.ErrInvalid, "job binding is mandatory even when steps are optional")
			_, err = s.CIJob(ctx, repo.FullName, jobs[0].ID)
			require.ErrorIs(t, err, cibackend.ErrInvalid)
		})
	}
}

func TestCIArtifactsOnlyUnsupportedFallsBackToEmpty(t *testing.T) {
	for _, tc := range []struct{ err, want error }{
		{fmt.Errorf("optional: %w", cibackend.ErrUnsupported), nil},
		{cibackend.ErrUnavailable, cibackend.ErrUnavailable},
		{cibackend.ErrForbidden, ErrForbidden},
		{cibackend.ErrNotFound, ErrNotFound},
		{cibackend.ErrInvalid, cibackend.ErrInvalid},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			s, ctx, repo, _, fixture, config := ciFixture(t)
			backend := &optionalMetadataBackend{ciFixtureBackend: fixture, artifactsErr: tc.err}
			registry, err := cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
			require.NoError(t, err)
			s.CI = registry
			runs, err := s.CIRuns(ctx, repo.FullName, cibackend.Query{})
			require.NoError(t, err)
			artifacts, err := s.CIArtifacts(ctx, repo.FullName, runs.Runs[0].ID)
			require.ErrorIs(t, err, tc.want)
			require.Empty(t, artifacts)
			if tc.want == nil {
				require.NotNil(t, artifacts)
			}
		})
	}
}
