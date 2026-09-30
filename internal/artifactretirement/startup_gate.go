package artifactretirement

import (
	"errors"
	"os"
	"path/filepath"
)

type StartupMarker struct {
	Schema       string `json:"schema"`
	IntentPath   string `json:"intent_path"`
	IntentSHA256 string `json:"intent_sha256"`
	RepositoryID uint   `json:"repository_id"`
	Repository   string `json:"repository"`
	OperationID  string `json:"operation_id"`
}

type StartupGate struct {
	directory string
	marker    StartupMarker
	release   func()
}

func startupGateDirectory(root string) string { return filepath.Join(root, ".ags-artifact-retirement") }

// CheckStartupGate runs even with no intent configured. A partially published
// migration cannot be made invisible simply by unsetting an environment entry.
func CheckStartupGate(root, intentPath string) error {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	path := filepath.Join(startupGateDirectory(absoluteRoot), "pending.json")
	var marker StartupMarker
	if err := readPrivateJSON(path, &marker); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if marker.Schema != "ags.artifact-retirement.pending.v1" || !fullDigest(marker.IntentSHA256) || marker.RepositoryID == 0 {
		return errors.New("invalid pending retirement marker")
	}
	if intentPath == "" || intentPath != marker.IntentPath {
		return errors.New("an incomplete artifact retirement requires its original intent")
	}
	intent, err := LoadIntent(intentPath)
	if err != nil {
		return err
	}
	digest, err := IntentSHA256(intent)
	if err != nil {
		return err
	}
	if digest != marker.IntentSHA256 {
		return errors.New("pending artifact retirement intent changed")
	}
	return nil
}

func AcquireStartupGate(root, intentPath string, repositoryID uint, intent Intent) (*StartupGate, error) {
	if repositoryID == 0 {
		return nil, errors.New("retirement repository identity is required")
	}
	digest, err := IntentSHA256(intent)
	if err != nil {
		return nil, err
	}
	directory := startupGateDirectory(root)
	release, err := LockState(directory)
	if err != nil {
		return nil, err
	}
	gate := &StartupGate{directory: directory, release: release, marker: StartupMarker{
		Schema: "ags.artifact-retirement.pending.v1", IntentPath: intentPath, IntentSHA256: digest,
		RepositoryID: repositoryID, Repository: intent.Repository, OperationID: intent.OperationID,
	}}
	var current StartupMarker
	if err := readPrivateJSON(filepath.Join(directory, "pending.json"), &current); err == nil {
		if current != gate.marker {
			release()
			return nil, errors.New("another or changed retirement owns the pending journal")
		}
	} else if !os.IsNotExist(err) {
		release()
		return nil, err
	}
	return gate, nil
}

func (g *StartupGate) MarkPending() error {
	return writePrivateJSON(filepath.Join(g.directory, "pending.json"), g.marker)
}

// Complete is called only after the exact completed receipt is durable and all
// local/provider/database checks have succeeded. A mismatched journal remains.
func (g *StartupGate) Complete() error {
	path := filepath.Join(g.directory, "pending.json")
	var current StartupMarker
	if err := readPrivateJSON(path, &current); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if current != g.marker {
		return errors.New("retirement journal changed before completion")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncStateDirectory(g.directory)
}

func (g *StartupGate) Close() {
	if g.release != nil {
		g.release()
		g.release = nil
	}
}
