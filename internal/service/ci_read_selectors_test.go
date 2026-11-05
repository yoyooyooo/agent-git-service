package service

import (
	"context"
	"strconv"
	"testing"

	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/stretchr/testify/require"
)

type ciReadSelectorFixture struct{ *ciFixtureBackend }

func (f *ciReadSelectorFixture) RunBySelector(_ context.Context, repository, selector string) (cibackend.Run, error) {
	f.calls++
	if repository != "ci/project" || selector != "forgejo:9" {
		return cibackend.Run{}, cibackend.ErrNotFound
	}
	return f.runs[0], f.failure
}

func TestCIReadSelectorsKeepAuthorizationAndCanonicalIdentity(t *testing.T) {
	s, ctx, repo, _, fixture, config := ciFixture(t)
	backend := &ciReadSelectorFixture{fixture}
	options := config.Backends["builder"]
	options.Kind = "forgejo"
	config.Backends["builder"] = options
	registry, err := cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
	require.NoError(t, err)
	s.CI = registry
	listed, err := s.CIRuns(ctx, repo.FullName, cibackend.Query{})
	require.NoError(t, err)
	canonical := listed.Runs[0].ID
	selected, err := s.CIReadRun(ctx, repo.FullName, "forgejo:9")
	require.NoError(t, err)
	require.Equal(t, canonical, selected.ID)
	require.Equal(t, listed.Runs[0].WorkflowID, selected.WorkflowID)
	numeric, err := s.CIReadRun(ctx, repo.FullName, strconv.FormatUint(canonical, 10))
	require.NoError(t, err)
	require.Equal(t, canonical, numeric.ID)
	calls := backend.calls
	_, err = s.CIReadRun(ctx, repo.FullName, "9")
	require.ErrorIs(t, err, ErrNotFound, "bare provider numbers never become canonical IDs")
	require.Equal(t, calls, backend.calls)
	reader := db.User{Login: "unrelated-selector-reader", Type: db.TypeUser}
	require.NoError(t, s.DB.Create(&reader).Error)
	_, err = s.CIReadRun(ContextWithUser(t.Context(), reader), repo.FullName, "forgejo:9")
	require.Error(t, err)
	require.Equal(t, calls, backend.calls, "a selector cannot bypass AGS repository access")
	other := db.Repository{OwnerID: repo.OwnerID, Name: "other", FullName: "native-agent/other", Private: true}
	require.NoError(t, s.DB.Create(&other).Error)
	config.Repositories[other.FullName] = cibackend.Binding{Backend: "builder", Repository: "ci/project"}
	s.CI, err = cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
	require.NoError(t, err)
	_, err = s.CIReadRun(ctx, other.FullName, strconv.FormatUint(canonical, 10))
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, calls, backend.calls, "canonical IDs are scoped to the AGS repository too")
	options.URL = "https://replacement.example.test"
	config.Backends["builder"] = options
	s.CI, err = cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
	require.NoError(t, err)
	_, err = s.CIReadRun(ctx, repo.FullName, strconv.FormatUint(canonical, 10))
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, calls, backend.calls, "retired backend IDs are not sent to another origin")
	fresh, err := s.CIReadRun(ctx, repo.FullName, "forgejo:9")
	require.NoError(t, err)
	require.NotEqual(t, canonical, fresh.ID)
}

func TestCIRunsAcceptsAConclusionFilter(t *testing.T) {
	s, ctx, repo, _, backend, _ := ciFixture(t)
	backend.runs[0].Conclusion = "failure"
	for _, status := range []string{"failure", "completed"} {
		found, err := s.CIRuns(ctx, repo.FullName, cibackend.Query{Status: status})
		require.NoError(t, err)
		require.Len(t, found.Runs, 1)
		require.Equal(t, 1, found.Total)
	}
}
