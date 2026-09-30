package artifactretirement

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// The migration is a local storage operation. Child processes inherit neither
// provider credentials nor user Git overrides, cannot run repository hooks, and
// cannot outlive the migration's cancellation into ordinary service startup.
func runGit(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base := []string{"--no-pager", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "gc.autoDetach=false", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false"}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = sanitizedGitEnv()
	limit := 16 * 1024 * 1024
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "cat-file" && args[i+1] == "blob" {
			limit = 256 * 1024 * 1024
		}
	}
	stdout := &retirementOutput{limit: limit}
	cmd.Stdout = stdout
	cmd.Stderr = io.Discard
	configureRetirementCommand(cmd)
	err := cmd.Run()
	finishRetirementCommand(cmd, ctx.Err() != nil)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("git %s failed: exit %d", firstArg(args), exit.ExitCode())
		}
		return nil, errors.New("retirement Git process could not start or drain")
	}
	return stdout.buffer.Bytes(), nil
}

type retirementOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (w *retirementOutput) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.buffer.Len() {
		return 0, errors.New("retirement Git output exceeds budget")
	}
	return w.buffer.Write(p)
}
