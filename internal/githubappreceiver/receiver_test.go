package githubappreceiver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	if !strings.Contains(rec.Body.String(), "payload exceeds 10 MiB") {
		t.Fatalf("body %s", rec.Body.String())
	}
	if _, ok := store.Body("123456", "delivery-large"); ok {
		t.Fatal("oversized body was stored")
	}
}

type failReader struct {
	payload []byte
	err     error
	n       int
}

func (r *failReader) Read(p []byte) (int, error) {
	if r.n < len(r.payload) {
		n := copy(p, r.payload[r.n:])
		r.n += n
		return n, nil
	}
	return 0, r.err
}

func TestReceiverReadFailuresAreNotOversized(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "unexpected eof", err: io.ErrUnexpectedEOF},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "timeout", err: timeoutError{}},
		{name: "closed", err: net.ErrClosed},
		{name: "reset", err: errors.New("read tcp 127.0.0.1:443: connection reset by peer")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemoryStore()
			var logs bytes.Buffer
			receiver := Receiver{
				AppID:  "123456",
				Secret: "fixture-secret",
				Store:  store,
				Log:    slog.New(slog.NewTextHandler(&logs, nil)),
			}
			body := []byte(`{"action":"opened"}`)
			req := httptest.NewRequest(http.MethodPost, Path, &failReader{payload: body, err: tc.err})
			req.Header.Set(headerDelivery, "delivery-read-fail")
			req.Header.Set(headerSignature, sign("fixture-secret", body))
			rec := httptest.NewRecorder()
			receiver.Handler().ServeHTTP(rec, req)
			if rec.Code == http.StatusRequestEntityTooLarge {
				t.Fatalf("read error classified as 413: %s", rec.Body.String())
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "payload read failed") {
				t.Fatalf("body %s", rec.Body.String())
			}
			if _, ok := store.Body("123456", "delivery-read-fail"); ok {
				t.Fatal("failed read was stored")
			}
			if !strings.Contains(logs.String(), "payload read failed") || !strings.Contains(logs.String(), "bytes") {
				t.Fatalf("log %s", logs.String())
			}
		})
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestReceiverSlowSmallBodyTimesOutAsReadFailure(t *testing.T) {
	store := NewMemoryStore()
	receiver := Receiver{AppID: "123456", Secret: "fixture-secret", Store: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	server := httptest.NewUnstartedServer(receiver.Handler())
	server.Config.ReadTimeout = 200 * time.Millisecond
	server.Start()
	defer server.Close()

	body := []byte(`{"action":"opened"}`)
	req, err := http.NewRequest(http.MethodPost, server.URL+Path, &slowReader{payload: body, delay: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(headerDelivery, "delivery-slow")
	req.Header.Set(headerSignature, sign("fixture-secret", body))
	req.ContentLength = int64(len(body))
	client := &http.Client{Timeout: 3 * time.Second}
	started := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		t.Fatalf("slow body classified as 413 after %s: %s", elapsed, raw)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d after %s body %s", resp.StatusCode, elapsed, raw)
	}
	if !strings.Contains(string(raw), "payload read failed") {
		t.Fatalf("body %s", raw)
	}
	if elapsed < 150*time.Millisecond || elapsed > 900*time.Millisecond {
		t.Fatalf("elapsed %s, want near the read deadline", elapsed)
	}
	if _, ok := store.Body("123456", "delivery-slow"); ok {
		t.Fatal("timed out body was stored")
	}
}

type slowReader struct {
	payload []byte
	delay   time.Duration
	n       int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.n >= len(r.payload) {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	n := copy(p, r.payload[r.n:])
	r.n += n
	return n, nil
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
