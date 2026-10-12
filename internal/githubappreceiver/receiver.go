// Package githubappreceiver is the dedicated GitHub App webhook receiver.
// It verifies signatures, bounds the body, and records deliveries. It has no
// Git client, no AGS database, and no GitHub write credential. A successful
// response means received, duplicate, or quarantined; a spool failure is 503
// and is never acknowledged first.
package githubappreceiver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

const (
	// Path is the only route this receiver registers.
	Path = "/api/ext/v1/integrations/github/events"
	// MaxBodyBytes is the GitHub webhook body limit.
	MaxBodyBytes = 10 << 20
	// Provider is the fixed delivery-key host.
	Provider = "github.com"

	headerDelivery  = "X-GitHub-Delivery"
	headerSignature = "X-Hub-Signature-256"
	headerEvent     = "X-GitHub-Event"

	statusReceived   = "received"
	statusDuplicate  = "duplicate"
	statusQuarantine = "quarantined"
	statusConflict   = "conflict"
)

// Delivery is the persisted receipt for one GitHub delivery GUID.
type Delivery struct {
	Provider       string `json:"provider"`
	AppID          string `json:"app_id"`
	DeliveryID     string `json:"delivery_id"`
	Digest         string `json:"digest"`
	Event          string `json:"event"`
	InstallationID int64  `json:"installation_id,omitempty"`
	RepositoryID   int64  `json:"repository_id,omitempty"`
	Status         string `json:"status"`
}

// Envelope is what intake receives after a delivery is stored. The body is the
// raw request bytes, not a re-encoded document.
type Envelope struct {
	Delivery Delivery
	Headers  map[string]string
	Body     []byte
}

// Store persists raw deliveries and remembers their digest.
type Store interface {
	// Save returns the existing delivery when the GUID was already stored.
	// ErrSpoolUnavailable means nothing was committed.
	Save(delivery Delivery, headers map[string]string, body []byte) (Delivery, error)
}

// ErrSpoolUnavailable is returned when the spool cannot commit a delivery.
var ErrSpoolUnavailable = errors.New("github app webhook spool unavailable")

// Receiver verifies and records GitHub App webhook deliveries.
type Receiver struct {
	AppID  string
	Secret string
	Store  Store
	// Intake is optional. A nil intake quarantines a verified new delivery
	// after it has been stored.
	Intake func(Envelope) error
	Log    *slog.Logger
}

// Handler is the only HTTP handler. It accepts POST Path.
func (r Receiver) Handler() http.Handler {
	return http.HandlerFunc(r.serveHTTP)
}

func (r Receiver) serveHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || req.URL.Path != Path {
		http.NotFound(w, req)
		return
	}
	body, tooLarge := readBody(req.Body)
	if tooLarge {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload exceeds 10 MiB"})
		return
	}
	deliveryID := strings.TrimSpace(req.Header.Get(headerDelivery))
	if !validDeliveryID(deliveryID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing delivery id"})
		return
	}
	digest, err := verifySignature(r.Secret, req.Header.Get(headerSignature), body)
	if err != nil {
		r.logDelivery(deliveryID, 0, 0, "rejected")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
		return
	}
	installationID, repositoryID := identifyPayload(body)
	eventName := strings.TrimSpace(req.Header.Get(headerEvent))
	incoming := Delivery{
		Provider:       Provider,
		AppID:          r.AppID,
		DeliveryID:     deliveryID,
		Digest:         digest,
		Event:          eventName,
		InstallationID: installationID,
		RepositoryID:   repositoryID,
		Status:         statusReceived,
	}
	headers := selectedHeaders(req.Header)
	stored, err := r.Store.Save(incoming, headers, body)
	if errors.Is(err, ErrSpoolUnavailable) {
		r.logDelivery(deliveryID, installationID, repositoryID, "spool_unavailable")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delivery not stored"})
		return
	}
	if err != nil {
		r.logDelivery(deliveryID, installationID, repositoryID, "store_failed")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delivery not stored"})
		return
	}
	if stored.Digest != digest {
		r.logDelivery(deliveryID, installationID, repositoryID, statusConflict)
		writeJSON(w, http.StatusConflict, map[string]string{
			"status":      statusConflict,
			"delivery_id": deliveryID,
		})
		return
	}
	status := stored.Status
	if status == "" {
		status = statusReceived
	}
	if status == statusReceived && r.Intake != nil {
		if err := r.Intake(Envelope{Delivery: stored, Headers: headers, Body: body}); err != nil {
			status = statusQuarantine
		}
	}
	if status == statusReceived && r.Intake == nil {
		status = statusQuarantine
	}
	r.logDelivery(deliveryID, installationID, repositoryID, status)
	writeJSON(w, http.StatusOK, map[string]string{
		"status":      status,
		"delivery_id": deliveryID,
	})
}

func readBody(src io.Reader) ([]byte, bool) {
	limited := io.LimitReader(src, MaxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, true
	}
	return body, len(body) > MaxBodyBytes
}

func verifySignature(secret, header string, body []byte) (string, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", fmt.Errorf("webhook secret is not configured")
	}
	header = strings.TrimSpace(header)
	prefix, encoded, ok := strings.Cut(header, "=")
	if !ok || prefix != "sha256" || encoded == "" {
		return "", fmt.Errorf("webhook signature is missing")
	}
	got, err := hex.DecodeString(encoded)
	if err != nil || len(got) != sha256.Size {
		return "", fmt.Errorf("webhook signature is invalid")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	expected := mac.Sum(nil)
	if !hmac.Equal(got, expected) {
		return "", fmt.Errorf("webhook signature mismatch")
	}
	return "sha256:" + hex.EncodeToString(expected), nil
}

func validDeliveryID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func identifyPayload(body []byte) (installationID, repositoryID int64) {
	var payload struct {
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Repository struct {
			ID int64 `json:"id"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, 0
	}
	return payload.Installation.ID, payload.Repository.ID
}

func selectedHeaders(header http.Header) map[string]string {
	out := map[string]string{}
	for _, name := range []string{headerDelivery, headerEvent, "X-GitHub-Hook-ID", "X-GitHub-Hook-Installation-Target-ID", "X-GitHub-Hook-Installation-Target-Type", "User-Agent", "Content-Type"} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			out[name] = value
		}
	}
	return out
}

func (r Receiver) logDelivery(deliveryID string, installationID, repositoryID int64, status string) {
	logger := r.Log
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("github app webhook delivery",
		"delivery_id", deliveryID,
		"installation_id", installationID,
		"repository_id", repositoryID,
		"status", status,
	)
}

func writeJSON(w http.ResponseWriter, code int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
