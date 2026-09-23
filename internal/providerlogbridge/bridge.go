package providerlogbridge

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/providerlogprotocol"
	"gorm.io/gorm"
)

const (
	Schema      = "ags.internal-provider-log.v1"
	ErrorSchema = "ags.internal-provider-log-error.v1"
)

type Config struct {
	DB            *gorm.DB
	ActionsLogDir string
	ServiceToken  string
	MaxBytes      int64
	AllowedCIDRs  []string
}

type socketPeerContextKey struct{}

// CaptureSocketPeer records the transport peer before proxy-aware middleware
// can rewrite Request.RemoteAddr. The private key prevents callers from
// manufacturing this trust fact directly.
func CaptureSocketPeer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), socketPeerContextKey{}, r.RemoteAddr)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type Handler struct {
	db          *gorm.DB
	root        string
	tokenSum    [sha256.Size]byte
	maxBytes    int64
	allowed     []*net.IPNet
	zstdCommand func(context.Context, string) *exec.Cmd
}

type logBinding struct {
	RunNumber     int64
	JobName       string
	CommitSHA     string
	JobCommitSHA  string
	TaskCommitSHA string
	ProviderRef   string
	Event         string
	LogFilename   string
	InStorage     bool
	Expired       bool
}

type expectedBinding struct {
	ProviderPR int64
	HeadRef    string
	HeadSHA    string
}

type verifiedBinding struct {
	ProviderPR  int64
	HeadRef     string
	ProviderRef string
	Event       string
}

type response struct {
	Schema      string `json:"schema"`
	Repo        string `json:"repo"`
	TaskID      int64  `json:"task_id"`
	RunNumber   int64  `json:"run_number"`
	JobName     string `json:"job_name"`
	HeadSHA     string `json:"head_sha"`
	ProviderPR  int64  `json:"provider_pr,omitempty"`
	HeadRef     string `json:"head_ref,omitempty"`
	ProviderRef string `json:"provider_ref"`
	Event       string `json:"event"`
	Text        string `json:"text"`
}

func New(cfg Config) (*Handler, error) {
	if cfg.DB == nil || strings.TrimSpace(cfg.ActionsLogDir) == "" || strings.TrimSpace(cfg.ServiceToken) == "" {
		return nil, errors.New("provider log bridge requires database, actions log directory, and service token")
	}
	root, err := filepath.Abs(strings.TrimSpace(cfg.ActionsLogDir))
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve actions log root: %w", err)
	}
	allowed := make([]*net.IPNet, 0, len(cfg.AllowedCIDRs))
	for _, raw := range cfg.AllowedCIDRs {
		_, network, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("invalid provider log bridge CIDR")
		}
		allowed = append(allowed, network)
	}
	if len(allowed) == 0 {
		return nil, errors.New("provider log bridge requires an allowed source CIDR")
	}
	maxBytes := cfg.MaxBytes
	if maxBytes > providerlogprotocol.DecodedTextMaxBytes {
		return nil, fmt.Errorf("provider log bridge max bytes exceeds decoded protocol maximum of %d", providerlogprotocol.DecodedTextMaxBytes)
	}
	if maxBytes <= 0 {
		maxBytes = providerlogprotocol.DecodedTextMaxBytes
	}
	return &Handler{
		db:       cfg.DB,
		root:     root,
		tokenSum: sha256.Sum256([]byte(strings.TrimSpace(cfg.ServiceToken))),
		maxBytes: maxBytes,
		allowed:  allowed,
		zstdCommand: func(ctx context.Context, path string) *exec.Cmd {
			return exec.CommandContext(ctx, "zstd", "-dc", "--", path)
		},
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.fail(w, http.StatusNotFound, "not_found", "Provider log was not found")
		return
	}
	socketPeer, captured := r.Context().Value(socketPeerContextKey{}).(string)
	if !captured || !h.sourceAllowed(socketPeer) || !h.authorized(r.Header.Get("Authorization")) {
		h.fail(w, http.StatusUnauthorized, "unauthorized", "Provider log bridge authentication failed")
		return
	}
	owner, repo := chi.URLParam(r, "owner"), chi.URLParam(r, "repo")
	taskID, err := strconv.ParseInt(chi.URLParam(r, "task"), 10, 64)
	expected, expectedErr := parseExpectedBinding(r)
	if err != nil || taskID <= 0 || !safePart(owner) || !safePart(repo) || expectedErr != nil {
		h.fail(w, http.StatusNotFound, "not_found", "Provider log was not found")
		return
	}
	binding, status, code, err := h.binding(r.Context(), owner, repo, taskID)
	if err != nil {
		h.fail(w, status, code, providerMessage(status))
		return
	}
	verified, err := verifyBinding(binding, expected)
	if err != nil {
		h.fail(w, http.StatusConflict, "binding_mismatch", providerMessage(http.StatusConflict))
		return
	}
	text, status, code, err := h.read(r.Context(), binding.LogFilename)
	if err != nil {
		h.fail(w, status, code, providerMessage(status))
		return
	}
	h.write(w, http.StatusOK, response{
		Schema: Schema, Repo: owner + "/" + repo, TaskID: taskID, RunNumber: binding.RunNumber,
		JobName: binding.JobName, HeadSHA: binding.CommitSHA, ProviderPR: verified.ProviderPR,
		HeadRef: verified.HeadRef, ProviderRef: verified.ProviderRef, Event: verified.Event, Text: string(text),
	})
}

