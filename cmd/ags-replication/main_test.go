package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorBinaryIsAClientAndPrintsHelp(t *testing.T) {
	deps, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("dependencies: %v %s", err, deps)
	}
	for _, dependency := range strings.Fields(string(deps)) {
		if strings.HasPrefix(dependency, "gorm.io/") || dependency == "github.com/ngaut/agent-git-service/server" || dependency == "github.com/ngaut/agent-git-service/internal/service" || dependency == "github.com/ngaut/agent-git-service/internal/gitstore" || dependency == "github.com/ngaut/agent-git-service/internal/edge" {
			t.Errorf("operator command imported runtime: %s", dependency)
		}
	}
	binary := filepath.Join(t.TempDir(), "ags-replication")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	cmd := exec.Command(binary, "help")
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "peer-plan") || !strings.Contains(string(out), "no automatic rollout") {
		t.Fatalf("binary help: %v %s", err, out)
	}
}
