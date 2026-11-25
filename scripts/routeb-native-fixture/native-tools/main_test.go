package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestFailureDiagnosticUsesClosedStageAndReason(t *testing.T) {
	tests := []struct {
		stage, reason string
		err           error
	}{
		{"verify-catalog", "authority_binding_mismatch", errors.New("authority_binding_mismatch")},
		{"verify-catalog", "catalog_error", errors.New("authority_private_sentinel")},
		{"inspect-elf", "permission_denied", &os.PathError{Op: "open", Path: "private-path-sentinel", Err: os.ErrPermission}},
		{"private-stage-sentinel", "operation_error", errors.New("private-error-sentinel")},
	}
	for _, test := range tests {
		t.Run(test.reason, func(t *testing.T) {
			value := diagnosticFailure(test.stage, test.err)
			raw, err := json.Marshal(value)
			if err != nil || strings.Contains(string(raw), "private") || value.Reason != test.reason {
				t.Fatalf("unsafe or incorrect diagnostic: %s, %v", raw, err)
			}
			if value.Schema != "ags.routeb-fixture-command-error.v1" {
				t.Fatal("unexpected diagnostic schema")
			}
		})
	}
}

func TestFailureDiagnosticPreservesActualCatalogRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog-input.json")
	if err := os.WriteFile(path, []byte(`{"private-input-sentinel":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := run([]string{"verify-catalog", path})
	if err == nil {
		t.Fatal("invalid catalog input was accepted")
	}
	value := diagnosticFailure("verify-catalog", err)
	if value.Stage != "verify-catalog" || value.Reason != "authority_json_shape" {
		t.Fatalf("real catalog refusal was lost: %+v", value)
	}
}

func TestSeedUsesActualSchemaAndRefusesOverwrite(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("fixture seeding deliberately requires its non-root runtime account")
	}
	root := filepath.Join(t.TempDir(), ".local/state/ags-routeb-native/test/ags/data")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.db")
	if err := seed(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private real database missing: %v", err)
	}
	database, err := gorm.Open(sqlite.Open(path+"?mode=ro"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	connection, _ := database.DB()
	defer connection.Close()
	for _, table := range []string{"users", "repositories", "access_grant_invocations", "pull_request_action_intents"} {
		if !database.Migrator().HasTable(table) {
			t.Fatalf("real migration did not create %s", table)
		}
	}
	if err := seed(path); err == nil {
		t.Fatal("existing database was accepted for seeding")
	}
}

func TestELFInspectorDoesNotExecuteInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-an-executable")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	value, err := inspectELF(path)
	if err != nil || value["native"] != false {
		t.Fatalf("unexpected inspection: %v, %v", value, err)
	}
	if _, err := inspectELF(path + ".missing"); err == nil {
		t.Fatal("unreadable input was treated as non-native content")
	}
}
