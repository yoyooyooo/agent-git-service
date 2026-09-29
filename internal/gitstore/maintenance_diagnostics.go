package gitstore

import (
	"errors"
)

// RootInventoryError exposes only a finite reason and a schema location supplied
// by the application. It never wraps database diagnostics, SQL arguments or
// constraint contents. Storage callers must still supply the full inventory.
type RootInventoryError struct {
	Code   string
	Source string
}

func (e *RootInventoryError) Error() string { return "application object inventory failed" }

func rootInventoryDiagnostic(err error) (string, string) {
	var root *RootInventoryError
	if !errors.As(err, &root) {
		return "application_inventory_failed", ""
	}
	switch root.Code {
	case "database_unavailable", "query_failed", "scan_failed", "inventory_budget", "constraints_invalid", "invalid_oid":
	default:
		return "application_inventory_failed", ""
	}
	// Source is a static table.column label, never a repository name or data.
	if len(root.Source) > 96 {
		return "application_inventory_failed", ""
	}
	for _, c := range root.Source {
		if !(c >= 'a' && c <= 'z' || c == '_' || c == '.') {
			return "application_inventory_failed", ""
		}
	}
	return "application_" + root.Code, root.Source
}
