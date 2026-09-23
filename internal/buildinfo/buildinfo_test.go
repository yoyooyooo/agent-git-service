package buildinfo

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestVersionIsClosedCredentialFreeAndIndependentFromEnvironment(t *testing.T) {
	t.Setenv("DB_DSN", "invalid-must-not-open")
	t.Setenv("AGS_EDGE_READ_CONFIG_FILE", "/missing/must-not-open")
	t.Setenv("GITHUB_TOKEN", "synthetic-must-not-print")
	for _, command := range []string{"gh-server", "ags-edge", "ags-replication"} {
		var out bytes.Buffer
		handled, err := PrintVersion([]string{"--version"}, command, &out)
		if !handled || err != nil {
			t.Fatalf("version: %v", err)
		}
		var result map[string]any
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result) != 8 || result["schema"] != "ags.build.v1" || result["command"] != command || result["revision"] != Revision {
			t.Fatalf("unexpected build identity: %v", result)
		}
		if bytes.Contains(out.Bytes(), []byte("synthetic-must-not-print")) {
			t.Fatal("environment leaked")
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failure") }
func TestVersionDoesNotConsumeOtherCommandsAndReportsOutputFailure(t *testing.T) {
	for _, args := range [][]string{nil, {"status"}, {"--version", "extra"}} {
		var out bytes.Buffer
		if handled, err := PrintVersion(args, "ags-edge", &out); handled || err != nil || out.Len() != 0 {
			t.Fatal("consumed another command")
		}
	}
	if handled, err := PrintVersion([]string{"version"}, "ags-edge", failingWriter{}); !handled || err == nil {
		t.Fatal("lost output failure")
	}
}
