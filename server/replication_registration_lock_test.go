package server

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/gorm"
)

// Put a second writer in the gap between the cascade's repository read and
// its first DELETE. This is also a write performed by startup projection repair.
func TestReplicationRegistrationDeleteWithConcurrentProjectionWriter(t *testing.T) {
	f := newRegistrationFixture(t, true)
	// Fail the competing writer immediately rather than waiting while the
	// callback deliberately holds the cascade at its read/write boundary.
	pool, err := sql.Open("sqlite3", f.server.cfg.DBdsn+"?_busy_timeout=0")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var writer *sql.Tx
	var writerErr error
	injected := false
	type cascadeContextKey struct{}
	ctx := context.WithValue(f.ctx, cascadeContextKey{}, true)
	callback := "test:concurrent_projection_writer"
	if err := f.server.deps.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		// Other queries, including live background workers, must not access
		// this synchronous injection's state.
		if tx.Statement.Context.Value(cascadeContextKey{}) != true {
			return
		}
		if injected || tx.Statement.Table != "repositories" || tx.Statement.Selects == nil {
			return
		}
		if _, ok := tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
			return
		}
		injected = true
		writer, writerErr = conn.BeginTx(context.Background(), nil)
		if writerErr == nil {
			_, writerErr = writer.Exec(`INSERT INTO wiki_search_projection_tasks
				(repository_id, slug, kind, generation, created_at, updated_at)
				SELECT repository_id, slug, 'lexical', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
				FROM wiki_pages WHERE deleted_at IS NULL`)
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer f.server.deps.DB.Callback().Query().Remove(callback)
	defer func() {
		if writer != nil {
			_ = writer.Rollback()
		}
	}()

	deleteErr := f.server.deps.SvcDeps.DeleteRepo(ctx, f.repo.FullName)
	if !injected {
		t.Fatal("did not exercise the cascade read/write boundary")
	}
	if deleteErr != nil {
		t.Fatalf("delete with concurrent projection writer: %v (writer: %v)", deleteErr, writerErr)
	}
	var sqliteErr sqlite3.Error
	if !errors.As(writerErr, &sqliteErr) || sqliteErr.Code != sqlite3.ErrBusy {
		t.Fatalf("cascade did not reserve the SQLite writer before reading: %v", writerErr)
	}
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	writer = nil
	var count int64
	if err := f.server.deps.DB.Model(&db.Repository{}).Where("id = ?", f.repo.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("repository survived successful deletion: count=%d err=%v", count, err)
	}
}
