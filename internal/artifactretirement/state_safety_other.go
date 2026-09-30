//go:build !linux && !darwin

package artifactretirement

import (
	"errors"
	"os"
)

func ownedStateFile(st os.FileInfo) bool { return false }
func lockStateFile(path string) (func(), error) {
	return nil, errors.New("artifact retirement requires a supported owner/lock platform")
}
