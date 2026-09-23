package db

import (
	"fmt"

	"gorm.io/gorm"
)

// MigratePullRequestActionBoundaryReceipt closes the explicit upgrade path for
// databases created before exact pr.rebase intents carried Boundary Receipt
// lineage. The migration is additive and deliberately leaves historical rows
// empty: an empty provider_effect_status means not_recorded, not not_attempted.
//
// There is no destructive down migration. Removing these lineage columns would
// discard durable authority evidence and is therefore intentionally unsupported.
func MigratePullRequestActionBoundaryReceipt(database *gorm.DB) error {
	if database == nil {
		return fmt.Errorf("migrate pull request action boundary receipt: database is nil")
	}
	migrator := database.Migrator()
	for _, field := range []string{"BoundaryProtocol", "BoundaryReceiptID", "ProviderEffectStatus"} {
		if migrator.HasColumn(&PullRequestActionIntent{}, field) {
			continue
		}
		if err := migrator.AddColumn(&PullRequestActionIntent{}, field); err != nil {
			return fmt.Errorf("add pull request action intent column %s: %w", field, err)
		}
	}
	const index = "idx_pr_action_intent_boundary_receipt"
	if !migrator.HasIndex(&PullRequestActionIntent{}, index) {
		if err := migrator.CreateIndex(&PullRequestActionIntent{}, index); err != nil {
			return fmt.Errorf("create pull request action boundary receipt index: %w", err)
		}
	}
	return nil
}
