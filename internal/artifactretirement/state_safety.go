package artifactretirement

import (
	"errors"
	"os"
	"path/filepath"
)

// EnsurePrivateStateDirectory never chmods an existing directory into apparent
// compliance. In particular a symlink in an operator state path is rejected
// before creating or deleting staging files beneath it.
func EnsurePrivateStateDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("retirement state path must be absolute and canonical")
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 || !ownedStateFile(st) {
		return errors.New("retirement state directory must be private and owned")
	}
	return nil
}

func rejectSymlinkComponents(path string) error {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		st, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && st.Mode()&os.ModeSymlink != 0 {
			return errors.New("retirement state path contains a symlink")
		}
		if parent := filepath.Dir(current); parent == current {
			break
		}
	}
	return nil
}

func validatePrivateStateFile(path string, maximum int64) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("retirement state file must be absolute and canonical")
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > maximum || !ownedStateFile(st) {
		return nil, errors.New("retirement state file must be bounded, private and owned")
	}
	return st, nil
}

// LockState serializes resumable work even when two startup processes are
// accidentally launched. It supplements, not replaces, the runtime owner lock.
func LockState(directory string) (func(), error) {
	if err := EnsurePrivateStateDirectory(directory); err != nil {
		return nil, err
	}
	return lockStateFile(filepath.Join(directory, "operation.lock"))
}
