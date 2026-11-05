package providerlogbridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func runBridgeFixture(t *testing.T) (*Handler, http.Handler) {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "run-inventory.db")), &gorm.Config{})
	require.NoError(t, err)
	pool, err := database.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	for _, statement := range []string{
		`CREATE TABLE repository(id INTEGER PRIMARY KEY, owner_name TEXT, lower_name TEXT)`,
		`CREATE TABLE action_run(id INTEGER PRIMARY KEY, repo_id INTEGER, "index" INTEGER, workflow_id TEXT, title TEXT, ref TEXT, commit_sha TEXT, event TEXT, status INTEGER, need_approval BOOLEAN, is_fork_pull_request BOOLEAN, created INTEGER, updated INTEGER, started INTEGER)`,
		`INSERT INTO repository VALUES (1,'ci','project'),(2,'ci','other')`,
	} {
		require.NoError(t, database.Exec(statement).Error)
	}
	for n := 1; n <= 125; n++ {
		branch, workflow, status := "refs/heads/main", "ci.yml", 1
		if n%2 == 0 {
			branch, workflow, status = "refs/heads/feature", "test.yml", 2
		}
		event := "push"
		if n%5 == 0 {
			branch, event = "refs/pull/42/head", "pull_request_sync"
		}
		require.NoError(t, database.Exec(`INSERT INTO action_run VALUES (?,?,?,?,'fixture run',?,?,?,?,false,false,1767225600,1767225660,1767225601)`, 10000+n, 1, n, workflow, branch, fixtureSHA, event, status).Error)
	}
	require.NoError(t, database.Exec(`INSERT INTO action_run SELECT 99999,2,99999,workflow_id,title,ref,commit_sha,event,status,need_approval,is_fork_pull_request,created,updated,started FROM action_run LIMIT 1`).Error)
	h, err := New(Config{DB: database, ActionsLogDir: t.TempDir(), ServiceToken: "bridge-only", AllowedCIDRs: []string{"192.0.2.0/24"}})
	require.NoError(t, err)
	mux := chi.NewRouter()
	mux.Use(CaptureSocketPeer)
	mux.Get("/api/internal/provider-logs/repos/{owner}/{repo}/runs", h.ServeHTTP)
	return h, mux
}

func runBridgeRequest(mux http.Handler, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/internal/provider-logs/repos/ci/project/runs?"+query, nil)
	req.RemoteAddr = fixturePeer
	req.Header.Set("Authorization", "Bearer bridge-only")
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	return res
}

func TestBridgeRunsFilterOrderCountAndPaginateWithinOneRepository(t *testing.T) {
	_, mux := runBridgeFixture(t)
	for page := 1; page <= 3; page++ {
		res := runBridgeRequest(mux, fmt.Sprintf("page=%d&per_page=100", page))
		require.Equal(t, 200, res.Code, res.Body.String())
		var body struct {
			Schema, Repo string
			Total        int `json:"total_count"`
			Runs         []struct {
				ID     int64 `json:"id"`
				Number int64 `json:"index_in_repo"`
			} `json:"workflow_runs"`
		}
		require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
		require.Equal(t, "ags.internal-provider-runs.v1", body.Schema)
		require.Equal(t, "ci/project", body.Repo)
		require.Equal(t, 125, body.Total)
		require.Len(t, body.Runs, map[int]int{1: 100, 2: 25, 3: 0}[page])
		for i, run := range body.Runs {
			require.Equal(t, int64(125-(page-1)*100-i), run.Number)
			require.Equal(t, 10000+run.Number, run.ID)
		}
	}
	q := url.Values{"page": {"2"}, "per_page": {"7"}, "branch": {"feature"}, "workflow": {"test.yml"}, "status": {"failure"}, "event": {"push"}, "head_sha": {fixtureSHA}}
	res := runBridgeRequest(mux, q.Encode())
	require.Equal(t, 200, res.Code, res.Body.String())
	var body struct {
		Total int `json:"total_count"`
		Runs  []struct {
			Number int64 `json:"index_in_repo"`
		} `json:"workflow_runs"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
	want := []int64{}
	for n := int64(125); n > 0; n-- {
		if n%2 == 0 && n%5 != 0 {
			want = append(want, n)
		}
	}
	require.Equal(t, len(want), body.Total)
	require.Len(t, body.Runs, 7)
	for i, row := range body.Runs {
		require.Equal(t, want[7+i], row.Number)
	}
	for _, query := range []string{
		"page=1&per_page=100&event=pull_request&branch=%2342&status=completed",
		"page=1&per_page=100&event=pull_request&status=completed",
	} {
		res := runBridgeRequest(mux, query)
		require.Equal(t, 200, res.Code, res.Body.String())
		require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
		require.Equal(t, 25, body.Total)
		require.Len(t, body.Runs, 25)
	}
	res = runBridgeRequest(mux, "page=1&per_page=100&branch="+url.QueryEscape("' OR 1=1 --"))
	require.Equal(t, 200, res.Code, res.Body.String())
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
	require.Zero(t, body.Total)
	require.Empty(t, body.Runs)
}

func TestBridgeRunInventoryRejectsUnboundAndInvalidReads(t *testing.T) {
	h, mux := runBridgeFixture(t)
	for _, query := range []string{"page=0&per_page=10", "page=1001&per_page=10", "page=1&per_page=101", "page=1&per_page=0", "page=01&per_page=10", "page=1&per_page=10&status=x&status=y", "page=1&per_page=10&head_sha=bad", "page=1&per_page=10&download=1"} {
		res := runBridgeRequest(mux, query)
		require.Equal(t, 422, res.Code, query+": "+res.Body.String())
	}
	for _, mode := range []string{"wrong-peer", "wrong-token", "wrong-repo"} {
		req := httptest.NewRequest(http.MethodGet, "/api/internal/provider-logs/repos/ci/project/runs?page=1&per_page=10", nil)
		req.RemoteAddr = fixturePeer
		req.Header.Set("Authorization", "Bearer bridge-only")
		want := 401
		switch mode {
		case "wrong-peer":
			req.RemoteAddr = "198.51.100.10:40000"
			req.Header.Set("X-Forwarded-For", "192.0.2.10")
		case "wrong-token":
			req.Header.Set("Authorization", "Bearer other")
		case "wrong-repo":
			req.URL.Path = strings.Replace(req.URL.Path, "/ci/project/", "/ci/missing/", 1)
			want = 404
		}
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		require.Equal(t, want, res.Code, mode+": "+res.Body.String())
	}
	require.NoError(t, h.db.Exec(`UPDATE action_run SET "index"=125 WHERE id=10124`).Error)
	res := runBridgeRequest(mux, "page=1&per_page=100")
	require.Equal(t, 502, res.Code, res.Body.String())
	require.NotContains(t, res.Body.String(), "workflow_runs", "invalid ordering cannot return a partial page")
}
