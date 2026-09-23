package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Snapshot construction intentionally ignores ambient Git configuration. That
// must not bypass the primary CGI's EFFECTIVE (system/global/local) read policy.
// Until hidden-ref evaluation is supported, reject it rather than export more.
// Use the primary backend's inherited environment, not arbitrary GIT_CONFIG_*
// overrides. Never return config contents, paths or credentials in an error.
func checkPrimaryExportPolicy(ctx context.Context, source string) error {
	env := []string{}
	for _, key := range []string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "LANG", "LC_ALL"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	run := func(args ...string) ([]byte, int) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", source, "config"}, args...)...)
		cmd.Env = env
		var output bytes.Buffer
		// Only bool output is read below; configured hidden/hook values are
		// discarded, not buffered or logged (they may contain private paths).
		if len(args) > 0 && args[0] == "--bool" {
			cmd.Stdout = &output
		} else {
			cmd.Stdout = io.Discard
		}
		cmd.Stderr = io.Discard
		err := cmd.Run()
		if err == nil {
			return output.Bytes(), 0
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, exit.ExitCode()
		}
		return nil, -1
	}
	for _, key := range []string{"uploadpack.hideRefs", "transfer.hideRefs", "uploadpack.packObjectsHook"} {
		_, code := run("--get-all", key)
		if code != 1 {
			return errors.New("effective primary Git policy is not exportable")
		}
	}
	value, code := run("--bool", "--get", "http.uploadpack")
	if code != 1 && (code != 0 || strings.TrimSpace(string(value)) != "true") {
		return errors.New("primary Git upload-pack is disabled or invalid")
	}
	return ctx.Err()
}
