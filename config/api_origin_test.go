package config

import "testing"

func TestAdvertisedAPIOriginDoesNotChangeGitBase(t *testing.T) {
	t.Setenv("DB_DSN", "sqlite::memory:")
	t.Setenv("BASE_URL", "http://git.example.test:6666")
	t.Setenv("AGS_API_BASE_URL", "https://api.example.test/")
	cfg, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIBaseURL != "https://api.example.test" || cfg.BaseURL != "http://git.example.test:6666" {
		t.Fatalf("origins were conflated: %+v", struct{ Git, API string }{cfg.BaseURL, cfg.APIBaseURL})
	}
	t.Setenv("AGS_API_BASE_URL", "")
	cfg, err = New()
	if err != nil || cfg.APIBaseURL != "" {
		t.Fatal("optional API origin did not preserve legacy default", err)
	}
}
func TestAdvertisedAPIOriginRejectsCredentialsPathsAndNonHTTP(t *testing.T) {
	t.Setenv("DB_DSN", "sqlite::memory:")
	for _, value := range []string{"https://user:secret@api.example.test", "https://api.example.test/path", "https://api.example.test/?token=secret", "https://api.example.test/#fragment", "file:///tmp/api", "relative.example.test", "https://api.example.test/%2f"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("AGS_API_BASE_URL", value)
			if _, err := New(); err == nil {
				t.Fatal("invalid advertised API origin accepted")
			}
		})
	}
}
