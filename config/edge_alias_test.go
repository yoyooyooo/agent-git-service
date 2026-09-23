package config

import (
	"strings"
	"testing"
)

func TestEdgeAliasesAreExplicitOriginsForOneAuthority(t *testing.T) {
	base := EdgeConfig{ID: "edge-1", PrimaryURL: "http://127.0.0.1:6666", CanonicalURL: "http://primary.example.test:6666"}
	cfg := base
	cfg.CanonicalAliases = []string{"http://primary-alias.example.test:6666/"}
	normalized, err := NormalizeEdge(cfg)
	if err != nil || len(normalized.CanonicalAliases) != 1 || normalized.CanonicalAliases[0] != "http://primary-alias.example.test:6666" {
		t.Fatalf("explicit alias rejected: %v", err)
	}
	if normalized.PrimaryURL != base.PrimaryURL || normalized.CanonicalURL != base.CanonicalURL {
		t.Fatal("alias changed primary authority or dial target")
	}
	for _, aliases := range [][]string{
		{"http://primary.example.test:6666"},
		{"http://alias:6666", "http://ALIAS:6666"},
		{"https://alias:6666"}, {"http://alias:6667"},
		{"http://*.example:6666"}, {"http://user:password@alias:6666"},
		{"http://alias:6666/repo"}, {"http://alias:6666?x=1"},
		{"http://alias:6666#fragment"}, {""}, {" http://alias:6666"},
		strings.Split(strings.Repeat("http://alias:6666,", 17), ","),
	} {
		cfg := base
		cfg.CanonicalAliases = aliases
		if _, err := NormalizeEdge(cfg); err == nil {
			t.Fatal("invalid or ambiguous alias configuration admitted")
		}
	}
}

func TestNewEdgeReadsExplicitCanonicalAliases(t *testing.T) {
	t.Setenv("AGS_EDGE_ID", "edge-1")
	t.Setenv("AGS_EDGE_PRIMARY_URL", "http://127.0.0.1:6666")
	t.Setenv("AGS_EDGE_CANONICAL_URL", "http://primary.example.test:6666")
	t.Setenv("AGS_EDGE_CANONICAL_ALIASES", "http://primary-alias.example.test:6666")
	cfg, err := NewEdge()
	if err != nil || len(cfg.CanonicalAliases) != 1 {
		t.Fatalf("alias env not loaded: %v", err)
	}
}
