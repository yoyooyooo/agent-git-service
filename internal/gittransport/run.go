// Package gittransport owns credential lifetime at native provider-Git process
// boundaries. It executes once: a failed write must be reconciled by its caller.
package gittransport

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const outputLimit = 4 << 20

var ErrOutputLimit = errors.New("provider Git output exceeded limit")

// Run replaces the one exact remote argument with a credential-free URL. HTTP
// credentials supplied explicitly or through a legacy in-memory URL are placed
// in a private, request-owned Git include file, never argv, environment or the
// repository config. No raw stderr is returned; known Git failure classes are
// preserved without echoing a hostile remote's response. The caller owns the
// operation/ref/lease policy and its context deadline.
//
// The temporary directory is removed after the process is reaped, including
// cancellation and start failure. SIGKILL/host failure cannot run cleanup; use
// a private ephemeral runtime filesystem and its normal orphan-file policy.
// Other processes with the same OS identity or root remain trusted.
func Run(ctx context.Context, remoteURL, token string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clean, username, password, err := credentialParts(remoteURL, token)
	if err != nil {
		return nil, err
	}
	commandArgs := append([]string(nil), args...)
	matches := 0
	for i, arg := range commandArgs {
		if arg == remoteURL {
			commandArgs[i] = clean
			matches++
		}
	}
	if matches != 1 {
		return nil, errors.New("provider Git requires one exact remote argument")
	}
	if password != "" {
		for _, arg := range commandArgs {
			if strings.Contains(arg, password) {
				return nil, errors.New("provider Git argument contains credential material")
			}
		}
		root, err := os.MkdirTemp("", "ags-provider-auth-")
		if err != nil {
			return nil, errors.New("provider Git private authentication staging unavailable")
		}
		defer os.RemoveAll(root)
		if err := os.Chmod(root, 0o700); err != nil {
			return nil, errors.New("provider Git private authentication permissions failed")
		}
		configPath := filepath.Join(root, "http-auth.config")
		header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
		// URL-scoped headers are cleared before inserting this request's exact
		// credential. Redirects are forbidden, including the initial discovery.
		// The include path, not its contents, is propagated to Git children.
		config := "[http]\n\textraHeader =\n\tfollowRedirects = false\n" +
			"[http " + strconv.Quote(clean) + "]\n\textraHeader =\n\textraHeader = " + strconv.Quote(header) + "\n\tfollowRedirects = false\n" +
			"[credential]\n\thelper =\n" +
			"[credential " + strconv.Quote(clean) + "]\n\thelper =\n"
		if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
			return nil, errors.New("provider Git private authentication staging failed")
		}
		// Put protected runtime configuration after caller tuning but before the
		// verb. Git parses global -c options only before the subcommand.
		verb := -1
		for i, arg := range commandArgs {
			if arg == "push" || arg == "fetch" || arg == "ls-remote" {
				verb = i
				break
			}
		}
		if verb < 0 {
			return nil, errors.New("unsupported authenticated provider Git command")
		}
		protected := []string{"-c", "include.path=" + configPath, "-c", "credential.interactive=false"}
		commandArgs = append(append(append([]string(nil), commandArgs[:verb]...), protected...), commandArgs[verb:]...)
	}
	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	cmd.Env = processEnvironment()
	cmd.WaitDelay = 3 * time.Second
	stdout, stderr := &limitedOutput{}, &limitedOutput{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Run()
	if cause := ctx.Err(); cause != nil {
		return nil, cause
	}
	if err != nil {
		// ExitError.Error contains the exit status, not stderr or argv. Avoid
		// wrapping PathError/start errors, which may contain operator paths.
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("provider Git %s: %w", failureClass(string(stderr.data)), exit)
		}
		return nil, errors.New("provider Git process failed to start or drain")
	}
	if stdout.overflow {
		return nil, ErrOutputLimit
	}
	return stdout.data, nil
}

func credentialParts(raw, token string) (clean, username, password string, err error) {
	invalid := errors.New("invalid provider Git remote or credential")
	if raw == "" || strings.HasPrefix(raw, "-") || strings.ContainsAny(raw, "\x00\r\n") || strings.ContainsAny(token, "\x00\r\n") {
		return "", "", "", invalid
	}
	if !strings.Contains(raw, "://") {
		if strings.Contains(raw, "::") {
			return "", "", "", invalid
		}
		// Native local paths and SSH scp syntax use operator-owned SSH identity,
		// not the HTTP token. No token is copied into their process environment.
		return raw, "", "", nil
	}
	u, parseErr := url.Parse(raw)
	if parseErr != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(u.Path, "\x00\r\n") {
		return "", "", "", invalid
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		if u.Scheme != "ssh" && u.Scheme != "file" {
			return "", "", "", invalid
		}
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword || u.Scheme == "file" {
				return "", "", "", invalid
			}
		}
		return raw, "", "", nil
	}
	if u.Hostname() == "" || u.Opaque != "" {
		return "", "", "", invalid
	}
	username = "x-access-token"
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
		if username == "" || strings.ContainsAny(username, ":\x00\r\n") || strings.ContainsAny(password, "\x00\r\n") {
			return "", "", "", invalid
		}
	}
	if token != "" {
		if password != "" && password != token {
			return "", "", "", errors.New("provider Git credential sources disagree")
		}
		password = token
	}
	u.User = nil
	return u.String(), username, password, nil
}

// Use an allowlist, not a denylist of today's token names. In particular, Git
// trace/config overrides and the primary's unrelated provider secrets cannot
// reach subprocesses. SSH_AUTH_SOCK/HOME retain ordinary operator SSH routing.
func processEnvironment() []string {
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
		"TMPDIR": true, "TMP": true, "TEMP": true, "LANG": true,
		"LC_ALL": true, "LC_CTYPE": true, "SSH_AUTH_SOCK": true,
		"SYSTEMROOT": true, "WINDIR": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "CURL_CA_BUNDLE": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
		"http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true,
	}
	var result []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if allowed[strings.ToUpper(key)] || allowed[key] {
			result = append(result, entry)
		}
	}
	return append(result, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
}

type limitedOutput struct {
	data     []byte
	overflow bool
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := outputLimit - len(w.data)
	if n > remaining {
		w.overflow = true
		p = p[:remaining]
	}
	w.data = append(w.data, p...)
	return n, nil
}

func failureClass(stderr string) string {
	text := strings.ToLower(stderr)
	for _, known := range []string{"stale info", "non-fast-forward", "fetch first", "authentication failed", "repository not found", "could not resolve host", "connection refused", "certificate problem", "redirect"} {
		if strings.Contains(text, known) {
			return known
		}
	}
	return "operation failed (response withheld)"
}
