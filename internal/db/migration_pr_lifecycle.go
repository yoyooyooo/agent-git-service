package db

import "gorm.io/gorm"

// MigratePRLifecycleColumns is additive and idempotent for deployed databases.
// Zero generation preserves pre-reopen terminal delivery identities; NULL
// watermark means no provider-clock fact has been accepted yet. Existing PRs
// are not assigned an AGS-clock watermark during migration.
func MigratePRLifecycleColumns(database *gorm.DB) error {
	if database == nil || !database.Migrator().HasTable(&PullRequest{}) {
		return nil
	}
	for _, field := range []string{"LifecycleGeneration", "ForgejoReopenPending", "ForgejoLifecycleAt"} {
		if !database.Migrator().HasColumn(&PullRequest{}, field) {
			if err := database.Migrator().AddColumn(&PullRequest{}, field); err != nil {
				return err
			}
		}
	}
	return nil
}
