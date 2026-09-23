package testdb

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"gorm.io/gorm"
)

var discardOnReturn sync.Map // *sql.DB -> struct{}; only schema-pool fixtures

// DiscardOnReturn prevents a deliberately corrupted fixture or exhausted auto-ID
// allocator from contaminating the next test. Ordinary tests retain pool reuse.
func DiscardOnReturn(t testing.TB, database *gorm.DB) {
	t.Helper()
	pool, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	discardOnReturn.Store(pool, struct{}{})
}

func shouldDiscard(pool *sql.DB) bool {
	_, discard := discardOnReturn.LoadAndDelete(pool)
	return discard
}

// MutateWithoutForeignKeys installs intentional orphan rows for fail-closed
// recovery tests. One checked-out connection owns the temporary SQL setting;
// FK enforcement is restored before service code sees the malformed row.
func MutateWithoutForeignKeys(t testing.TB, database *gorm.DB, mutate func(*gorm.DB) error) {
	t.Helper()
	if database.Dialector.Name() != "mysql" {
		t.Fatal("orphan fixture requires the isolated TiDB test database")
	}
	pool, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var enabled int
	if err := conn.QueryRowContext(ctx, "SELECT @@SESSION.foreign_key_checks").Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("foreign key guard before fixture: enabled=%d err=%v", enabled, err)
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION FOREIGN_KEY_CHECKS=0"); err != nil {
		t.Fatal(err)
	}
	result := func() (result error) {
		defer func() {
			_, restoreErr := conn.ExecContext(ctx, "SET SESSION FOREIGN_KEY_CHECKS=1")
			result = errors.Join(result, restoreErr)
		}()
		local := database.Session(&gorm.Session{NewDB: true}).WithContext(ctx)
		local.Statement.ConnPool = conn
		return mutate(local)
	}()
	if err := conn.QueryRowContext(ctx, "SELECT @@SESSION.foreign_key_checks").Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("foreign key guard was not restored: enabled=%d err=%v", enabled, err)
	}
	if result != nil {
		t.Fatalf("install deliberately orphaned fixture: %v", result)
	}
}
