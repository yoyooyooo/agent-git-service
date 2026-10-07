package transform_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/rest/transform"
)

func TestSeparateAPIOriginPreservesCloneAndBrowserOrigins(t *testing.T) {
	for _, origin := range []string{
		"http://git.example.test:6666",
		"https://git.example.test",
		"https://git.example.test:8443",
	} {
		t.Run(origin, func(t *testing.T) {
			transform.Wrap("http://outer.example.test:6666", func() {
				transform.WrapEndpoints(origin, "https://api.example.test", func() {
					repo := transform.Repo(db.Repository{FullName: "team/project", Name: "project", DefaultBranch: "main"})
					if repo["clone_url"] != origin+"/team/project.git" {
						t.Fatalf("Git clone target changed: %v", repo["clone_url"])
					}
					if repo["url"] != "https://api.example.test/api/v3/repos/team/project" {
						t.Fatalf("API URL leaked Git origin: %v", repo["url"])
					}
					if repo["html_url"] != origin+"/team/project" {
						t.Fatalf("browser URL changed configured scheme or port: %v", repo["html_url"])
					}
					if transform.HTMLBase() != origin {
						t.Fatalf("handler HTML origin differs from repository URL: %s", transform.HTMLBase())
					}
					run := db.WorkflowRun{}
					run.ID = 17
					result := transform.WorkflowRun(run, "team/project")
					if result["html_url"] != origin+"/team/project/actions/runs/17" {
						t.Fatalf("workflow browser URL changed configured origin: %v", result["html_url"])
					}
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
		})
	}
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
