package edge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/buildinfo"
)

func TestDiagnosticsUseReleaseIdentityWithoutVCSMetadata(t *testing.T) {
	// Release archives are built with -buildvcs=false. Their linker identity,
	// not an optional vcs.revision setting, must survive in operator diagnostics.
	previousVersion, previousRevision, previousTree := buildinfo.Version, buildinfo.Revision, buildinfo.Tree
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Revision, buildinfo.Tree = previousVersion, previousRevision, previousTree
	})
	buildinfo.Version = "fork-20260924.1-rc3"
	buildinfo.Revision = strings.Repeat("a", 40)
	buildinfo.Tree = strings.Repeat("b", 40)
	t.Setenv("AGS_EDGE_SOURCE_REVISION", "untrusted-environment-value")
	server, err := New(config.EdgeConfig{ID: "release-fixture", PrimaryURL: "http://127.0.0.1:1", CanonicalURL: "http://primary.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/status", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	response := httptest.NewRecorder()
	server.DiagnosticsHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("diagnostics status %d", response.Code)
	}
	var result struct {
		SourceRevision string         `json:"source_revision"`
		Build          buildinfo.Info `json:"build"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SourceRevision != buildinfo.Revision {
		t.Fatalf("diagnostics lost release source identity: got %q", result.SourceRevision)
	}
	if result.Build != buildinfo.Current("ags-edge") {
		t.Fatalf("diagnostics and --version disagree: %#v", result.Build)
	}
	if strings.Contains(response.Body.String(), "untrusted-environment-value") {
		t.Fatal("environment overrode the compiled release identity")
	}
}