func (h *Handler) binding(ctx context.Context, owner, repo string, taskID int64) (logBinding, int, string, error) {
	var row logBinding
	err := h.db.WithContext(ctx).Raw(`
SELECT ar."index", arj.name, ar.commit_sha, arj.commit_sha, at.commit_sha, ar.ref, ar.event, at.log_filename, at.log_in_storage, at.log_expired
FROM action_task at
JOIN action_run_job arj ON arj.task_id = at.id AND arj.repo_id = at.repo_id
JOIN action_run ar ON ar.id = arj.run_id AND ar.repo_id = at.repo_id
JOIN repository r ON r.id = at.repo_id
WHERE lower(r.owner_name) = lower(?) AND lower(r.lower_name) = lower(?) AND at.id = ?
`, owner, repo, taskID).Row().Scan(
		&row.RunNumber, &row.JobName, &row.CommitSHA, &row.JobCommitSHA, &row.TaskCommitSHA,
		&row.ProviderRef, &row.Event, &row.LogFilename, &row.InStorage, &row.Expired,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return row, http.StatusNotFound, "not_found", err
	}
	if err != nil {
		return row, http.StatusBadGateway, "database_unavailable", err
	}
	if !row.InStorage || row.Expired || strings.TrimSpace(row.LogFilename) == "" {
		return row, http.StatusConflict, "log_unavailable", errors.New("provider log unavailable")
	}
	return row, 0, "", nil
}

func parseExpectedBinding(r *http.Request) (expectedBinding, error) {
	query := r.URL.Query()
	if len(query["provider_pr"]) != 1 || len(query["head_ref"]) != 1 || len(query["head_sha"]) != 1 {
		return expectedBinding{}, errors.New("provider log binding fields must be singular")
	}
	providerPRRaw := query.Get("provider_pr")
	providerPR, err := strconv.ParseInt(providerPRRaw, 10, 64)
	headRef := query.Get("head_ref")
	headSHA := query.Get("head_sha")
	if err != nil || providerPR <= 0 || strconv.FormatInt(providerPR, 10) != providerPRRaw || !safeHeadRef(headRef) || !canonicalSHA(headSHA) {
		return expectedBinding{}, errors.New("invalid provider log binding request")
	}
	return expectedBinding{ProviderPR: providerPR, HeadRef: headRef, HeadSHA: headSHA}, nil
}

func verifyBinding(binding logBinding, expected expectedBinding) (verifiedBinding, error) {
	if binding.CommitSHA != expected.HeadSHA || binding.JobCommitSHA != expected.HeadSHA || binding.TaskCommitSHA != expected.HeadSHA {
		return verifiedBinding{}, errors.New("provider log commit binding mismatch")
	}
	providerRef := strings.TrimSpace(binding.ProviderRef)
	event := strings.TrimSpace(binding.Event)
	pullRef := "refs/pull/" + strconv.FormatInt(expected.ProviderPR, 10) + "/head"
	if providerRef == pullRef && event == "pull_request" {
		return verifiedBinding{ProviderPR: expected.ProviderPR, ProviderRef: providerRef, Event: event}, nil
	}
	branchRef := "refs/heads/" + expected.HeadRef
	if providerRef == branchRef && event == "workflow_dispatch" {
		return verifiedBinding{HeadRef: expected.HeadRef, ProviderRef: providerRef, Event: event}, nil
	}
	return verifiedBinding{}, errors.New("provider log ref binding mismatch")
}

