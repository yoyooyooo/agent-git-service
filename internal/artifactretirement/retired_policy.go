package artifactretirement

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// InstallRetiredBlobPolicy publishes only exact object IDs, never executable
// policy. The managed receive hook reads it to reject old-client resurrection.
func InstallRetiredBlobPolicy(gitDir string, blobs []BlobSpec) error {
	ids := make([]string, 0, len(blobs))
	seen := map[string]bool{}
	for _, blob := range blobs {
		if !fullOID(blob.OID) || seen[blob.OID] {
			return errors.New("invalid retired artifact policy")
		}
		seen[blob.OID] = true
		ids = append(ids, blob.OID)
	}
	if len(ids) == 0 || len(ids) > 32 {
		return errors.New("retired artifact policy size invalid")
	}
	sort.Strings(ids)
	content := []byte(strings.Join(ids, "\n") + "\n")
	path := filepath.Join(gitDir, "ags-retired-blobs")
	if _, err := os.Lstat(path); err == nil {
		if _, err := validatePrivateStateFile(path, 4096); err != nil {
			return err
		}
		prior, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(prior, content) {
			return errors.New("existing retired artifact policy differs from completed operation")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := rejectSymlinkComponents(gitDir); err != nil {
		return err
	}
	file, err := os.CreateTemp(gitDir, ".ags-retired-blobs-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncStateDirectory(gitDir)
}
