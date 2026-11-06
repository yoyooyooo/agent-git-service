//go:build linux

package routebstage0

import (
	"os/exec"
	"syscall"
	"time"
)

// Only the os/exec handle of the child just created is used. This failure
// path never turns a forced termination into a normal-exit receipt.
func stopSpawnedChild(command *exec.Cmd, grace time.Duration) bool {
	done := make(chan struct{}, 1)
	go func() { command.Wait(); done <- struct{}{} }()
	command.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
		return false
	case <-time.After(grace):
	}
	command.Process.Kill()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	return true
}
