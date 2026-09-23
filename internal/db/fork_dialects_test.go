package db

import (
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestForkSQLiteLegacyWikiColumnCleanupPreservesRowsAndIndexes(t *testing.T) {
	database, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "legacy.db")+"?_foreign_keys=on"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	for _, statement := range []string{
		`CREATE TABLE wiki_pages (page_id TEXT PRIMARY KEY, repository_id INTEGER NOT NULL, slug TEXT NOT NULL, slug_ci_v1 TEXT)`,
		`CREATE UNIQUE INDEX idx_wiki_pages_repo_slug_ci ON wiki_pages(repository_id, slug_ci_v1)`,
		`CREATE INDEX idx_wiki_pages_repo_prefix ON wiki_pages(repository_id, slug_ci_v1)`,
		`CREATE INDEX retained_slug_index ON wiki_pages(slug)`,
		`INSERT INTO wiki_pages VALUES ('page-1', 1, 'Docs/Readme', 'docs/readme')`,
		`CREATE TABLE wiki_dependents (id INTEGER PRIMARY KEY, page_id TEXT REFERENCES wiki_pages(page_id) ON DELETE CASCADE)`,
		`INSERT INTO wiki_dependents VALUES (1, 'page-1')`,
		`CREATE TABLE wiki_search_documents (id INTEGER PRIMARY KEY, repository_id INTEGER NOT NULL, slug TEXT, slug_ci_v1 TEXT)`,
		`CREATE INDEX idx_wiki_search_repo_slug_ci ON wiki_search_documents(repository_id, slug_ci_v1)`,
		`INSERT INTO wiki_search_documents VALUES (1, 1, 'Docs/Readme', 'docs/readme')`,
		`CREATE TABLE wiki_page_links (id INTEGER PRIMARY KEY, dst_slug_ci TEXT)`,
		`CREATE INDEX idx_wiki_links_dst_string ON wiki_page_links(dst_slug_ci)`,
		`INSERT INTO wiki_page_links VALUES (1, 'Docs/Readme')`,
	} {
		if err := database.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := MigrateWikiSlugColumnsBeforeAutoMigrate(database); err != nil {
			t.Fatal(err)
		}
		if err := MigrateWikiSlugColumns(database); err != nil {
			t.Fatal(err)
		}
	}
	for _, column := range obsoleteWikiSlugColumns {
		if database.Migrator().HasColumn(column.table, column.column) {
			t.Fatalf("legacy column remains: %s.%s", column.table, column.column)
		}
	}
	var slug, target string
	if err := database.Raw(`SELECT slug FROM wiki_pages WHERE page_id = 'page-1'`).Scan(&slug).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Raw(`SELECT dst_slug FROM wiki_page_links WHERE id = 1`).Scan(&target).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := database.Table("wiki_dependents").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if slug != "Docs/Readme" || target != slug || count != 1 {
		t.Fatalf("migration changed data: slug=%q target=%q dependents=%d", slug, target, count)
	}
	if !database.Migrator().HasIndex("wiki_pages", "retained_slug_index") {
		t.Fatal("unrelated index was dropped")
	}
	correct, err := hasCorrectIndexShape(database, "wiki_pages", "idx_wiki_pages_repo_prefix", []string{"repository_id", "slug"})
	if err != nil || !correct {
		t.Fatalf("prefix index was not rebuilt correctly: %v, %v", correct, err)
	}
}

func TestForkDialectDispatchRetainsDeployedBackends(t *testing.T) {
	for _, tc := range []struct{ dsn, dialect string }{
		{":memory:", "sqlite"}, {"file:local.db", "sqlite"},
		{"sqlite://local.db", "sqlite"}, {"sqlite:local.db", "sqlite"},
		{"postgres://user:pass@127.0.0.1/db", "postgres"},
		{"postgresql://user:pass@127.0.0.1/db", "postgres"},
		{"root:@tcp(127.0.0.1:45400)/ags?parseTime=true", "mysql"},
	} {
		t.Run(tc.dialect+"/"+tc.dsn[:4], func(t *testing.T) {
			dialector, name := DialectorForDSN(tc.dsn)
			if name != tc.dialect || dialector.Name() != tc.dialect {
				t.Fatalf("dialect=%q, want %q", name, tc.dialect)
			}
		})
	}
}

func TestForkSQLiteMigrationRetainsIdentityAndUpstreamWikiTables(t *testing.T) {
	database, err := Init("file:" + filepath.Join(t.TempDir(), "fork.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	for _, model := range []any{&WikiGitRepairObligation{}, &WikiSearchProjectionTask{}, &AccessGrant{}, &AuthorityBoundaryReceipt{}} {
		if !database.Migrator().HasTable(model) {
			t.Fatalf("missing merged model %T", model)
		}
	}
	identity := "0123456789abcdef0123456789abcdef"
	repo := Repository{Name: "preserved", FullName: "fork/preserved", GitStorageID: &identity}
	if err := database.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(database); err != nil {
		t.Fatal(err)
	}
	var observed Repository
	if err := database.First(&observed, repo.ID).Error; err != nil {
		t.Fatal(err)
	}
	if observed.GitStorageID == nil || *observed.GitStorageID != identity {
		t.Fatal("migration changed replica identity")
	}
	if IsTiDB(database) || SupportsTiDBFullText(database) {
		t.Fatal("SQLite advertised TiDB-only features")
	}
}
