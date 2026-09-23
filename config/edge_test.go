package config

import (
	"strings"
	"testing"
	"time"
)

func TestEdgeConfigIndependentOfPrimary(t *testing.T) {
	t.Setenv("DB_DSN", "deliberately-invalid-and-must-not-be-used")
	t.Setenv("CONTROL_PLANE_DSN", "also-unused")
	t.Setenv("AGS_EDGE_ID", "edge-1")
	t.Setenv("AGS_EDGE_PRIMARY_URL", "https://ags.example.test")
	t.Setenv("AGS_EDGE_CANONICAL_URL", "")
	t.Setenv("AGS_EDGE_LISTEN_ADDR", "")
	t.Setenv("AGS_EDGE_REQUEST_TIMEOUT", "")
	t.Setenv("AGS_EDGE_ALLOW_INSECURE_PRIMARY_HTTP", "")
	cfg, err := NewEdge()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:6667" || cfg.CanonicalURL != cfg.PrimaryURL || cfg.RequestTimeout != 15*time.Minute {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestNormalizeEdge(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*EdgeConfig)
		valid  bool
	}{
		{"valid", func(c *EdgeConfig) {}, true},
		{"missing id", func(c *EdgeConfig) { c.ID = "" }, false},
		{"header id", func(c *EdgeConfig) { c.ID = "edge-fixture\r\nInjected: 1" }, false},
		{"missing upstream", func(c *EdgeConfig) { c.PrimaryURL = "" }, false},
		{"credential URL", func(c *EdgeConfig) { c.PrimaryURL = "https://user:secret@ags.test" }, false},
		{"path", func(c *EdgeConfig) { c.PrimaryURL += "/repo" }, false},
		{"query", func(c *EdgeConfig) { c.PrimaryURL += "?token=secret" }, false},
		{"empty query", func(c *EdgeConfig) { c.PrimaryURL += "?" }, false},
		{"fragment", func(c *EdgeConfig) { c.PrimaryURL += "#secret" }, false},
		{"wrong scheme", func(c *EdgeConfig) { c.PrimaryURL = "file:///tmp/repo" }, false},
		{"plain remote denied", func(c *EdgeConfig) { c.PrimaryURL = "http://primary.test:6666" }, false},
		{"plain explicit", func(c *EdgeConfig) { c.PrimaryURL = "http://primary.test:6666"; c.AllowInsecurePrimaryHTTP = true }, true},
		{"loopback http", func(c *EdgeConfig) { c.PrimaryURL = "http://127.0.0.1:6666" }, true},
		{"bad upstream port", func(c *EdgeConfig) { c.PrimaryURL = "https://ags.test:99999" }, false},
		{"bad listen", func(c *EdgeConfig) { c.ListenAddr = "not-an-address" }, false},
		{"bad port", func(c *EdgeConfig) { c.ListenAddr = ":99999" }, false},
		{"test ephemeral port", func(c *EdgeConfig) { c.ListenAddr = "127.0.0.1:0" }, true},
		{"timeout", func(c *EdgeConfig) { c.RequestTimeout = -time.Second }, false},
		{"canonical credentials", func(c *EdgeConfig) { c.CanonicalURL = "https://token:secret@ags.test" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := EdgeConfig{ID: "edge-1", PrimaryURL: "https://primary.test", CanonicalURL: "https://ags.test"}
			tc.change(&cfg)
			_, err := NormalizeEdge(cfg)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaks credential: %v", err)
			}
		})
	}
}

func TestEdgeEnvRejectsMalformedOverrides(t *testing.T) {
	t.Setenv("AGS_EDGE_ID", "edge-1")
	t.Setenv("AGS_EDGE_PRIMARY_URL", "https://primary.test")
	t.Setenv("AGS_EDGE_ALLOW_INSECURE_PRIMARY_HTTP", "sometimes")
	if _, err := NewEdge(); err == nil {
		t.Fatal("accepted invalid boolean")
	}
	t.Setenv("AGS_EDGE_ALLOW_INSECURE_PRIMARY_HTTP", "false")
	t.Setenv("AGS_EDGE_REQUEST_TIMEOUT", "0s")
	if _, err := NewEdge(); err == nil {
		t.Fatal("accepted zero explicit timeout")
	}
}
