//go:build linux

package routebstage0

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// Keep ownership of the exact os/exec child until Wait observes its exit.
// Identity failure cannot justify SIGKILL, a retry, or a normal-exit receipt.
// Catch stop signals so they cannot end this supervisor while the child lives.
func stopSpawnedChild(command *exec.Cmd) error {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	_ = command.Process.Signal(syscall.SIGTERM)
	for {
		select {
		case err := <-done:
			return err
		case <-signals:
			// TERM was already requested. Continue observing without escalation.
		}
	}
}
