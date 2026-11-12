package cibackend

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/providerlogbridge"
	"github.com/ngaut/agent-git-service/internal/providerlogprotocol"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestForgejoStepLogsPreserveUnattributedOutputThroughRealBridge(t *testing.T) {
	t.Run("plain", func(t *testing.T) { testForgejoStepLogs(t, false) })
	t.Run("zstd", func(t *testing.T) { testForgejoStepLogs(t, true) })
}

func testForgejoStepLogs(t *testing.T, compressed bool) {
	backend, _, text, parts := forgejoStepLogFixture(t, compressed)
	run, err := backend.Run(t.Context(), "ci/project", "17")
	if err != nil {
		t.Fatal(err)
	}
	job, err := backend.Job(t.Context(), "ci/project", "17:41")
	if err != nil {
		t.Fatal(err)
	}
	steps, err := backend.(StepBackend).JobSteps(t.Context(), "ci/project", run, job)
	if err != nil || len(steps) != 3 {
		t.Fatalf("provider steps and explicit unassigned-output step: %+v %v", steps, err)
	}
	if steps[0].Number != 1 || steps[0].Conclusion != "success" || steps[1].Number != 2 || steps[1].Conclusion != "failure" || steps[2].Number != 3 || steps[2].Name != "AGS: output outside recorded steps" || steps[2].Conclusion != "failure" {
		t.Fatalf("step identity and diagnostic conclusion: %+v", steps)
	}
	archive, err := backend.RunLogs(t.Context(), "ci/project", "17")
	if err != nil {
		t.Fatal(err)
	}
	files := readStepLogArchive(t, archive)
	want := map[string]string{
		"test/1_step.txt": parts[1],
		"test/2_step.txt": parts[2] + parts[3],
		"test/3_step.txt": parts[0] + parts[4],
	}
	if len(files) != len(want) {
		t.Fatalf("step archive contains %d files, want %d: %v", len(files), len(want), files)
	}
	for name, body := range want {
		if files[name] != body {
			t.Errorf("%s = %q, want %q", name, files[name], body)
		}
	}
	if files["test/3_step.txt"][:len(parts[0])]+files["test/1_step.txt"]+files["test/2_step.txt"]+files["test/3_step.txt"][len(parts[0]):] != text {
		t.Error("archive did not preserve every byte exactly once")
	}
	// The failed provider step includes successful test output inside it. Only
	// provider step boundaries, not text heuristics, define --log-failed.
	if !strings.Contains(files["test/2_step.txt"], "test passed inside failed step") {
		t.Error("passing output inside a failed step was discarded")
	}
	raw, err := backend.JobLogs(t.Context(), "ci/project", "17:41")
	if err != nil || string(raw) != text {
		t.Fatalf("raw job log changed: %q %v", raw, err)
	}
}

func TestForgejoStepLogsFallbackPreservesEntireJob(t *testing.T) {
	for _, test := range []struct{ name, mutation string }{
		{"overlap", `UPDATE action_task_step SET log_index=1 WHERE "index"=1`},
		{"out_of_range", `UPDATE action_task_step SET log_length=999 WHERE "index"=1`},
		{"missing_failed_step", `DELETE FROM action_task_step WHERE "index"=1`},
		{"wrong_step_repository", `UPDATE action_task_step SET repo_id=2 WHERE "index"=1`},
		{"corrupt_indexes", `UPDATE action_task SET log_indexes=x'80'`},
		{"wrong_size", `UPDATE action_task SET log_size=1`},
		{"no_failed_step", `UPDATE action_task_step SET status=1`},
		{"unfinished_step", `UPDATE action_task_step SET status=6 WHERE "index"=1`},
		{"missing_metadata", `DROP TABLE action_task_step`},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, database, text, _ := forgejoStepLogFixture(t)
			if err := database.Exec(test.mutation).Error; err != nil {
				t.Fatal(err)
			}
			archive, err := backend.RunLogs(t.Context(), "ci/project", "17")
			if err != nil {
				t.Fatal(err)
			}
			files := readStepLogArchive(t, archive)
			if len(files) != 1 || files["0_test.txt"] != text {
				t.Fatalf("invalid optional ranges must retain the whole verified log: %v", files)
			}
		})
	}
}

