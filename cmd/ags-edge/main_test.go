package main

import (
	"os/exec"
	"strings"
	"testing"
)

// This is a dependency-graph gate, not a textual import grep: transitive
// imports must not silently restore the primary bootstrap on the Edge.
func TestEdgeBinaryDoesNotImportPrimaryRuntime(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, dependency := range strings.Fields(string(out)) {
		for _, forbidden := range []string{
			"github.com/ngaut/agent-git-service/server",
			"github.com/ngaut/agent-git-service/internal/service",
			"github.com/ngaut/agent-git-service/internal/db",
			"github.com/ngaut/agent-git-service/internal/controlplane",
			"github.com/ngaut/agent-git-service/internal/forgejointegration",
			"gorm.io/",
		} {
			if dependency == forbidden || strings.HasSuffix(forbidden, "/") && strings.HasPrefix(dependency, forbidden) {
				t.Errorf("Edge imported primary dependency %s", dependency)
			}
		}
	}
}
