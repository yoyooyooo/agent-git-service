package main

import (
	"os"
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

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
