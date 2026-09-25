package service

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/gorm"
)

// Real SQLite constraints and the production cascade, not a mocked SQL sequence.
func TestDeleteRepoCascadeProjectionIntegrity(t *testing.T) {
	for _, mode := range []string{"on", "off"} {
		t.Run("foreign_keys_"+mode, func(t *testing.T) {
			database, err := db.Init("sqlite:" + filepath.Join(t.TempDir(), "cascade.db") + "?_foreign_keys=" + mode)
			if err != nil {
				t.Fatal(err)
			}
			pool, err := database.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pool.Close() })
			owner := db.User{Login: "cascade-owner", Type: db.TypeUser}
			if err := database.Create(&owner).Error; err != nil {
				t.Fatal(err)
			}
			repos := []db.Repository{
				{OwnerID: owner.ID, Name: "remove", FullName: "cascade-owner/remove", DefaultBranch: "main"},
				{OwnerID: owner.ID, Name: "keep", FullName: "cascade-owner/keep", DefaultBranch: "main"},
			}
			now := time.Now().UTC()
			for i := range repos {
				if err := database.Create(&repos[i]).Error; err != nil {
					t.Fatal(err)
				}
				for _, row := range []any{
					&db.ProjectionEvent{Provider: "forgejo", Type: "test_projection", Status: "active", RepositoryID: repos[i].ID, RepoFullName: repos[i].FullName, Ref: "refs/heads/main", OccurredAt: now},
					&db.ProjectionRefState{Provider: "forgejo", Type: "test_projection", Status: "active", RepositoryID: repos[i].ID, RepoFullName: repos[i].FullName, Ref: "refs/heads/main", FirstSeenAt: now, LastSeenAt: now},
				} {
					if err := database.Create(row).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			svc := &Service{DB: database}
			remove := func() error {
				return database.Transaction(func(tx *gorm.DB) error {
					return svc.deleteRepoCascade(tx, repos[0].ID, repos[0].FullName)
				})
			}
			count := func(table string, repoID uint) int64 {
				t.Helper()
				var result int64
				if err := database.Table(table).Where("repository_id = ?", repoID).Count(&result).Error; err != nil {
					t.Fatal(err)
				}
				return result
			}
			// A later failure must roll back deletion of both kinds of projection fact.
			if err := database.Exec(`CREATE TRIGGER abort_repo_delete BEFORE DELETE ON repositories BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`).Error; err != nil {
				t.Fatal(err)
			}
			if err := remove(); err == nil {
				t.Fatal("expected the injected repository delete to fail")
			}
			for _, table := range []string{"projection_events", "projection_ref_states"} {
				if count(table, repos[0].ID) != 1 || count(table, repos[1].ID) != 1 {
					t.Fatalf("rollback changed %s", table)
				}
			}
			if err := database.Exec("DROP TRIGGER abort_repo_delete").Error; err != nil {
				t.Fatal(err)
			}
			if err := remove(); err != nil {
				t.Fatalf("delete repository with projection facts: %v", err)
			}
			for _, table := range []string{"projection_events", "projection_ref_states"} {
				if count(table, repos[0].ID) != 0 || count(table, repos[1].ID) != 1 {
					t.Fatalf("%s cascade crossed ownership or left an orphan", table)
				}
			}
			rows, err := pool.Query("PRAGMA foreign_key_check")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			if rows.Next() {
				t.Fatal("cascade left foreign-key violations")
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
