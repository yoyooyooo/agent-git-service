package db

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Migrate includes implicit GORM join tables and later migration helpers, which
// cannot be inferred from only the models in its first AutoMigrate call.
func TestSQLiteMigrationsMatchUpgradeInventory(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "inventory.db")),
		&gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := Migrate(database); err != nil {
		t.Fatal(err)
	}
	var actual []string
	if err := database.Raw("SELECT name FROM sqlite_master WHERE type = 'table' AND name <> 'sqlite_sequence' ORDER BY name").Scan(&actual).Error; err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join("..", "..", "scripts", "upgrade_inventory.py"))
	if err != nil {
		t.Fatal(err)
	}
	literal := regexp.MustCompile(`(?s)KNOWN_TABLES = frozenset\('''([a-z_\s]+)'''\.split\(\)\)`).FindSubmatch(source)
	if len(literal) != 2 {
		t.Fatal("upgrade inventory table declaration changed; review the migration contract")
	}
	expected := strings.Fields(string(literal[1]))
	slices.Sort(expected)
	if !slices.Equal(actual, expected) {
		t.Fatalf("migrated SQLite tables differ from upgrade inventory\nactual: %v\nexpected: %v", actual, expected)
	}
}