func forgejoStepLogFixture(t *testing.T, compressed ...bool) (Backend, *gorm.DB, string, []string) {
	t.Helper()
	head := strings.Repeat("a", 40)
	parts := []string{"setup 😅\r\n", "install passed\r\n", "test passed inside failed step\r\n", "assertion failed\r\n", "post cleanup"}
	text := strings.Join(parts, "")
	var indexes []byte
	offset := int64(0)
	for _, part := range parts {
		indexes = binary.AppendVarint(indexes, offset)
		offset += int64(len(part))
	}
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "forgejo.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE repository (id integer primary key, owner_name text, lower_name text)`,
		`CREATE TABLE action_run (id integer primary key, repo_id integer, "index" integer, ref text, commit_sha text, event text)`,
		`CREATE TABLE action_run_job (id integer primary key, run_id integer, repo_id integer, task_id integer, name text, commit_sha text)`,
		`CREATE TABLE action_task (id integer primary key, repo_id integer, commit_sha text, log_filename text, log_in_storage boolean, log_expired boolean, status integer, log_length integer, log_size integer, log_indexes blob)`,
		`CREATE TABLE action_task_step (id integer primary key, task_id integer, repo_id integer, name text, "index" integer, status integer, started integer, stopped integer, log_index integer, log_length integer)`,
		`INSERT INTO repository VALUES (1,'ci','project')`,
		`INSERT INTO action_run VALUES (17,1,9,'refs/heads/feature','` + head + `','push')`,
		`INSERT INTO action_run_job VALUES (31,17,1,41,'test','` + head + `')`,
		`INSERT INTO action_task_step VALUES (1,41,1,'Install',0,1,0,0,1,1),(2,41,1,'Test',1,2,0,0,2,2)`,
	} {
		if err := database.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Exec(`INSERT INTO action_task VALUES (41,1,?,'job.log',true,false,2,5,?,?)`, head, len(text), indexes).Error; err != nil {
		t.Fatal(err)
	}
	logs := t.TempDir()
	if err := os.WriteFile(filepath.Join(logs, "job.log"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(compressed) > 0 && compressed[0] {
		if output, err := exec.Command("zstd", "-q", filepath.Join(logs, "job.log"), "-o", filepath.Join(logs, "job.log.zst")).CombinedOutput(); err != nil {
			t.Fatalf("compress fixture: %v: %s", err, output)
		}
		if err := database.Exec(`UPDATE action_task SET log_filename='job.log.zst' WHERE id=41`).Error; err != nil {
			t.Fatal(err)
		}
	}
	handler, err := providerlogbridge.New(providerlogbridge.Config{DB: database, ActionsLogDir: logs, ServiceToken: "logs-only", AllowedCIDRs: []string{"127.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Use(providerlogbridge.CaptureSocketPeer)
	router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/tasks/{task}", handler.ServeHTTP)
	router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/runs/{run}/jobs", handler.ServeHTTP)
	bridge := httptest.NewServer(router)
	t.Cleanup(bridge.Close)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token provider-only" {
			t.Error("unexpected provider credential")
		}
		run := forgejoFixtureRun()
		run["status"] = "failure"
		switch r.URL.Path {
		case "/api/v1/repos/ci/project/actions/runs/17":
			_ = json.NewEncoder(w).Encode(run)
		case "/api/v1/repos/ci/project/actions/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{run}})
		case "/api/v1/repos/ci/project/actions/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{map[string]any{"id": 41, "run_number": 9, "workflow_id": "ci.yml", "name": "test", "head_sha": head, "status": "failure", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z"}}})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: bridge.URL, TokenFile: "fixture", AllowHTTP: true}}, "provider-only", "logs-only")
	if err != nil {
		t.Fatal(err)
	}
	return backend, database, text, parts
}

func readStepLogArchive(t *testing.T, data []byte) map[string]string {
	t.Helper()
	if err := ValidateLogArchive(data); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name] = string(body)
	}
	return files
}

