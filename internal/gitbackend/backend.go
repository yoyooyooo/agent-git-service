// Package gitbackend executes an already-authorized Git Smart HTTP request.
// It has no database, authentication, repository creation, or post-push hooks.
// Callers own authorization and must keep the selected repository view alive
// until Serve returns. The zero-value Request never permits receive-pack.
package gitbackend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	UploadPack  = "git-upload-pack"
	ReceivePack = "git-receive-pack"
	// DefaultMaxPushBytes is the historical AGS chunked request spool limit.
	DefaultMaxPushBytes int64 = 2 * 1024 * 1024 * 1024
)

// Request describes a trusted local repository, not a user-supplied filesystem
// path. Repository is owner/name (without .git); ProjectRoot contains it.
// AllowReceive must be explicitly set by a primary write handler. An Edge
// reader must never set it, even if its mirror was configured to accept pushes.
type Request struct {
	ProjectRoot  string
	Repository   string
	Service      string
	Advertise    bool
	AllowReceive bool
	// AllowDeleteCurrent is a request-scoped primary Wiki policy. It never
	// modifies repository config and is invalid for isolated/read operations.
	AllowDeleteCurrent bool
	// IsolatedRead prevents ambient system/user Git configuration from changing
	// a verified snapshot's serving policy. It is incompatible with writes.
	IsolatedRead           bool
	ProtectedRefs          []string
	Delegated              bool
	DelegatedProtectedRefs []string
}

func (req Request) validate(r *http.Request) error {
	if req.ProjectRoot == "" {
		return errors.New("git backend: missing project root")
	}
	parts := strings.Split(req.Repository, "/")
	if len(parts) != 2 {
		return errors.New("git backend: invalid repository name")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n") {
			return errors.New("git backend: invalid repository name")
		}
	}
	if req.Service != UploadPack && req.Service != ReceivePack {
		return errors.New("git backend: unsupported service")
	}
	if req.AllowDeleteCurrent && (req.Service != ReceivePack || !req.AllowReceive || req.IsolatedRead || !strings.HasSuffix(req.Repository, ".wiki")) {
		return errors.New("git backend: current-branch deletion is primary-write-only")
	}
	if req.IsolatedRead && (req.Service != UploadPack || req.AllowReceive) {
		return errors.New("git backend: isolated snapshots are read-only")
	}
	if req.Service == ReceivePack && !req.AllowReceive {
		return errors.New("git backend: receive-pack is disabled")
	}
	query, err := urlQuery(r)
	if err != nil {
		return err
	}
	if req.Advertise {
		if r.Method != http.MethodGet || len(query) != 1 || query[0] != req.Service {
			return errors.New("git backend: advertisement service mismatch")
		}
	} else if r.Method != http.MethodPost || len(query) != 0 {
		return errors.New("git backend: invalid RPC request")
	}
	return nil
}

// urlQuery validates before CGI sees the query; duplicate service parameters
// must not make the authorization and backend select different operations.
func urlQuery(r *http.Request) ([]string, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errors.New("git backend: invalid query")
	}
	return q["service"], nil
}

