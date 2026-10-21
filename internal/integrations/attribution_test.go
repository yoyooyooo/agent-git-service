package integrations

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAttributionConfigUsesOneValidatedTable(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		valid      bool
	}{
		{"shared", "attribution:\n  instance: primary-a\n  principals:\n    alice: {forgejo: alice.remote, github: alice-gh}\n", true},
		{"duplicate", "attribution:\n  principals:\n    alice: {forgejo: same}\n    bob: {forgejo: SAME}\n", false},
		{"unknown", "attribution:\n  principlas: {}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "integrations.yaml")
			if err := os.WriteFile(file, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadFile(file)
			if (err == nil) != tc.valid {
				t.Fatal("validation mismatch", err)
			}
			if tc.valid && (cfg.Attribution.Principals["alice"].Forgejo != "alice.remote" || cfg.Attribution.Principals["alice"].GitHub != "alice-gh") {
				t.Fatal("target columns lost")
			}
		})
	}
}
