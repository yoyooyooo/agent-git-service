package config

import "testing"

func TestReplicationAuthorityRegistrationConfigIsStrictAndOptIn(t *testing.T) {
	base := Config{DBdsn: "file:unused-test.db"}
	if cfg, err := Normalize(base); err != nil || cfg.ReplicationAuthorityID != "" {
		t.Fatal("default enabled registration", err)
	}
	base.ReplicationAuthorityID = "region-b-authority"
	if cfg, err := Normalize(base); err != nil || cfg.ReplicationAuthorityID != base.ReplicationAuthorityID || cfg.ReplicationConfigFile != "" {
		t.Fatal("registration required a peer listener", err)
	}
	for _, change := range []func(*Config){func(c *Config) { c.ReplicationAuthorityID = "../unsafe" }, func(c *Config) { c.ReplicationAuthorityID = " leading" }, func(c *Config) { c.ControlPlaneDSN = "file:tenant.db" }, func(c *Config) { c.AllowAnyToken = true }} {
		cfg := base
		change(&cfg)
		if _, err := Normalize(cfg); err == nil {
			t.Fatal("unsafe registration configuration accepted")
		}
	}
	t.Setenv("DB_DSN", "file:unused-test.db")
	t.Setenv("AGS_REPLICATION_AUTHORITY_ID", "primary-config-test")
	cfg, err := New()
	if err != nil || cfg.ReplicationAuthorityID != "primary-config-test" {
		t.Fatal("environment entry missing", err)
	}
}
