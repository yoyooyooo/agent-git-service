package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/gitstore"
	"github.com/ngaut/agent-git-service/internal/service"
	"gorm.io/gorm"
)

func TestCreatePRProjectionReceiptKeepsNativeSuccessAndHonorsSelection(t *testing.T) {
	directory := t.TempDir()
	database, err := db.Init("sqlite:" + filepath.Join(directory, "receipt.sqlite") + "?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	git, err := gitstore.New(filepath.Join(directory, "repos"))
	if err != nil {
		t.Fatal(err)
	}
	svc := &service.Service{DB: database, Git: git, BaseURL: "https://primary.example.test"}
	actor := db.User{Login: "fixture", Name: "Fixture", Type: db.TypeUser, UserKind: db.UserKindHuman, Status: db.UserStatusActive}
	if err = database.Create(&actor).Error; err != nil {
		t.Fatal(err)
	}
	ctx := service.ContextWithUser(context.Background(), actor)
	repository, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: actor.Login, Name: "receipt", AutoInit: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = git.CreateBranch(ctx, repository.FullName, "feature", "main"); err != nil {
		t.Fatal(err)
	}
	head, err := git.WriteFile(ctx, repository.FullName, "feature", "feature.txt", "fixture change", []byte("fixture\n"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(svc)
	request := gqlRequest{Variables: map[string]any{"input": map[string]any{"repositoryId": gqlID("Repository", repository.ID), "title": "Native receipt", "headRefName": "feature", "baseRefName": "main", "draft": true}}}
	call := func(selection string) map[string]any {
		t.Helper()
		request.Query = "mutation($input:CreatePullRequestInput!){createPullRequest(input:$input){pullRequest{" + selection + "}}}"
		response := server.doCreatePR(ctx, request)
		if response["errors"] != nil {
			t.Fatalf("optional projection changed native result: %#v", response)
		}
		data, ok := response["data"].(map[string]any)
		if !ok {
			t.Fatalf("missing native receipt %#v", response)
		}
		return data["createPullRequest"].(map[string]any)["pullRequest"].(map[string]any)
	}
	selection := "id number url isDraft externalProjections{provider externalNumber externalUrl lastSyncedSha} projectionJob{provider phase status externalNumber externalUrl lastErrorType}"
	created := call(selection)
	if created["url"] != "https://primary.example.test/fixture/receipt/pull/1" || created["isDraft"] != true {
		t.Fatal("canonical draft creation changed", created["url"])
	}
	if rows := created["externalProjections"].([]map[string]any); len(rows) != 0 || created["projectionJob"] != nil {
		t.Fatal("disabled integration manufactured a provider result")
	}
	pr, err := svc.GetPR(ctx, repository.FullName, 1)
	if err != nil {
		t.Fatal(err)
	}
	render := func() map[string]any { return server.prGQL(ctx, pr, request.Query) }
	job := db.PullRequestProjectionJob{PullRequestID: pr.ID, RepositoryID: repository.ID, Provider: service.ProjectionProviderForgejo, Trigger: service.ForgejoProjectionTriggerPullRequest, RepoFullName: repository.FullName, AGSPRNumber: pr.Number, HeadRef: "feature", BaseRef: "main", HeadSHA: head, Phase: service.ForgejoProjectionPhaseQueued}
	if err = database.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	pending := render()
	if pending["id"] != created["id"] || pending["projectionJob"].(map[string]any)["phase"] != "queued" {
		t.Fatal("queued projection missing from native PR presentation")
	}
	if pending["projectionJob"].(map[string]any)["externalUrl"] != "" {
		t.Fatal("pending projection guessed a URL")
	}
	externalURL := "https://forgejo.example.test/ci/receipt/pulls/23"
	if err = svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: repository.ID, Provider: service.ProjectionProviderForgejo, ExternalRepo: "ci/receipt", ExternalNumber: 23, ExternalURL: externalURL, SourceBranch: "feature", TargetBranch: "main", State: service.ProjectionStateOpen, LastSyncedSHA: head}); err != nil {
		t.Fatal(err)
	}
	if err = database.Model(&job).Updates(map[string]any{"phase": service.ForgejoProjectionPhaseProjected, "external_repo": "ci/receipt", "external_number": 23, "external_url": externalURL}).Error; err != nil {
		t.Fatal(err)
	}
	ready := render()
	rows := ready["externalProjections"].([]map[string]any)
	if len(rows) != 1 || rows[0]["externalNumber"] != 23 || rows[0]["externalUrl"] != externalURL || ready["url"] != created["url"] {
		t.Fatal("AGS and provider identities were lost or conflated", rows)
	}
	if ready["projectionJob"].(map[string]any)["externalUrl"] != externalURL {
		t.Fatal("durable job URL missing")
	}
	// The provider row is reused for a rebase action. It must not disappear
	// from the PR-level projection summary merely because its trigger changed.
	if err = database.Model(&job).Updates(map[string]any{"trigger": service.ForgejoProjectionTriggerActionRebase, "action_generation": 2, "phase": service.ForgejoProjectionPhaseFailedRetryable}).Error; err != nil {
		t.Fatal(err)
	}
	actionSummary := render()["projectionJob"].(map[string]any)
	if actionSummary["trigger"] != service.ForgejoProjectionTriggerActionRebase || actionSummary["actionGeneration"] != uint(2) || actionSummary["nextRepairAction"] != "call_projection_retry" {
		t.Fatal("action job hidden or recovery misrepresented", actionSummary)
	}
	// A stock gh query must not eagerly query projection tables just because they
	// exist. Its id/url contract is unchanged and it never receives a fake URL.
	queried := 0
	err = database.Callback().Query().Before("gorm:query").Register("fixture_projection_query", func(tx *gorm.DB) {
		switch tx.Statement.Table {
		case "pull_request_projections", "pull_request_projection_jobs", "workflow_runs", "workflow_run_jobs":
			queried++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = git.CreateBranch(ctx, repository.FullName, "stock", "feature"); err != nil {
		t.Fatal(err)
	}
	request.Variables["input"].(map[string]any)["headRefName"] = "stock"
	stock := call("id url")
	if queried != 0 || stock["url"] != "https://primary.example.test/fixture/receipt/pull/2" || stock["projectionJob"] != nil {
		t.Fatal("stock query acquired provider dependencies")
	}
	if err = database.Model(&job).Updates(map[string]any{"phase": service.ForgejoProjectionPhaseFailedTerminal, "last_error_type": "provider_unavailable", "last_error": "private-provider-debug-detail"}).Error; err != nil {
		t.Fatal(err)
	}
	request.Query = selection
	failed := render()
	payload, _ := json.Marshal(failed)
	if strings.Contains(string(payload), "private-provider-debug-detail") || failed["id"] != created["id"] {
		t.Fatal("failed projection leaked diagnostics or lost native receipt")
	}
	// Model the legal race where projection facts have already arrived by the
	// time a NEW create response is rendered. No real provider is contacted.
	if err = database.Callback().Create().After("gorm:after_create").Register("fixture_completed_projection", func(tx *gorm.DB) {
		if tx.Statement.Table != "pull_requests" {
			return
		}
		newPR, ok := tx.Statement.Dest.(*db.PullRequest)
		if !ok || newPR.HeadRef != "rich" {
			return
		}
		row := db.PullRequestProjection{PullRequestID: newPR.ID, RepositoryID: newPR.RepositoryID, Provider: service.ProjectionProviderForgejo, ExternalRepo: "ci/receipt", ExternalNumber: 77, ExternalURL: "https://forgejo.example.test/ci/receipt/pulls/77", SourceBranch: "rich", TargetBranch: "main", State: service.ProjectionStateOpen, LastSyncedSHA: head}
		_ = tx.AddError(tx.Session(&gorm.Session{NewDB: true}).Create(&row).Error)
	}); err != nil {
		t.Fatal(err)
	}
	if err = git.CreateBranch(ctx, repository.FullName, "rich", "feature"); err != nil {
		t.Fatal(err)
	}
	request.Variables["input"].(map[string]any)["headRefName"] = "rich"
	rich := call(selection)
	richRows := rich["externalProjections"].([]map[string]any)
	if rich["url"] != "https://primary.example.test/fixture/receipt/pull/3" || len(richRows) != 1 || richRows[0]["externalNumber"] != 77 {
		t.Fatal("new creation did not include requested provider mapping", richRows)
	}

	// Unavailable is not a successfully observed empty set. Reading presentation
	// must preserve the already-committed AGS PR and not leak database details.
	err = database.Callback().Query().Before("gorm:query").Register("fixture_projection_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "pull_request_projections" || tx.Statement.Table == "pull_request_projection_jobs" {
			_ = tx.AddError(errors.New("private database diagnostic"))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	unavailable := render()
	payload, _ = json.Marshal(unavailable)
	if unavailable["projectionJob"].(map[string]any)["status"] != "unavailable" || !strings.Contains(string(payload), `"externalProjections":null`) || unavailable["id"] != created["id"] {
		t.Fatal("unavailable observation became empty success or changed PR outcome")
	}
	var count int64
	if err = database.Model(&db.PullRequest{}).Count(&count).Error; err != nil || count != 3 {
		t.Fatal("projection presentation changed native PR count", count, err)
	}
}
