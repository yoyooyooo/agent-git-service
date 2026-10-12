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
	usedCodes map[string]struct{}
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
func (f *Flow) Complete(code, state string) (Registration, error) {
	code = strings.TrimSpace(code)
	state = strings.TrimSpace(state)
	if code == "" || state == "" {
		return Registration{}, fmt.Errorf("manifest code and state are required")
	}
	if f.usedCodes == nil {
		f.usedCodes = map[string]struct{}{}
	}
	if _, used := f.usedCodes[code]; used {
		return Registration{}, fmt.Errorf("manifest code was already used")
	}
	loaded, err := f.loadState()
	if err != nil {
		return Registration{}, err
	}
	if loaded.Value != state {
		return Registration{}, fmt.Errorf("manifest state does not match")
	}
	if f.now().After(loaded.ExpiresAt) {
		return Registration{}, fmt.Errorf("manifest registration window expired")
	}
	conversion, err := f.Exchanger.Convert(code)
	if err != nil {
		return Registration{}, err
	}
	f.usedCodes[code] = struct{}{}
	registration, err := f.persist(conversion)
	f.wipe(&conversion)
	if err != nil {
		return Registration{}, err
	}
	if err := os.Remove(f.StatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Registration{}, fmt.Errorf("remove manifest state: %w", err)
	}
	f.logCompleted(registration)
	return registration, nil
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
	for _, file := range files {
		if err := writeNewFile(file.path, file.body, f.ownerUID()); err != nil {
			return Registration{}, err
		}
		if err := confirmFile(file.path, f.ownerUID()); err != nil {
			return Registration{}, err
		}
	}
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

func writeNewFile(path string, body []byte, uid int) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("credential path is empty")
	}
	dir := filepath.Dir(path)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("credential directory is not ready")
	}
	tmp, err := os.OpenFile(filepath.Join(dir, "."+filepath.Base(path)+".tmp"), os.O_RDWR|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, stateFileMode)
	if err != nil {
		return fmt.Errorf("create credential temp file: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(stateFileMode); err != nil {
		_ = tmp.Close()
		return err
	}
	if uid > 0 {
		if err := tmp.Chown(uid, -1); err != nil && !errors.Is(err, syscall.EPERM) {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
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
