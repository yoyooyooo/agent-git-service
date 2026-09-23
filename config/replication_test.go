package config

import "testing"

func TestReplicationConfigurationIsExplicitOptIn(t *testing.T) {
	t.Setenv("DB_DSN", "file:config-test.db")
	t.Setenv("AGS_REPLICATION_CONFIG_FILE", "")
	cfg, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReplicationConfigFile != "" {
		t.Fatal("replication enabled by default")
	}
	t.Setenv("AGS_REPLICATION_CONFIG_FILE", "/operator/primary-replication.json")
	cfg, err = New()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReplicationConfigFile != "/operator/primary-replication.json" {
		t.Fatal("explicit replication configuration not loaded")
	}
}
