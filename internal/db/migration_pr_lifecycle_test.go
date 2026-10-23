package db

import (
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"path/filepath"
	"testing"
)

func checkLegacyPRLifecycle(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	require.NoError(t, gdb.Exec(`CREATE TABLE pull_requests (id INTEGER PRIMARY KEY, state VARCHAR(20), merged BOOLEAN)`).Error)
	require.NoError(t, gdb.Exec(`INSERT INTO pull_requests (id,state,merged) VALUES (1,'open',false),(2,'closed',true)`).Error)
	require.NoError(t, MigratePRLifecycleColumns(gdb))
	require.NoError(t, MigratePRLifecycleColumns(gdb))
	var rows []PullRequest
	require.NoError(t, gdb.Order("id").Find(&rows).Error)
	require.Len(t, rows, 2)
	require.Equal(t, "open", rows[0].State)
	require.True(t, rows[1].Merged)
	for _, row := range rows {
		require.Zero(t, row.LifecycleGeneration)
		require.False(t, row.ForgejoReopenPending)
		require.Nil(t, row.ForgejoLifecycleAt)
	}
}
func TestPRLifecycleMigrationSQLite(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "legacy.db")), &gorm.Config{})
	require.NoError(t, err)
	checkLegacyPRLifecycle(t, gdb)
}
func TestPRLifecycleMigrationTiDB(t *testing.T) { checkLegacyPRLifecycle(t, openTiDB(t)) }