func safeHeadRef(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.HasPrefix(value, "refs/") && !strings.HasPrefix(value, "#") && !strings.ContainsAny(value, "\x00\r\n")
}

func canonicalSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func (h *Handler) read(ctx context.Context, relative string) ([]byte, int, string, error) {
	if filepath.IsAbs(relative) || filepath.Clean(relative) != relative {
		return nil, http.StatusConflict, "unsafe_path", errors.New("unsafe provider log path")
	}
	candidate := filepath.Join(h.root, relative)
	rel, err := filepath.Rel(h.root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, http.StatusConflict, "unsafe_path", errors.New("unsafe provider log path")
	}
	current := h.root
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, http.StatusNotFound, "not_found", err
			}
			return nil, http.StatusBadGateway, "read_failed", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, http.StatusConflict, "unsafe_path", errors.New("symlink provider log path")
		}
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.Mode().IsRegular() {
		if os.IsNotExist(err) {
			return nil, http.StatusNotFound, "not_found", err
		}
		return nil, http.StatusConflict, "log_unavailable", errors.New("provider log is not regular")
	}
	if info.Size() > h.maxBytes {
		return nil, http.StatusRequestEntityTooLarge, "log_too_large", errors.New("provider log exceeds limit")
	}
	if strings.HasSuffix(candidate, ".zst") {
		decodeCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		cmd := h.zstdCommand(decodeCtx, candidate)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, http.StatusBadGateway, "decode_failed", err
		}
		if err := cmd.Start(); err != nil {
			return nil, http.StatusBadGateway, "decode_failed", err
		}
		data, readErr := io.ReadAll(io.LimitReader(stdout, h.maxBytes+1))
		if int64(len(data)) > h.maxBytes {
			// The decoder may still be blocked writing to stdout after LimitReader
			// stops. Terminate it and close the pipe before Wait so oversize input
			// cannot deadlock the request while keeping memory bounded.
			cancel()
			_ = stdout.Close()
			_ = cmd.Wait()
			return nil, http.StatusRequestEntityTooLarge, "log_too_large", errors.New("provider log exceeds limit")
		}
		waitErr := cmd.Wait()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, http.StatusBadGateway, "decode_failed", errors.Join(ctxErr, readErr, waitErr)
		}
		if readErr != nil || waitErr != nil {
			return nil, http.StatusBadGateway, "decode_failed", errors.Join(readErr, waitErr)
		}
		return data, 0, "", nil
	}
	file, err := os.Open(candidate)
	if err != nil {
		return nil, http.StatusBadGateway, "read_failed", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, h.maxBytes+1))
	if err != nil {
		return nil, http.StatusBadGateway, "read_failed", err
	}
	if int64(len(data)) > h.maxBytes {
		return nil, http.StatusRequestEntityTooLarge, "log_too_large", errors.New("provider log exceeds limit")
	}
	return data, 0, "", nil
}

func (h *Handler) authorized(value string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(strings.TrimPrefix(value, prefix))))
	return subtle.ConstantTimeCompare(got[:], h.tokenSum[:]) == 1
}

func (h *Handler) sourceAllowed(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return false
	}
	for _, network := range h.allowed {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func safePart(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00\r\n")
}
func providerMessage(status int) string {
	switch status {
	case http.StatusNotFound:
		return "Provider log was not found"
	case http.StatusConflict:
		return "Provider log is unavailable"
	case http.StatusRequestEntityTooLarge:
		return "Provider log exceeds the configured limit"
	default:
		return "Provider log bridge is unavailable"
	}
}
func (h *Handler) fail(w http.ResponseWriter, status int, code, message string) {
	h.write(w, status, map[string]string{"schema": ErrorSchema, "code": code, "message": message})
}
func (h *Handler) write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
