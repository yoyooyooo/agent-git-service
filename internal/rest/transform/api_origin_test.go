package transform_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/rest/transform"
)

func TestSeparateAPIOriginPreservesCloneAndRedirectOrigins(t *testing.T) {
	transform.Wrap("http://outer.example.test:6666", func() {
		transform.WrapEndpoints("http://git.example.test:6666", "https://api.example.test", func() {
			repo := transform.Repo(db.Repository{FullName: "team/project", Name: "project", DefaultBranch: "main"})
			if repo["clone_url"] != "http://git.example.test:6666/team/project.git" {
				t.Fatal("Git clone target changed")
			}
			if repo["url"] != "https://api.example.test/api/v3/repos/team/project" {
				t.Fatalf("API URL leaked Git origin: %v", repo["url"])
			}
			if repo["html_url"] != "https://git.example.test:6666/team/project" {
				t.Fatal("API option changed browser origin")
			}
			run := db.WorkflowRun{}
			run.ID = 17
			result := transform.WorkflowRun(run, "team/project")
			for _, key := range []string{"url", "jobs_url", "logs_url", "artifacts_url", "cancel_url", "rerun_url"} {
				if !strings.HasPrefix(result[key].(string), "https://api.example.test/api/v3/") {
					t.Fatalf("%s uses wrong API origin: %v", key, result[key])
				}
			}
			if transform.ExtensionAPIBase() != "https://api.example.test/api/ext/v1" {
				t.Fatal("extension links use wrong API origin")
			}
		})
		if transform.APIBase() != "http://outer.example.test:6666/api/v3" {
			t.Fatal("nested endpoint state was not restored")
		}
	})
}
func TestSeparateAPIOriginIsRequestScoped(t *testing.T) {
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	results := make(chan string, 2)
	for _, api := range []string{"https://one.example.test", "https://two.example.test"} {
		go func(api string) {
			transform.WrapEndpoints("http://git.example.test:6666", api, func() { ready.Done(); <-start; results <- transform.APIBase() })
		}(api)
	}
	ready.Wait()
	close(start)
	seen := map[string]bool{<-results: true, <-results: true}
	if !seen["https://one.example.test/api/v3"] || !seen["https://two.example.test/api/v3"] {
		t.Fatal("API origins crossed concurrent requests")
	}
}
