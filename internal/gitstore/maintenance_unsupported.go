//go:build !linux && !darwin

package gitstore

import (
	"errors"
	"os/exec"
)

func configureMaintenanceCommand(cmd *exec.Cmd)              {}
func finishMaintenanceCommand(cmd *exec.Cmd, cancelled bool) {}
func maintenanceSpaceAvailable(dir string, stats StorageStats) error {
	return errors.New("automatic repository maintenance requires a supported platform")
}
func lockRepositoryMaintenance(dir string) (func(), error) {
	return nil, errors.New("automatic repository maintenance requires a supported process/lock platform")
}
