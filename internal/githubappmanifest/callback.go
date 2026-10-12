// Package githubappmanifest exchanges a GitHub App manifest code and writes
// the three owner-only credential files. It does not use a personal token.
// Response secrets stay in the returned file bodies and are then wiped.
package githubappmanifest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// CallbackPath is the tailnet-only manifest redirect path.
	CallbackPath = "/api/ext/v1/integrations/github/app-manifest/callback"
	// ExchangeURL is the GitHub manifest conversion endpoint with a code placeholder.
	exchangeURLPath = "/app-manifests/%s/conversions"
	// Window is the official three-step registration limit.
	Window = time.Hour
	// stateFileMode is required for the state file and the written secrets.
	stateFileMode os.FileMode = 0o600
)

// Paths are the final credential files. They are replaced atomically.
type Paths struct {
	AppID         string
	PrivateKey    string
	WebhookSecret string
}

// Registration is the non-secret record retained after a successful exchange.
type Registration struct {
	AppID       string
	Slug        string
	OwnerLogin  string
	Permissions map[string]string
	Events      []string
	WebhookURL  string
	RedirectURL string
}

// State is the one-hour owner confirmation record.
type State struct {
	Value       string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RedirectURL string
}

// Exchanger performs the manifest conversion. Tests supply a fixture.
type Exchanger interface {
	Convert(code string) (Conversion, error)
}

// Conversion is the subset of the GitHub 201 body this flow uses.
type Conversion struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
	Permissions    map[string]string `json:"permissions"`
	Events         []string          `json:"events"`
	HookAttributes struct {
		URL string `json:"url"`
	} `json:"hook_attributes"`
}

// Flow is one manifest registration window.
type Flow struct {
	StatePath string
	Paths     Paths
	Exchanger Exchanger
	Now       func() time.Time
	OwnerUID  int
	Log       *slog.Logger
	// StageFailAfter is how many credential files may be staged before the next
	// one fails. Zero means no injected failure. Tests use it to prove a partial
	// set is not committed.
	StageFailAfter int
	// rename publishes one staged credential file. Tests set it to fail or stall
	// one commit; nil uses os.Rename. Restoring a failed commit does not use it.
	rename func(staged, dest string) error

	mu        sync.Mutex
	usedCodes map[string]struct{}
	// pendingCodes is the set of codes whose exchange or credential write is
	// still in progress. A second callback with one of those codes waits out
	// this process instead of starting another one.
	pendingCodes map[string]struct{}
	// retained keeps a conversion whose files were not committed, keyed by
	// code, so the one-time exchange can be written again without calling
	// GitHub a second time.
	retained map[string]Conversion
}

// Begin writes a new 0600 state file. An existing state is not reused.
func (f *Flow) Begin(redirectURL string) (State, error) {
	if strings.TrimSpace(f.StatePath) == "" {
		return State{}, fmt.Errorf("manifest state path is empty")
	}
	now := f.now()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return State{}, err
	}
	state := State{
		Value:       hex.EncodeToString(buf),
		CreatedAt:   now.UTC(),
		ExpiresAt:   now.Add(Window).UTC(),
		RedirectURL: strings.TrimSpace(redirectURL),
	}
	body, err := json.Marshal(state)
	if err != nil {
		return State{}, err
	}
	if err := writeNewFile(f.StatePath, body, f.ownerUID()); err != nil {
		return State{}, err
	}
	return state, nil
}

// Complete checks state and the one-hour window, exchanges the code once, and
// writes the three credential files. The code is not logged.
//
// The state is claimed before the exchange and again before every retry of a
// retained conversion, so two callbacks cannot both pass the same state. A
// failed exchange releases the claim. A successful exchange is kept in memory
// when the credential set cannot be committed, and the same code retries the
// write without a second conversion. A retry still has to present the current
// state and pass the one-hour check. Every previous credential is backed up
// before the new set is published, and a failed publication restores that
// backup, so the previous set stays byte-for-byte unchanged.
func (f *Flow) Complete(code, state string) (Registration, error) {
	code = strings.TrimSpace(code)
	state = strings.TrimSpace(state)
	if code == "" || state == "" {
		return Registration{}, fmt.Errorf("manifest code and state are required")
	}
	conversion, release, err := f.claim(code, state)
	if err != nil {
		return Registration{}, err
	}
	if release != nil {
		defer release()
	}
	registration, err := f.persist(conversion)
	f.wipe(&conversion)
	if err != nil {
		return Registration{}, err
	}
	if err := os.Remove(f.StatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Registration{}, fmt.Errorf("remove manifest state: %w", err)
	}
	f.finish(code)
	release = nil
	f.logCompleted(registration)
	return registration, nil
}

// claim reserves the state before any exchange and before every retry of a
// retained conversion. release is non-nil only when the caller must undo that
// reservation because nothing was committed. The lock is not held across
// Convert.
func (f *Flow) claim(code, state string) (Conversion, func(), error) {
	retained, err := f.reserve(code, state)
	if err != nil {
		return Conversion{}, nil, err
	}
	if retained != nil {
		return *retained, func() { f.release(code, state) }, nil
	}
	conversion, err := f.Exchanger.Convert(code)
	if err != nil {
		f.release(code, state)
		return Conversion{}, nil, err
	}
	f.remember(code, conversion)
	return conversion, func() { f.release(code, state) }, nil
}

