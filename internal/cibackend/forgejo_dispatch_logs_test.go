package cibackend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestForgejoDetachedDispatchLogsThroughRealBridge(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, test := range []struct {
		name, event, trigger, storedRef, wantEvent string
	}{
		{"legacy_empty_event", "", "workflow_dispatch", head, "workflow_dispatch"},
		{"explicit_dispatch_event", "workflow_dispatch", "", head, "workflow_dispatch"},
		{"commit_named_dispatch_branch", "", "workflow_dispatch", "refs/heads/" + head, "workflow_dispatch"},
		{"commit_named_push_branch", "push", "workflow_dispatch", "refs/heads/" + head, "push"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := forgejoFixtureRun()
			raw["prettyref"], raw["event"], raw["trigger_event"], raw["status"] = head, test.event, test.trigger, "failure"
			backend, database, text, parts := forgejoStepLogRunFixture(t, raw)
			if err := database.Exec("UPDATE action_run SET ref=?, event=? WHERE id=17", test.storedRef, test.wantEvent).Error; err != nil {
				t.Fatal(err)
			}
			run, err := backend.Run(t.Context(), "ci/project", "17")
			if err != nil {
				t.Fatal(err)
			}
			if run.Branch != head || run.HeadSHA != head || run.Event != test.wantEvent || len(run.PullRequests) != 0 {
				t.Errorf("run identity was not preserved: %+v", run)
			}
			jobs, err := backend.Jobs(t.Context(), "ci/project", "17")
			if err != nil || jobs.Total != 1 || len(jobs.Items) != 1 || jobs.Items[0].Key != "17:41" {
				t.Fatalf("only the real current task is a job: %+v %v", jobs, err)
			}
			t.Run("steps", func(t *testing.T) {
				steps, err := backend.(StepBackend).JobSteps(t.Context(), "ci/project", run, jobs.Items[0])
				if err != nil || len(steps) != 3 {
					t.Fatalf("exact detached step binding: %+v %v", steps, err)
				}
				if steps[0].Name != "Install" || steps[0].Conclusion != "success" ||
					steps[1].Name != "Test" || steps[1].Conclusion != "failure" ||
					steps[2].Name != "AGS: output outside recorded steps" || steps[2].Conclusion != "failure" {
					t.Fatalf("provider and diagnostic steps changed: %+v", steps)
				}
			})
			t.Run("job_log", func(t *testing.T) {
				body, err := backend.JobLogs(t.Context(), "ci/project", "17:41")
				if err != nil || string(body) != text {
					t.Fatalf("bound whole-job log: %q %v", body, err)
				}
			})
			t.Run("run_logs", func(t *testing.T) {
				body, err := backend.RunLogs(t.Context(), "ci/project", "17")
				if err != nil {
					t.Fatal(err)
				}
				want := map[string]string{
					"test/1_step.txt": parts[1],
					"test/2_step.txt": parts[2] + parts[3],
					"test/3_step.txt": parts[0] + parts[4],
				}
				if actual := readStepLogArchive(t, body); !reflect.DeepEqual(actual, want) {
					t.Fatalf("step archive must preserve every byte and boundary: %v", actual)
				}
			})
		})
	}
}

func TestForgejoDetachedDispatchDoesNotInferMissingTrigger(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, trigger := range []string{"", "push", "unknown", " workflow_dispatch", "workflow_dispatch "} {
		t.Run("trigger="+trigger, func(t *testing.T) {
			raw := forgejoFixtureRun()
			raw["prettyref"], raw["event"], raw["trigger_event"] = head, "", trigger
			backend, database, _, _ := forgejoStepLogRunFixture(t, raw)
			if err := database.Exec("UPDATE action_run SET ref=?, event='workflow_dispatch' WHERE id=17", head).Error; err != nil {
				t.Fatal(err)
			}
			run, err := backend.Run(t.Context(), "ci/project", "17")
			if err != nil || run.Event != "" {
				t.Fatalf("unknown trigger must not supply a dispatch event: %+v %v", run, err)
			}
			job, err := backend.Job(t.Context(), "ci/project", "17:41")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backend.(StepBackend).JobSteps(t.Context(), "ci/project", run, job); err == nil {
				t.Fatal("steps accepted without a canonical dispatch event")
			}
			if _, err := backend.JobLogs(t.Context(), "ci/project", "17:41"); err == nil {
				t.Fatal("logs accepted without a canonical dispatch event")
			}
		})
	}
}

func TestForgejoDetachedDispatchAdapterRejectsForeignBindings(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, test := range []struct {
		name string
		edit func(*Run, map[string]any)
	}{
		{"multiple_pull_requests", func(r *Run, _ map[string]any) { r.PullRequests = []int{42, 43} }},
		{"push_event", func(r *Run, v map[string]any) { r.Event, v["event"] = "push", "push" }},
		{"different_branch", func(r *Run, _ map[string]any) { r.Branch = "feature" }},
		{"different_provider_ref", func(_ *Run, v map[string]any) { v["provider_ref"] = strings.Repeat("b", 40) }},
		{"foreign_provider_pr", func(_ *Run, v map[string]any) { v["provider_pr"] = 42 }},
		{"noncanonical_commit", func(r *Run, v map[string]any) {
			r.Branch, r.HeadSHA = strings.Repeat("A", 40), strings.Repeat("A", 40)
			v["provider_ref"], v["head_sha"] = r.HeadSHA, r.HeadSHA
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := Run{Key: "17", Number: 9, Branch: head, HeadSHA: head, Event: "workflow_dispatch"}
			observation := map[string]any{
				"schema": "ags.internal-provider-log.v1", "repo": "ci/project", "task_id": 41,
				"run_number": 9, "job_name": "test", "head_sha": head, "provider_pr": 0,
				"provider_ref": head, "event": "workflow_dispatch", "text": "fixture\n",
			}
			test.edit(&run, observation)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer logs-only" || r.URL.Path != "/api/internal/provider-logs/repos/ci/project/tasks/41" {
					t.Error("unexpected bridge request")
				}
				_ = json.NewEncoder(w).Encode(observation)
			}))
			defer server.Close()
			backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: server.URL, TokenFile: "fixture", AllowHTTP: true,
				LogBridge: &LogBridgeConfig{URL: server.URL, TokenFile: "fixture", AllowHTTP: true}}, "provider-only", "logs-only")
			if err != nil {
				t.Fatal(err)
			}
			job := Job{Key: "17:41", Run: "17", Name: "test", HeadSHA: run.HeadSHA}
			if _, err := backend.(*Forgejo).boundJobObservation(t.Context(), "ci/project", run, job, false); err == nil {
				t.Fatal("foreign detached binding accepted")
			}
		})
	}
}