// Serve delegates to the system git-http-backend. A nil error means the HTTP
// response was handled, NOT that a push succeeded: Git reports ref outcomes in
// its protocol body. No authentication credential is forwarded to CGI.
func Serve(w http.ResponseWriter, r *http.Request, req Request) error {
	if err := req.validate(r); err != nil {
		return err
	}
	backend, err := findBackend()
	if err != nil {
		return fmt.Errorf("git-http-backend not found: %w", err)
	}
	action := req.Service
	if req.Advertise {
		action = "info/refs"
	}
	env := []string{
		"GIT_PROJECT_ROOT=" + req.ProjectRoot,
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO=/" + req.Repository + ".git/" + action,
		"REMOTE_USER=git",
	}
	if req.IsolatedRead {
		env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM="+os.DevNull,
			"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1")
	}
	if req.Service == ReceivePack {
		env = append(env,
			"AGS_GIT_HTTP_RECEIVE_PACK=1",
			"AGS_GIT_HTTP_PROTECTED_REFS="+strings.Join(req.ProtectedRefs, ":"),
		)
	}
	if req.AllowDeleteCurrent {
		env = append(env, "AGS_SYNTHETIC_WIKI_WRITE=1", "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=receive.denyDeleteCurrent", "GIT_CONFIG_VALUE_0=ignore")
	}
	if req.Delegated {
		env = append(env,
			"AGS_DELEGATED_SESSION=1",
			"AGS_DELEGATED_PROTECTED_REFS="+strings.Join(req.DelegatedProtectedRefs, ":"),
		)
	}
	cgiReq := r.Clone(r.Context())
	cgiReq.Header = r.Header.Clone()
	for _, header := range []string{"Authorization", "Proxy-Authorization", "Cookie"} {
		cgiReq.Header.Del(header)
	}
	if hasChunkedTransferEncoding(cgiReq.TransferEncoding) {
		tmp, size, exceeded, err := SpoolChunkedBody(r.Context(), w, cgiReq.Body, MaxPushBytes())
		if err != nil {
			return fmt.Errorf("spool chunked request body: %w", err)
		}
		if exceeded {
			return nil
		}
		defer func() {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}()
		cgiReq.Body = tmp
		cgiReq.ContentLength = size
		cgiReq.TransferEncoding = nil
		cgiReq.Header.Del("Transfer-Encoding")
	}
	dir, _ := os.Getwd()
	handler := &cgi.Handler{
		Path: backend, Dir: dir, Env: env,
		InheritEnv: []string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "LANG", "LC_ALL"},
	}
	handler.ServeHTTP(w, cgiReq)
	return nil
}

func hasChunkedTransferEncoding(encodings []string) bool {
	for _, encoding := range encodings {
		if strings.EqualFold(encoding, "chunked") {
			return true
		}
	}
	return false
}

// MaxPushBytes preserves GITHTTP_MAX_PUSH_BYTES for the primary's preflight
// Content-Length check and the common CGI chunked-body adapter.
func MaxPushBytes() int64 {
	if value := os.Getenv("GITHTTP_MAX_PUSH_BYTES"); value != "" {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return DefaultMaxPushBytes
}

// SpoolChunkedBody adapts an un-sized body to CGI without buffering it in RAM.
// On success the caller owns closing and deleting the returned file. On a
// limit breach the 413 response has already been written and tmp is nil.
func SpoolChunkedBody(ctx context.Context, w http.ResponseWriter, body io.ReadCloser, maxBytes int64) (*os.File, int64, bool, error) {
	if err := ctx.Err(); err != nil {
		_ = body.Close()
		return nil, 0, false, err
	}
	// Request bodies must support concurrent Close and Read. A timeout must
	// unblock a slow client instead of leaving a CGI spool and worker pinned.
	stopCancel := context.AfterFunc(ctx, func() { _ = body.Close() })
	defer stopCancel()
	tmp, err := os.CreateTemp(os.Getenv("GITHTTP_SPOOL_DIR"), "git-push-*.body")
	if err != nil {
		return nil, 0, false, fmt.Errorf("create temp: %w", err)
	}
	release := tmp
	defer func() {
		if release != nil {
			_ = release.Close()
			_ = os.Remove(release.Name())
		}
	}()
	n, err := io.Copy(tmp, io.LimitReader(body, maxBytes+1))
	_ = body.Close()
	if cancelErr := ctx.Err(); cancelErr != nil {
		return nil, 0, false, cancelErr
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("spool body: %w", err)
	}
	if n > maxBytes {
		slog.WarnContext(ctx, "githttp body exceeded limit", "size", n, "limit", maxBytes)
		http.Error(w, "push body exceeds maximum size", http.StatusRequestEntityTooLarge)
		return nil, n, true, nil
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, 0, false, fmt.Errorf("rewind temp: %w", err)
	}
	release = nil
	return tmp, n, false, nil
}

func findBackend() (string, error) {
	candidates := []string{
		"/usr/lib/git-core/git-http-backend",
		"/usr/libexec/git-core/git-http-backend",
		"/opt/homebrew/libexec/git-core/git-http-backend",
	}
	if out, err := exec.Command("git", "--exec-path").Output(); err == nil {
		candidates = append([]string{filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")}, candidates...)
	}
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", errors.New("not found in common locations")
}