// reserve returns a retained conversion for a retry, or nil when this call
// owns a fresh state reservation and must exchange. Both paths validate the
// current state file and hold its claim until the credential set is committed.
func (f *Flow) reserve(code, state string) (*Conversion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.usedCodes == nil {
		f.usedCodes = map[string]struct{}{}
	}
	if _, used := f.usedCodes[code]; used {
		return nil, fmt.Errorf("manifest code was already used")
	}
	if _, pending := f.pendingCodes[code]; pending {
		return nil, fmt.Errorf("manifest code is already in use")
	}
	loaded, err := f.loadState()
	if err != nil {
		return nil, err
	}
	if loaded.Value != state {
		return nil, fmt.Errorf("manifest state does not match")
	}
	if f.now().After(loaded.ExpiresAt) {
		return nil, fmt.Errorf("manifest registration window expired")
	}
	if _, claimed := f.usedCodes[state]; claimed {
		return nil, fmt.Errorf("manifest state is already in use")
	}
	f.usedCodes[state] = struct{}{}
	if f.pendingCodes == nil {
		f.pendingCodes = map[string]struct{}{}
	}
	f.pendingCodes[code] = struct{}{}
	if retained, ok := f.retained[code]; ok {
		copyOfRetained := retained
		return &copyOfRetained, nil
	}
	return nil, nil
}

func (f *Flow) remember(code string, conversion Conversion) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retained == nil {
		f.retained = map[string]Conversion{}
	}
	f.retained[code] = conversion
}

func (f *Flow) release(code, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.usedCodes, state)
	delete(f.pendingCodes, code)
}

func (f *Flow) finish(code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.usedCodes == nil {
		f.usedCodes = map[string]struct{}{}
	}
	f.usedCodes[code] = struct{}{}
	delete(f.pendingCodes, code)
	delete(f.retained, code)
}

func (f *Flow) persist(conversion Conversion) (Registration, error) {
	if conversion.ID <= 0 || strings.TrimSpace(conversion.PEM) == "" || strings.TrimSpace(conversion.WebhookSecret) == "" {
		return Registration{}, fmt.Errorf("manifest conversion is missing id, pem, or webhook secret")
	}
	appID := fmt.Sprintf("%d", conversion.ID)
	files := []struct {
		path string
		body []byte
	}{
		{f.Paths.AppID, []byte(appID + "\n")},
		{f.Paths.PrivateKey, []byte(conversion.PEM)},
		{f.Paths.WebhookSecret, []byte(conversion.WebhookSecret + "\n")},
	}
	staged := make([]string, 0, len(files))
	cleanupStaged := true
	defer func() {
		if cleanupStaged {
			for _, path := range staged {
				_ = os.Remove(path)
			}
		}
	}()
	for i, file := range files {
		if f.StageFailAfter > 0 && i >= f.StageFailAfter {
			return Registration{}, fmt.Errorf("injected credential staging failure")
		}
		tmp, err := stageCredential(file.path, file.body, f.ownerUID())
		if err != nil {
			return Registration{}, err
		}
		staged = append(staged, tmp)
	}
	renameFile := f.rename
	if renameFile == nil {
		renameFile = os.Rename
	}
	if err := publishCredentialSet(files, staged, f.ownerUID(), renameFile); err != nil {
		return Registration{}, err
	}
	cleanupStaged = false
	return Registration{
		AppID:       appID,
		Slug:        conversion.Slug,
		OwnerLogin:  conversion.Owner.Login,
		Permissions: conversion.Permissions,
		Events:      append([]string(nil), conversion.Events...),
		WebhookURL:  conversion.HookAttributes.URL,
	}, nil
}

func (f *Flow) loadState() (State, error) {
	info, err := os.Lstat(f.StatePath)
	if err != nil {
		return State{}, fmt.Errorf("read manifest state: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != stateFileMode {
		return State{}, fmt.Errorf("manifest state file must be a regular mode 0600 file")
	}
	file, err := os.Open(f.StatePath)
	if err != nil {
		return State{}, err
	}
	defer file.Close()
	var state State
	if err := json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&state); err != nil {
		return State{}, fmt.Errorf("decode manifest state: %w", err)
	}
	return state, nil
}

func (f *Flow) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Flow) ownerUID() int {
	if f.OwnerUID != 0 {
		return f.OwnerUID
	}
	return os.Getuid()
}

func (f *Flow) logCompleted(registration Registration) {
	logger := f.Log
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("github app manifest converted",
		"app_id", registration.AppID,
		"slug", registration.Slug,
		"owner", registration.OwnerLogin,
	)
}

func (f *Flow) wipe(conversion *Conversion) {
	conversion.ClientID = ""
	conversion.ClientSecret = strings.Repeat("\x00", len(conversion.ClientSecret))
	conversion.ClientSecret = ""
	conversion.WebhookSecret = strings.Repeat("\x00", len(conversion.WebhookSecret))
	conversion.WebhookSecret = ""
	conversion.PEM = strings.Repeat("\x00", len(conversion.PEM))
	conversion.PEM = ""
}

