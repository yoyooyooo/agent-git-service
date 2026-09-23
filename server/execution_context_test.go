package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ngaut/agent-git-service/config"
)

func TestInitExecutionContextRegistryFromIntegrationsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	if err := os.WriteFile(path, []byte(`
execution_context:
  enabled: true
  connectors:
    - source_instance_id: multica-mini
      adapter: multica_current_execution_context_v1
      accepted_runtime_endpoints: [http://primary.example.test:37134]
      egress_endpoint: http://multica-backend:8080/api/integrations/current-execution-context
      workspace_mappings:
        11111111-1111-4111-8111-111111111111: mini
`), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := initExecutionContextRegistry(config.Config{IntegrationsConfigFile: path})
	if err != nil {
		t.Fatal(err)
	}
	if registry == nil || !registry.Enabled() {
		t.Fatal("execution context registry was not enabled")
	}
}

func TestInitExecutionContextRegistryRejectsDynamicEgressShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	if err := os.WriteFile(path, []byte(`
execution_context:
  enabled: true
  connectors:
    - source_instance_id: multica-mini
      adapter: multica_current_execution_context_v1
      accepted_runtime_endpoints: [http://primary.example.test:37134]
      egress_endpoint: http://multica-backend:8080/api/integrations/current-execution-context?target=caller
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := initExecutionContextRegistry(config.Config{IntegrationsConfigFile: path}); err == nil {
		t.Fatal("dynamic egress query was accepted")
	}
}