func TestForgejoStepLogWireValidationFallsBackWithoutLosingText(t *testing.T) {
	job := Job{Name: "test", Status: "completed", Conclusion: "failure"}
	text := []byte("setup\npass\nfail\npost\n")
	valid := func() providerlogprotocol.StepLogs {
		return providerlogprotocol.StepLogs{Schema: providerlogprotocol.StepLogsSchema, ByteLength: int64(len(text)), Steps: []providerlogprotocol.StepLog{
			{Name: "Install", Number: 1, Status: "completed", Conclusion: "success", Ranges: []providerlogprotocol.ByteRange{{Start: 6, End: 11}}},
			{Name: "Test", Number: 2, Status: "completed", Conclusion: "failure", Ranges: []providerlogprotocol.ByteRange{{Start: 11, End: 16}}},
			{Name: providerlogprotocol.UnattributedStepName, Number: 3, Status: "completed", Conclusion: "failure", Ranges: []providerlogprotocol.ByteRange{{Start: 0, End: 6}, {Start: 16, End: 21}}, Diagnostic: "unattributed"},
		}}
	}
	layout := valid()
	raw, err := json.Marshal(layout)
	if err != nil {
		t.Fatal(err)
	}
	if entries := forgejoStepLogEntries(job, "test", text, raw); len(entries) != 3 || string(entries[2].body) != "setup\npost\n" {
		t.Fatalf("valid exact layout: %+v", entries)
	}
	for _, test := range []struct {
		name string
		edit func(*providerlogprotocol.StepLogs)
	}{
		{"wrong_schema", func(v *providerlogprotocol.StepLogs) { v.Schema = "unknown" }},
		{"wrong_length", func(v *providerlogprotocol.StepLogs) { v.ByteLength-- }},
		{"missing_ranges", func(v *providerlogprotocol.StepLogs) { v.Steps = v.Steps[:2] }},
		{"overlap", func(v *providerlogprotocol.StepLogs) { v.Steps[1].Ranges[0].Start = 6 }},
		{"out_of_bounds", func(v *providerlogprotocol.StepLogs) { v.Steps[1].Ranges[0].End = 99 }},
		{"middle_of_line", func(v *providerlogprotocol.StepLogs) { v.Steps[1].Ranges[0].Start++ }},
		{"duplicate_step", func(v *providerlogprotocol.StepLogs) { v.Steps[1].Number = 1 }},
		{"unfinished_step", func(v *providerlogprotocol.StepLogs) { v.Steps[1].Status = "in_progress" }},
		{"no_failed_step", func(v *providerlogprotocol.StepLogs) { v.Steps[1].Conclusion = "success" }},
		{"fake_diagnostic", func(v *providerlogprotocol.StepLogs) { v.Steps[2].Name = "Pretend provider step" }},
		{"wrong_diagnostic_conclusion", func(v *providerlogprotocol.StepLogs) { v.Steps[2].Conclusion = "success" }},
		{"unknown_diagnostic", func(v *providerlogprotocol.StepLogs) { v.Steps[2].Diagnostic = "unknown" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := valid()
			test.edit(&value)
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if entries := forgejoStepLogEntries(job, "test", text, raw); len(entries) != 0 {
				t.Fatalf("partial or inconsistent layout became a step ZIP: %+v", entries)
			}
		})
	}
	for _, raw := range []json.RawMessage{nil, []byte(`null`), []byte(`"malformed optional metadata"`), []byte(`{"steps": "wrong type"}`)} {
		if entries := forgejoStepLogEntries(job, "test", text, raw); len(entries) != 0 {
			t.Fatalf("legacy/malformed optional metadata must retain job mode: %+v", entries)
		}
	}
}

func TestGHLogNameMatchesOfficialUTF16Rules(t *testing.T) {
	for _, test := range []struct{ name, want string }{
		{"group / lint: linux", "group  lint linux"},
		{strings.Repeat("a", 89) + "😅suffix", strings.Repeat("a", 89) + "�"},
		{strings.Repeat("a", 88) + "😅suffix", strings.Repeat("a", 88) + "😅"},
		{"  " + strings.Repeat("a", 88) + "suffix", strings.Repeat("a", 88)},
	} {
		if got := ghLogJobName(test.name); got != test.want {
			t.Errorf("gh job filename = %q, want %q", got, test.want)
		}
	}
	if safeGHLogJobName("unsafe\\path") || safeGHLogJobName("nul\x00name") {
		t.Error("unsafe ZIP name must use the job-ID API fallback")
	}
}