// HTTPExchanger is the live GitHub conversion client. Segment 3 tests do not use it.
type HTTPExchanger struct {
	BaseURL string
	Client  *http.Client
}

// Convert posts the one-time code without a personal token.
func (e HTTPExchanger) Convert(code string) (Conversion, error) {
	base := strings.TrimRight(strings.TrimSpace(e.BaseURL), "/")
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequest(http.MethodPost, base+fmt.Sprintf(exchangeURLPath, code), nil)
	if err != nil {
		return Conversion{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Conversion{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Conversion{}, err
	}
	if resp.StatusCode != http.StatusCreated {
		return Conversion{}, fmt.Errorf("manifest conversion status %d", resp.StatusCode)
	}
	var conversion Conversion
	if err := json.Unmarshal(body, &conversion); err != nil {
		return Conversion{}, err
	}
	for i := range body {
		body[i] = 0
	}
	return conversion, nil
}

// publishCredentialSet copies every existing credential aside before any
// live path changes. Publication then replaces the live files one by one.
// Any failure restores the copies and removes files this attempt created, so
// the caller still sees the previous set.
func publishCredentialSet(files []struct {
	path string
	body []byte
}, staged []string, uid int, renameFile func(string, string) error) error {
	backups := make([]string, len(files))
	existed := make([]bool, len(files))
	for i, file := range files {
		if _, err := os.Lstat(file.path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("read previous credential %s: %w", filepath.Base(file.path), err)
		}
		existed[i] = true
		tmp, err := copyCredential(file.path, uid)
		if err != nil {
			removeCredentialTemps(backups)
			return err
		}
		backups[i] = tmp
	}
	replaced := 0
	err := publishStaged(files, staged, uid, renameFile, &replaced)
	if err != nil {
		restoreErr := restoreCredentialSet(files, backups, existed, replaced)
		removeCredentialTemps(backups)
		if restoreErr != nil {
			return fmt.Errorf("%w; restore credential set: %v", err, restoreErr)
		}
		return err
	}
	removeCredentialTemps(backups)
	return nil
}

func publishStaged(files []struct {
	path string
	body []byte
}, staged []string, uid int, renameFile func(string, string) error, replaced *int) error {
	for i, file := range files {
		if err := renameFile(staged[i], file.path); err != nil {
			return fmt.Errorf("commit credential file %s: %w", filepath.Base(file.path), err)
		}
		*replaced = i + 1
		if err := confirmFile(file.path, uid); err != nil {
			return err
		}
	}
	return nil
}

func restoreCredentialSet(files []struct {
	path string
	body []byte
}, backups []string, existed []bool, replaced int) error {
	for i := replaced - 1; i >= 0; i-- {
		if existed[i] {
			if err := os.Rename(backups[i], files[i].path); err != nil {
				return err
			}
			backups[i] = ""
			continue
		}
		if err := os.Remove(files[i].path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func copyCredential(path string, uid int) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("copy previous credential %s: %w", filepath.Base(path), err)
	}
	return stageCredential(path, body, uid)
}

func removeCredentialTemps(paths []string) {
	for _, path := range paths {
		if path != "" {
			_ = os.Remove(path)
		}
	}
}

func writeNewFile(path string, body []byte, uid int) error {
	tmpName, err := stageCredential(path, body, uid)
	if err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// stageCredential writes one new 0600 file next to path and returns its name.
// The name is random, so a leftover temp file cannot block the next attempt.
// The caller renames it onto the destination or removes it.
func stageCredential(path string, body []byte, uid int) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("credential path is empty")
	}
	dir := filepath.Dir(path)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("credential directory is not ready")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	tmpName := filepath.Join(dir, "."+filepath.Base(path)+"."+hex.EncodeToString(nonce[:])+".tmp")
	tmp, err := os.OpenFile(tmpName, os.O_RDWR|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, stateFileMode)
	if err != nil {
		return "", fmt.Errorf("create credential temp file: %w", err)
	}
	closed := false
	ok := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		return "", err
	}
	if err := tmp.Chmod(stateFileMode); err != nil {
		return "", err
	}
	if uid > 0 {
		if err := tmp.Chown(uid, -1); err != nil && !errors.Is(err, syscall.EPERM) {
			return "", err
		}
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	closed = true
	ok = true
	return tmpName, nil
}

func confirmFile(path string, uid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != stateFileMode {
		return fmt.Errorf("credential file %s is not a regular mode 0600 file", filepath.Base(path))
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("credential file owner is unavailable")
	}
	if uid > 0 && int(stat.Uid) != uid {
		return fmt.Errorf("credential file owner is not the process user")
	}
	return nil
}

// DecodeConversionFixture parses a fixture body. The caller wipes the result.
func DecodeConversionFixture(body []byte) (Conversion, error) {
	var conversion Conversion
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&conversion); err != nil {
		return Conversion{}, err
	}
	return conversion, nil
}
