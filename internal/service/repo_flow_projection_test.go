package service

import (
	"context"
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
)

func TestDispatchRepoFlowEnvProjectionProjectsEnvRef(t *testing.T) {
	var gotRefspec string
	svc := &Service{GitLabIntegration: gitlabintegration.New(gitlabintegration.Config{
		Enabled: true,
		BaseURL: "https://gitlab.example.local",
		Token:   "secret-token",
		Repos: map[string]gitlabintegration.RepoMapping{
			"example-owner/demo": {ProjectPath: "backup/demo", EnvBranches: map[string]string{"uat": "uat"}},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		gotRefspec = refspec
		return nil
	})}

	res, err := svc.DispatchRepoFlowEnvProjection(context.Background(), RepoFlowEnvProjectionRequest{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/tmp/demo.git",
		Ref:          "refs/heads/env/uat",
		Before:       "old",
		After:        "abc123",
	})
	if err != nil {
		t.Fatalf("DispatchRepoFlowEnvProjection: %v", err)
	}
	if !res.Handled || res.State != "projected" || res.Env != "uat" || res.TargetBranch != "uat" || res.PushedSHA != "abc123" {
		t.Fatalf("result=%#v", res)
	}
	if gotRefspec != "+abc123:refs/heads/uat" {
		t.Fatalf("refspec=%q", gotRefspec)
	}
}

func TestDispatchRepoFlowEnvProjectionRecordsCentralStatus(t *testing.T) {
	gdb := newRepoFlowProjectionTestDB(t)
	repo := db.Repository{FullName: "example-owner/demo", Name: "demo", DefaultBranch: "main"}
	if err := gdb.Create(&repo).Error; err != nil {
		t.Fatalf("create repo: %v", err)
	}
	svc := &Service{DB: gdb, GitLabIntegration: gitlabintegration.New(gitlabintegration.Config{
		Enabled: true,
		BaseURL: "https://gitlab.example.local",
		Token:   "secret-token",
		Repos: map[string]gitlabintegration.RepoMapping{
			"example-owner/demo": {ProjectPath: "backup/demo", EnvBranches: map[string]string{"uat": "uat"}},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error { return nil })}

	_, err := svc.DispatchRepoFlowEnvProjection(context.Background(), RepoFlowEnvProjectionRequest{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/tmp/demo.git",
		Ref:          "refs/heads/env/uat",
		After:        "abc123",
	})
	if err != nil {
		t.Fatalf("DispatchRepoFlowEnvProjection: %v", err)
	}
	status, err := svc.GetRepoFlowEnvProjection(context.Background(), "example-owner/demo", "uat")
	if err != nil {
		t.Fatalf("GetRepoFlowEnvProjection: %v", err)
	}
	if status.State != "projected" || status.Env != "uat" || status.SourceSHA != "abc123" || status.TargetBranch != "uat" {
		t.Fatalf("status=%#v", status)
	}
}

func TestDispatchRepoFlowEnvProjectionRecordsAGSEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	svc := &Service{GitLabIntegration: gitlabintegration.New(gitlabintegration.Config{
		Enabled: true,
		BaseURL: "https://gitlab.example.local",
		Token:   "secret-token",
		Repos: map[string]gitlabintegration.RepoMapping{
			"example-owner/demo": {
				ProjectPath: "backup/demo",
				EnvBranches: map[string]string{"uat": "uat"},
				RepoFlowEvidence: gitlabintegration.RepoFlowEvidenceConfig{
					Enabled: true,
					Store: gitlabintegration.RepoFlowEvidenceStoreConfig{
						Type:       "jsonl",
						Path:       path,
						MaxRecords: 10,
					},
				},
			},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error { return nil })}

	_, err := svc.DispatchRepoFlowEnvProjection(context.Background(), RepoFlowEnvProjectionRequest{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/tmp/demo.git",
		Ref:          "refs/heads/env/uat",
		After:        "abc123",
	})
	if err != nil {
		t.Fatalf("DispatchRepoFlowEnvProjection: %v", err)
	}
	history, err := svc.ListRepoFlowEvidenceHistory(context.Background(), "example-owner/demo", "uat", 10)
	if err != nil {
		t.Fatalf("ListRepoFlowEvidenceHistory: %v", err)
	}
	if len(history.Entries) != 2 {
		t.Fatalf("expected pending+projected evidence records, got %#v", history.Entries)
	}
	if history.Entries[0]["state"] != "projected" || history.Entries[0]["pushed_sha"] != "abc123" {
		t.Fatalf("latest evidence=%#v", history.Entries[0])
	}
}

func TestDispatchRepoFlowEnvProjectionSkipsNonEnvRef(t *testing.T) {
	called := false
	svc := &Service{GitLabIntegration: gitlabintegration.New(gitlabintegration.Config{Enabled: true, BaseURL: "https://gitlab.example.local", Token: "secret-token"}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		called = true
		return nil
	})}
	res, err := svc.DispatchRepoFlowEnvProjection(context.Background(), RepoFlowEnvProjectionRequest{RepoFullName: "example-owner/demo", RepoPath: "/tmp/demo.git", Ref: "refs/heads/main", After: "abc123"})
	if err != nil {
		t.Fatalf("DispatchRepoFlowEnvProjection: %v", err)
	}
	if called || res.State != "skipped" {
		t.Fatalf("called=%v result=%#v", called, res)
	}
}

func newRepoFlowProjectionTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(t.TempDir()+"/test.db"), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := gdb.AutoMigrate(&db.Repository{}, &db.RepoRedirect{}, &db.RepoFlowEnvProjection{}); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return gdb
}
