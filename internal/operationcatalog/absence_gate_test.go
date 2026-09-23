package operationcatalog

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoDuplicateRuntimeLowRiskTable is an absence gate for AGS-T022 Stage 5:
// Access Grant evaluation must not keep a second operation-risk truth table.
func TestNoDuplicateRuntimeLowRiskTable(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	serviceDir := filepath.Join(root, "internal", "service")
	entries, err := os.ReadDir(serviceDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(serviceDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "runtimeLowRiskAdditionalOperations") {
			t.Fatalf("%s still defines or references runtimeLowRiskAdditionalOperations", entry.Name())
		}
	}
}

// TestCatalogIsRiskAuthority checks every known catalog op has an explicit risk.
func TestCatalogIsRiskAuthority(t *testing.T) {
	t.Parallel()
	for _, op := range AllOperations() {
		risk, ok := RiskOf(op)
		if !ok || (risk != RiskStandard && risk != RiskPrivileged) {
			t.Fatalf("operation %q missing explicit risk", op)
		}
	}
	if IsStandard("pr.merge") || !IsPrivileged("pr.merge") {
		t.Fatal("pr.merge must be privileged-only")
	}
	if !IsStandard("pr.comment") || IsPrivileged("pr.comment") {
		t.Fatal("pr.comment must be standard-only")
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/operationcatalog -> repo root
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
