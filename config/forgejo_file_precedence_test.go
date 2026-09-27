package config

import "testing"

func TestForgejoFileOverridesInactiveEnvironmentRequirements(t *testing.T) {
	t.Setenv("DB_DSN", "sqlite::memory:")
	t.Setenv("FORGEJO_INTEGRATION_ENABLED", "1")
	t.Setenv("FORGEJO_INTEGRATION_BASE_URL", "")
	t.Setenv("FORGEJO_INTEGRATION_TOKEN", "")
	t.Setenv("FORGEJO_INTEGRATION_TOKEN_FILE", "")
	t.Setenv("AGS_INTEGRATIONS_CONFIG", "/operator-selected/integrations.yaml")
	// File loading/validation belongs to composition; legacy environment cannot
	// reject a file-owned integration before its enabled switch has been read.
	if _, err := New(); err != nil {
		t.Fatal("inactive env settings overrode selected file", err)
	}
	t.Setenv("AGS_INTEGRATIONS_CONFIG", "")
	if _, err := New(); err == nil {
		t.Fatal("active environment-only integration skipped validation")
	}
}
