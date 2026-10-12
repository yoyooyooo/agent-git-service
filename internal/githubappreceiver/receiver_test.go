package githubappreceiver

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func fixtureBody(installationID, repositoryID int) []byte {
	return []byte(`{"action":"opened","installation":{"id":` + itoa(installationID) + `},"repository":{"id":` + itoa(repositoryID) + `}}`)
}

func itoa(value int) string {
	return strings.TrimPrefix(strings.ReplaceAll(jsonNumber(value), ",", ""), "+")
}

func jsonNumber(value int) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func newRequest(t *testing.T, deliveryID, secret string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	req.Header.Set(headerDelivery, deliveryID)
	req.Header.Set(headerEvent, "push")
	req.Header.Set(headerSignature, sign(secret, body))
	return req
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReceiverAcceptsSignedFixtureAndRejectsBadSignature(t *testing.T) {
	store := NewMemoryStore()
	var got Envelope
	receiver := Receiver{
		AppID:  "123456",
		Secret: "fixture-secret",
		Store:  store,
		Intake: func(envelope Envelope) error {
			got = envelope
			return nil
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body := fixtureBody(42, 99)
	rec := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(rec, newRequest(t, "delivery-1", "fixture-secret", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if decode(t, rec)["status"] != statusReceived {
		t.Fatalf("body %s", rec.Body.String())
	}
	if got.Delivery.InstallationID != 42 || got.Delivery.RepositoryID != 99 {
		t.Fatalf("envelope ids = %+v", got.Delivery)
	}
	if !bytes.Equal(got.Body, body) {
		t.Fatal("intake did not receive the raw body")
	}

	bad := newRequest(t, "delivery-2", "fixture-secret", body)
	bad.Header.Set(headerSignature, sign("other-secret", body))
	rec = httptest.NewRecorder()
	receiver.Handler().ServeHTTP(rec, bad)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature status %d", rec.Code)
	}
	if _, ok := store.Body("123456", "delivery-2"); ok {
		t.Fatal("rejected signature was stored")
	}

	sha1Req := newRequest(t, "delivery-3", "fixture-secret", body)
	sha1Req.Header.Set(headerSignature, "sha1=0123456789abcdef0123456789abcdef01234567")
	rec = httptest.NewRecorder()
	receiver.Handler().ServeHTTP(rec, sha1Req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("sha1 status %d", rec.Code)
	}
}

func TestReceiverLimitsBodyTo10MiB(t *testing.T) {
	store := NewMemoryStore()
	receiver := Receiver{AppID: "123456", Secret: "fixture-secret", Store: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	body := bytes.Repeat([]byte("a"), MaxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	req.Header.Set(headerDelivery, "delivery-large")
	req.Header.Set(headerSignature, sign("fixture-secret", body))
	rec := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", rec.Code)
	}
	if _, ok := store.Body("123456", "delivery-large"); ok {
		t.Fatal("oversized body was stored")
	}
}

func TestReceiverDeliveryIsIdempotentAndDetectsConflict(t *testing.T) {
	store := NewMemoryStore()
	calls := 0
	receiver := Receiver{
		AppID:  "123456",
		Secret: "fixture-secret",
		Store:  store,
		Intake: func(Envelope) error {
			calls++
			return nil
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body := fixtureBody(1, 2)
	rec := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(rec, newRequest(t, "delivery-same", "fixture-secret", body))
	if rec.Code != http.StatusOK || decode(t, rec)["status"] != statusReceived {
		t.Fatalf("first %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	receiver.Handler().ServeHTTP(rec, newRequest(t, "delivery-same", "fixture-secret", body))
	if rec.Code != http.StatusOK || decode(t, rec)["status"] != statusDuplicate {
		t.Fatalf("duplicate %d %s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("intake calls = %d", calls)
	}

	other := fixtureBody(1, 3)
	rec = httptest.NewRecorder()
	receiver.Handler().ServeHTTP(rec, newRequest(t, "delivery-same", "fixture-secret", other))
	if rec.Code != http.StatusConflict || decode(t, rec)["status"] != statusConflict {
		t.Fatalf("conflict %d %s", rec.Code, rec.Body.String())
	}
	stored, ok := store.Body("123456", "delivery-same")
	if !ok || !bytes.Equal(stored, body) {
		t.Fatal("conflict replaced the original body")
	}
}

func TestReceiverReturns503WhenSpoolCannotCommit(t *testing.T) {
	store := NewMemoryStore()
	store.FailNext(1)
	receiver := Receiver{
		AppID:  "123456",
		Secret: "fixture-secret",
		Store:  store,
		Intake: func(Envelope) error { return nil },
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body := fixtureBody(7, 8)
	rec := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(rec, newRequest(t, "delivery-spool", "fixture-secret", body))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := store.Body("123456", "delivery-spool"); ok {
		t.Fatal("503 committed a delivery")
	}

	rec = httptest.NewRecorder()
	receiver.Handler().ServeHTTP(rec, newRequest(t, "delivery-spool", "fixture-secret", body))
	if rec.Code != http.StatusOK || decode(t, rec)["status"] != statusReceived {
		t.Fatalf("retry %d %s", rec.Code, rec.Body.String())
	}
}

func TestReceiverQuarantinesWithoutIntake(t *testing.T) {
	receiver := Receiver{
		AppID:  "123456",
		Secret: "fixture-secret",
		Store:  NewMemoryStore(),
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	rec := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(rec, newRequest(t, "delivery-q", "fixture-secret", fixtureBody(1, 1)))
	if rec.Code != http.StatusOK || decode(t, rec)["status"] != statusQuarantine {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
}

func TestReceiverDoesNotServeOtherPaths(t *testing.T) {
	receiver := Receiver{AppID: "1", Secret: "fixture-secret", Store: NewMemoryStore()}
	rec := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
}
