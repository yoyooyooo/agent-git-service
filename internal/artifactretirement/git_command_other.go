//go:build !linux && !darwin

package artifactretirement

import "os/exec"

func configureRetirementCommand(cmd *exec.Cmd)              {}
func finishRetirementCommand(cmd *exec.Cmd, cancelled bool) {}
