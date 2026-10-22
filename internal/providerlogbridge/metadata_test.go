package providerlogbridge

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestStepsUseDatabaseStatusAndNullableTimesWithoutRequiringLogs(t *testing.T) {
	h, _ := setupBridge(t, 1024)
	token := "bridge-secret"
	for _, sql := range []string{
		`CREATE TABLE action_task_step (id integer primary key, task_id integer, repo_id integer, name text, "index" integer, status integer, started integer, stopped integer)`,
		`INSERT INTO action_task_step VALUES (1,41,1,'checkout',0,1,1767225600,1767225601),(2,41,1,'pending',1,5,0,0),(3,41,1,'unknown',2,0,0,0)`,
		`UPDATE action_task SET log_expired=true`,
	} {
		if err := h.db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	// setupBridge's authoritative task/repo identifiers are used below.
	var task int64
	h.db.Raw(`SELECT id FROM action_task LIMIT 1`).Scan(&task)
	h.db.Exec(`UPDATE action_task_step SET task_id=?`, task)
	var repoID int64
	h.db.Raw(`SELECT repo_id FROM action_task LIMIT 1`).Scan(&repoID)
	h.db.Exec(`UPDATE action_task_step SET repo_id=?`, repoID)
	r := httptest.NewRequest("GET", "/?provider_pr=98&head_ref="+fixtureRef+"&head_sha="+fixtureSHA+"&resource=steps", nil)
	r.RemoteAddr = fixturePeer
	r.Header.Set("Authorization", "Bearer "+token)
	router := chi.NewRouter()
	router.Use(CaptureSocketPeer)
	router.Get("/", func(w http.ResponseWriter, r *http.Request) {
		rc := chi.RouteContext(r.Context())
		rc.URLParams.Add("owner", fixtureOwner)
		rc.URLParams.Add("repo", fixtureRepo)
		rc.URLParams.Add("task", strconvInt(task))
		h.ServeHTTP(w, r)
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("steps: %d %s", w.Code, w.Body.String())
	}
	var body struct{ Steps []map[string]any }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Steps) != 3 || body.Steps[0]["number"] != float64(1) || body.Steps[0]["conclusion"] != "success" || body.Steps[1]["started_at"] != nil || body.Steps[1]["conclusion"] != nil || body.Steps[2]["status"] != "" {
		t.Fatalf("steps=%s", w.Body.String())
	}
	// The log path still fails closed for expired logs.
	w = request(t, h, fixtureOwner, fixtureRepo, strconvInt(task), token)
	if w.Code != 409 {
		t.Fatalf("expired log status=%d", w.Code)
	}
}

func strconvInt(n int64) string { return fmt.Sprintf("%d", n) }

func TestArtifactsGroupFilesAndDownloadThroughBoundedOwnedStorage(t *testing.T) {
	h, _ := setupBridge(t, 4096)
	token := "bridge-secret"
	h.artifactRoot = t.TempDir()
	var run, repo int64
	h.db.Raw(`SELECT id FROM action_run LIMIT 1`).Scan(&run)
	h.db.Raw(`SELECT id FROM repository LIMIT 1`).Scan(&repo)
	if err := h.db.Exec(`CREATE TABLE action_artifact (id integer primary key, run_id integer, repo_id integer, artifact_name text, file_size integer, status integer, created_unix integer, updated_unix integer, expired_unix integer, storage_path text, artifact_path text, content_encoding text)`).Error; err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"first.txt", "nested/second.txt"} {
		storage := fmt.Sprintf("file-%d", i)
		if err := os.WriteFile(filepath.Join(h.artifactRoot, storage), []byte("payload"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := h.db.Exec(`INSERT INTO action_artifact VALUES (?,?,?,'bundle',7,2,1767225600,1767225601,1769999999,?,?, '')`, i+1, run, repo, storage, name).Error; err != nil {
			t.Fatal(err)
		}
	}
	call := func(query, credential string) *httptest.ResponseRecorder {
		router := chi.NewRouter()
		router.Use(CaptureSocketPeer)
		router.Get("/{owner}/{repo}/{run}", h.ServeArtifacts)
		req := httptest.NewRequest("GET", fmt.Sprintf("/%s/%s/%d?%s", fixtureOwner, fixtureRepo, run, query), nil)
		req.RemoteAddr = fixturePeer
		req.Header.Set("Authorization", "Bearer "+credential)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	query := "head_sha=" + fixtureSHA
	w := call(query, token)
	if w.Code != 200 {
		t.Fatalf("list=%d %s", w.Code, w.Body.String())
	}
	var body struct{ Artifacts []map[string]any }
	json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Artifacts) != 1 || body.Artifacts[0]["size_in_bytes"] != float64(14) || strings.Contains(w.Body.String(), "storage_path") {
		t.Fatalf("list=%s", w.Body.String())
	}
	w = call(query+"&download=1", token)
	if w.Code != 200 {
		t.Fatalf("zip=%d %s", w.Code, w.Body.String())
	}
	archive, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil || len(archive.File) != 2 {
		t.Fatalf("archive=%v %v", archive, err)
	}
	for _, f := range archive.File {
		r, _ := f.Open()
		data, _ := io.ReadAll(r)
		r.Close()
		if string(data) != "payload" {
			t.Fatal("lost artifact")
		}
	}
	if w = call(query, "wrong"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w = call("head_sha="+strings.Repeat("b", 40), token); w.Code != 404 {
		t.Fatal(w.Code)
	}
	h.db.Exec(`UPDATE action_artifact SET artifact_path='../escape' WHERE id=1`)
	if w = call(query+"&download=1", token); w.Code != 502 {
		t.Fatalf("unsafe path=%d", w.Code)
	}
	h.db.Exec(`UPDATE action_artifact SET status=4`)
	if w = call(query+"&download=1", token); w.Code != 410 {
		t.Fatalf("expired=%d", w.Code)
	}
}
