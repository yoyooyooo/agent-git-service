package gitlabintegration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRepoFlowEvidenceJSONLRecordStatusAndHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	integration := New(Config{
		Repos: map[string]RepoMapping{
			"example-owner/demo": {
				ProjectPath: "backup/demo",
				RepoFlowEvidence: RepoFlowEvidenceConfig{
					Enabled: true,
					Store: RepoFlowEvidenceStoreConfig{
						Type:       "jsonl",
						Path:       path,
						MaxRecords: 2,
					},
				},
			},
		},
	}, nil)

	for _, rec := range []map[string]any{
		{"action": "env.projection", "env": "dev", "state": "projected", "source_sha": "111"},
		{"action": "env.projection", "env": "uat", "state": "failed", "source_sha": "222"},
		{"action": "env.projection", "env": "uat", "state": "projected", "source_sha": "333"},
	} {
		if _, handled, err := integration.RecordRepoFlowEvidence(context.Background(), "example-owner/demo", rec); err != nil || !handled {
			t.Fatalf("RecordRepoFlowEvidence handled=%v err=%v", handled, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	if lines := nonEmptyLines(string(data)); lines != 2 {
		t.Fatalf("expected bounded 2 records, got %d: %s", lines, string(data))
	}

	status, handled := integration.RepoFlowEvidenceStatus("example-owner/demo")
	if !handled || status.State != "available" || status.Type != "jsonl" || status.Records != 2 || status.MaxRecords != 2 {
		t.Fatalf("status handled=%v status=%#v", handled, status)
	}

	history, handled, err := integration.ListRepoFlowEvidence(context.Background(), "example-owner/demo", "uat", 10)
	if err != nil || !handled {
		t.Fatalf("ListRepoFlowEvidence handled=%v err=%v", handled, err)
	}
	if history.State != "available" || history.Source != "ags-jsonl" || len(history.Entries) != 2 {
		t.Fatalf("history=%#v", history)
	}
	if history.Entries[0]["source_sha"] != "333" || history.Entries[1]["source_sha"] != "222" {
		t.Fatalf("entries not newest-first or not filtered: %#v", history.Entries)
	}
}

func TestRepoFlowEvidenceDisabledIsUnhandled(t *testing.T) {
	integration := New(Config{Repos: map[string]RepoMapping{"example-owner/demo": {ProjectPath: "backup/demo"}}}, nil)
	if _, handled := integration.RepoFlowEvidenceStatus("example-owner/demo"); handled {
		t.Fatal("expected disabled evidence to be unhandled")
	}
	if _, handled, err := integration.RecordRepoFlowEvidence(context.Background(), "example-owner/demo", map[string]any{"env": "uat"}); handled || err != nil {
		t.Fatalf("RecordRepoFlowEvidence handled=%v err=%v", handled, err)
	}
}

func nonEmptyLines(s string) int {
	count := 0
	for _, r := range s {
		if r == '\n' {
			count++
		}
	}
	return count
}
