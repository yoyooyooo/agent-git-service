package service_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestAuthorityBoundaryReceiptAppendOnlyIdempotentAndRecomputed(t *testing.T) {
	svc, cleanup := setupDelegatedSessionService(t)
	defer cleanup()
	svc.SourceRevision = strings.Repeat("a", 40)
	var admin db.User
	if err := svc.DB.First(&admin, "login = ?", "operator").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.User{}).Where("id = ?", admin.ID).Update("site_admin", true).Error; err != nil {
		t.Fatal(err)
	}
	admin.SiteAdmin = true
	ctx := service.ContextWithUser(context.Background(), admin)
	epoch, err := svc.CurrentAuthorityEpoch()
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.CaptureLegacyAuthorityBoundary(ctx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CaptureLegacyAuthorityBoundary(ctx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReceiptID != second.ReceiptID || first.SnapshotDigest != second.SnapshotDigest || !first.CreatedAt.Equal(second.CreatedAt) {
		t.Fatalf("idempotent capture drifted: first=%#v second=%#v", first, second)
	}
	var count int64
	if err := svc.DB.Model(&db.AuthorityBoundaryReceipt{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("receipt count=%d err=%v", count, err)
	}
	payload := string(first.Payload)
	for _, forbidden := range []string{"key_ids", "secret-token", "credential_hash", "session_token"} {
		if strings.Contains(strings.ToLower(payload), forbidden) {
			t.Fatalf("payload contains forbidden field/value %q: %s", forbidden, payload)
		}
	}
	if !strings.Contains(payload, `"principals"`) || !strings.Contains(payload, `"site_admin":false`) || !strings.Contains(payload, `"binding_revision"`) {
		t.Fatalf("payload lacks principal/binding readback: %s", payload)
	}
	if _, err := svc.GetAuthorityBoundaryReceipt(ctx, first.ReceiptID); err != nil {
		t.Fatalf("digest readback: %v", err)
	}

	var row db.AuthorityBoundaryReceipt
	if err := svc.DB.First(&row, "receipt_id = ?", first.ReceiptID).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&row).Update("claim_limit", "changed").Error; !errors.Is(err, db.ErrAuthorityBoundaryReceiptImmutable) {
		t.Fatalf("update error=%v", err)
	}
	if err := svc.DB.Delete(&row).Error; !errors.Is(err, db.ErrAuthorityBoundaryReceiptImmutable) {
		t.Fatalf("delete error=%v", err)
	}
}

func TestAuthorityBoundaryReceiptConcurrentCaptureConvergesToOneRow(t *testing.T) {
	svc, cleanup := setupDelegatedSessionService(t)
	defer cleanup()
	svc.SourceRevision = strings.Repeat("e", 40)
	var admin db.User
	if err := svc.DB.First(&admin, "login = ?", "operator").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.User{}).Where("id = ?", admin.ID).Update("site_admin", true).Error; err != nil {
		t.Fatal(err)
	}
	admin.SiteAdmin = true
	secondAdmin := db.User{Login: "receipt-admin-two", Type: db.TypeUser, Status: db.UserStatusActive, SiteAdmin: true}
	if err := svc.DB.Create(&secondAdmin).Error; err != nil {
		t.Fatal(err)
	}
	contexts := []context.Context{
		service.ContextWithUser(context.Background(), admin),
		service.ContextWithUser(context.Background(), secondAdmin),
	}
	epoch, err := svc.CurrentAuthorityEpoch()
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan service.AuthorityBoundaryReceipt, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, captureCtx := range contexts {
		wg.Add(1)
		go func(captureCtx context.Context) {
			defer wg.Done()
			<-start
			receipt, captureErr := svc.CaptureLegacyAuthorityBoundary(captureCtx, epoch)
			results <- receipt
			errs <- captureErr
		}(captureCtx)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for captureErr := range errs {
		if captureErr != nil {
			t.Fatalf("concurrent capture: %v", captureErr)
		}
	}
	var receiptID string
	for receipt := range results {
		if receiptID == "" {
			receiptID = receipt.ReceiptID
		} else if receipt.ReceiptID != receiptID {
			t.Fatalf("concurrent receipt IDs differ: %s != %s", receipt.ReceiptID, receiptID)
		}
	}
	var count int64
	if err := svc.DB.Model(&db.AuthorityBoundaryReceipt{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("receipt count=%d err=%v", count, err)
	}
}

func TestAuthorityBoundaryReceiptRetriesAmbiguousWholeTransactionConflict(t *testing.T) {
	svc, cleanup := setupDelegatedSessionService(t)
	defer cleanup()
	svc.SourceRevision = strings.Repeat("f", 40)
	var admin db.User
	if err := svc.DB.First(&admin, "login = ?", "operator").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.User{}).Where("id = ?", admin.ID).Update("site_admin", true).Error; err != nil {
		t.Fatal(err)
	}
	admin.SiteAdmin = true
	ctx := service.ContextWithUser(context.Background(), admin)
	epoch, err := svc.CurrentAuthorityEpoch()
	if err != nil {
		t.Fatal(err)
	}
	var attempts int
	service.SetAuthorityBoundaryTransactionHookForTest(svc, func(_ int, run func(*gorm.DB) error) error {
		attempts++
		if err := svc.DB.Transaction(run); err != nil {
			return err
		}
		// Every invocation reports an ambiguous result after a real commit.
		// Correct code must resolve the first result by base-DB integrity readback
		// rather than retrying until it falsely reports failure.
		return errors.New("write conflict after commit")
	})
	t.Cleanup(func() { service.SetAuthorityBoundaryTransactionHookForTest(svc, nil) })
	started := time.Now()
	receipt, err := svc.CaptureLegacyAuthorityBoundary(ctx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || time.Since(started) >= 2*time.Second {
		t.Fatalf("ambiguous commit was not resolved by immediate integrity readback: attempts=%d elapsed=%s", attempts, time.Since(started))
	}
	var count int64
	if err := svc.DB.Model(&db.AuthorityBoundaryReceipt{}).Where("receipt_id = ?", receipt.ReceiptID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("receipt count=%d err=%v", count, err)
	}
}

func TestAuthorityBoundaryReceiptAmbiguousCommitReadbackStaysOnTenantDB(t *testing.T) {
	svc, cleanup := setupDelegatedSessionService(t)
	defer cleanup()
	svc.SourceRevision = strings.Repeat("8", 40)

	tenant, err := gorm.Open(sqlite.Open("file:"+t.TempDir()+"/tenant.db?_busy_timeout=5000"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.AutoMigrate(&db.User{}, &db.AuthorityBoundaryReceipt{}); err != nil {
		t.Fatal(err)
	}
	var users []db.User
	if err := svc.DB.Find(&users).Error; err != nil {
		t.Fatal(err)
	}
	for index := range users {
		if users[index].Login == "operator" {
			users[index].SiteAdmin = true
		}
	}
	if err := tenant.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	var admin db.User
	if err := tenant.First(&admin, "login = ?", "operator").Error; err != nil {
		t.Fatal(err)
	}
	ctx := service.ContextWithDB(service.ContextWithUser(context.Background(), admin), tenant)
	epoch, err := svc.CurrentAuthorityEpoch()
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	service.SetAuthorityBoundaryTransactionHookForTest(svc, func(_ int, run func(*gorm.DB) error) error {
		attempts++
		if err := tenant.Transaction(run); err != nil {
			return err
		}
		return errors.New("write conflict after tenant commit")
	})
	defer service.SetAuthorityBoundaryTransactionHookForTest(svc, nil)
	receipt, err := svc.CaptureLegacyAuthorityBoundary(ctx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d", attempts)
	}
	var tenantCount, defaultCount int64
	if err := tenant.Model(&db.AuthorityBoundaryReceipt{}).Where("receipt_id = ?", receipt.ReceiptID).Count(&tenantCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.AuthorityBoundaryReceipt{}).Where("receipt_id = ?", receipt.ReceiptID).Count(&defaultCount).Error; err != nil {
		t.Fatal(err)
	}
	if tenantCount != 1 || defaultCount != 0 {
		t.Fatalf("tenant count=%d default count=%d", tenantCount, defaultCount)
	}
}

func TestAuthorityBoundaryReceiptRetryStopsAtAttemptAndBudgetBounds(t *testing.T) {
	setup := func(t *testing.T) (*service.Service, context.Context, string, func()) {
		t.Helper()
		svc, cleanup := setupDelegatedSessionService(t)
		svc.SourceRevision = strings.Repeat("9", 40)
		var admin db.User
		if err := svc.DB.First(&admin, "login = ?", "operator").Error; err != nil {
			cleanup()
			t.Fatal(err)
		}
		if err := svc.DB.Model(&db.User{}).Where("id = ?", admin.ID).Update("site_admin", true).Error; err != nil {
			cleanup()
			t.Fatal(err)
		}
		admin.SiteAdmin = true
		epoch, err := svc.CurrentAuthorityEpoch()
		if err != nil {
			cleanup()
			t.Fatal(err)
		}
		return svc, service.ContextWithUser(context.Background(), admin), epoch, cleanup
	}

	t.Run("five attempts", func(t *testing.T) {
		svc, ctx, epoch, cleanup := setup(t)
		defer cleanup()
		attempts := 0
		service.SetAuthorityBoundaryTransactionHookForTest(svc, func(_ int, _ func(*gorm.DB) error) error {
			attempts++
			return errors.New("write conflict before commit")
		})
		defer service.SetAuthorityBoundaryTransactionHookForTest(svc, nil)
		if _, err := svc.CaptureLegacyAuthorityBoundary(ctx, epoch); err == nil || !strings.Contains(err.Error(), "write conflict") {
			t.Fatalf("capture error=%v", err)
		}
		if attempts != 5 {
			t.Fatalf("attempts=%d want=5", attempts)
		}
	})

	t.Run("caller budget", func(t *testing.T) {
		svc, baseCtx, epoch, cleanup := setup(t)
		defer cleanup()
		ctx, cancel := context.WithTimeout(baseCtx, 25*time.Millisecond)
		defer cancel()
		attempts := 0
		service.SetAuthorityBoundaryTransactionHookForTest(svc, func(_ int, _ func(*gorm.DB) error) error {
			attempts++
			time.Sleep(30 * time.Millisecond)
			return errors.New("write conflict before commit")
		})
		defer service.SetAuthorityBoundaryTransactionHookForTest(svc, nil)
		started := time.Now()
		if _, err := svc.CaptureLegacyAuthorityBoundary(ctx, epoch); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("capture error=%v", err)
		}
		if attempts != 1 || time.Since(started) >= 250*time.Millisecond {
			t.Fatalf("attempts=%d elapsed=%s", attempts, time.Since(started))
		}
	})
}

func TestDelegatedEffectBoundaryReceiptUsesCurrentSessionAndServerPayload(t *testing.T) {
	svc, _, _ := setupAccessGrantService(t)
	svc.SourceRevision = strings.Repeat("d", 40)
	_, ctx, session := issuedAccessGrantTransportContext(t, svc)
	epoch, err := svc.CurrentAuthorityEpoch()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := svc.CaptureDelegatedEffectBoundary(ctx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Kind != service.AuthorityBoundaryReceiptDelegatedEffectKind || receipt.ParentDigest != session.PolicySnapshotHash || !strings.Contains(string(receipt.Payload), session.ID) {
		t.Fatalf("delegated receipt=%#v payload=%s", receipt, receipt.Payload)
	}
	var payload map[string]any
	if err := json.Unmarshal(receipt.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	assertExactBoundaryKeys(t, payload, []string{"schema", "session_id", "principal_id", "repository_id", "repository", "operation", "capabilities", "membership_epoch", "policy_snapshot_hash", "native_grant_revision"})
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	assertExactBoundaryKeys(t, envelope, []string{"receipt_id", "kind", "parent_digest", "authority_epoch", "snapshot_digest", "source_revision", "claim_limit", "payload", "created_by_user_id", "created_at"})
}

func TestDelegatedEffectBoundaryReceiptReadbackIsOriginatingSessionScoped(t *testing.T) {
	svc, _, _ := setupAccessGrantService(t)
	svc.SourceRevision = strings.Repeat("6", 40)
	_, originatingCtx, _ := issuedAccessGrantTransportContext(t, svc)
	_, otherCtx, _ := issuedAccessGrantTransportContext(t, svc)
	epoch, err := svc.CurrentAuthorityEpoch()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := svc.CaptureDelegatedEffectBoundary(originatingCtx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetCurrentDelegatedEffectBoundaryReceipt(originatingCtx, receipt.ReceiptID); err != nil {
		t.Fatalf("originating Session readback: %v", err)
	}
	if _, err := svc.GetCurrentDelegatedEffectBoundaryReceipt(otherCtx, receipt.ReceiptID); !errors.Is(err, service.ErrAuthorityBoundaryReceiptDenied) {
		t.Fatalf("same-principal second Session readback error=%v", err)
	}
	principal, ok := service.UserFromContext(originatingCtx)
	if !ok {
		t.Fatal("originating principal missing")
	}
	if _, err := svc.GetAuthorityBoundaryReceipt(service.ContextWithUser(context.Background(), principal), receipt.ReceiptID); !errors.Is(err, service.ErrAuthorityBoundaryReceiptDenied) {
		t.Fatalf("durable principal bypassed Session scope: %v", err)
	}
	if err := svc.DB.Model(&db.User{}).Where("id = ?", principal.ID).Update("site_admin", true).Error; err != nil {
		t.Fatal(err)
	}
	principal.SiteAdmin = true
	if _, err := svc.GetAuthorityBoundaryReceipt(service.ContextWithUser(context.Background(), principal), receipt.ReceiptID); err != nil {
		t.Fatalf("site-admin audit readback: %v", err)
	}
	if _, err := svc.GetCurrentDelegatedEffectBoundaryReceipt(service.ContextWithUser(context.Background(), principal), receipt.ReceiptID); !errors.Is(err, service.ErrAuthorityBoundaryReceiptDenied) {
		t.Fatalf("site admin without originating Session used Session route: %v", err)
	}
}

func assertExactBoundaryKeys(t *testing.T, value map[string]any, expected []string) {
	t.Helper()
	if len(value) != len(expected) {
		t.Fatalf("keys=%v, want exactly %v", value, expected)
	}
	for _, key := range expected {
		if _, ok := value[key]; !ok {
			t.Fatalf("missing key %q in %#v", key, value)
		}
	}
}

func TestAuthorityBoundaryReceiptReadbackRejectsRecomputedSchemaEpochAndSourceCorruption(t *testing.T) {
	svc, cleanup := setupDelegatedSessionService(t)
	defer cleanup()
	svc.SourceRevision = strings.Repeat("a", 40)
	var admin db.User
	if err := svc.DB.First(&admin, "login = ?", "operator").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.User{}).Where("id = ?", admin.ID).Update("site_admin", true).Error; err != nil {
		t.Fatal(err)
	}
	admin.SiteAdmin = true
	ctx := service.ContextWithUser(context.Background(), admin)
	epoch, err := svc.CurrentAuthorityEpoch()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := svc.CaptureLegacyAuthorityBoundary(ctx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	var original db.AuthorityBoundaryReceipt
	if err := svc.DB.First(&original, "receipt_id = ?", receipt.ReceiptID).Error; err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name        string
		mutate      func(*db.AuthorityBoundaryReceipt)
		recomputeID bool
	}{
		{name: "kind schema mismatch", recomputeID: true, mutate: func(row *db.AuthorityBoundaryReceipt) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
				t.Fatal(err)
			}
			payload["schema"] = "ags.authority-boundary.wrong.v1"
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			row.Payload = string(encoded)
			row.SnapshotDigest = testSHA256Hex(encoded)
		}},
		{name: "uppercase authority epoch", recomputeID: true, mutate: func(row *db.AuthorityBoundaryReceipt) { row.AuthorityEpoch = strings.ToUpper(row.AuthorityEpoch) }},
		{name: "uppercase source revision", recomputeID: true, mutate: func(row *db.AuthorityBoundaryReceipt) { row.SourceRevision = strings.ToUpper(row.SourceRevision) }},
		{name: "unknown top-level payload field", recomputeID: true, mutate: func(row *db.AuthorityBoundaryReceipt) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
				t.Fatal(err)
			}
			payload["unknown"] = true
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			row.Payload, row.SnapshotDigest = string(encoded), testSHA256Hex(encoded)
		}},
		{name: "unknown nested payload field", recomputeID: true, mutate: func(row *db.AuthorityBoundaryReceipt) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
				t.Fatal(err)
			}
			principals, ok := payload["principals"].([]any)
			if !ok || len(principals) == 0 {
				t.Fatal("missing principals fixture")
			}
			principal := principals[0].(map[string]any)
			principal["unknown"] = true
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			row.Payload, row.SnapshotDigest = string(encoded), testSHA256Hex(encoded)
		}},
		{name: "valid payload with wrong snapshot digest", recomputeID: true, mutate: func(row *db.AuthorityBoundaryReceipt) { row.SnapshotDigest = strings.Repeat("1", 64) }},
		{name: "valid payload and digest with wrong receipt id", mutate: func(row *db.AuthorityBoundaryReceipt) { row.ReceiptID = "abr_" + strings.Repeat("2", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := original
			tc.mutate(&row)
			if tc.recomputeID {
				row.ReceiptID = recomputeReceiptIDForTest(t, row)
			}
			if err := svc.DB.Session(&gorm.Session{SkipHooks: true}).Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := svc.GetAuthorityBoundaryReceipt(ctx, row.ReceiptID); !errors.Is(err, service.ErrAuthorityBoundaryReceiptCorrupt) {
				t.Fatalf("corrupt readback error=%v", err)
			}
		})
	}
	if _, err := svc.GetAuthorityBoundaryReceipt(ctx, receipt.SnapshotDigest); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("snapshot digest alias error=%v", err)
	}
}

func recomputeReceiptIDForTest(t *testing.T, row db.AuthorityBoundaryReceipt) string {
	t.Helper()
	identity := struct {
		Kind           string `json:"kind"`
		ParentDigest   string `json:"parent_digest,omitempty"`
		AuthorityEpoch string `json:"authority_epoch"`
		SnapshotDigest string `json:"snapshot_digest"`
		SourceRevision string `json:"source_revision"`
		ClaimLimit     string `json:"claim_limit"`
	}{row.Kind, row.ParentDigest, row.AuthorityEpoch, row.SnapshotDigest, row.SourceRevision, row.ClaimLimit}
	encoded, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	return "abr_" + testSHA256Hex(encoded)
}

func testSHA256Hex(encoded []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func TestAuthorityBoundaryReceiptFailsClosedOnEpochSourceAndCorruption(t *testing.T) {
	svc, cleanup := setupDelegatedSessionService(t)
	defer cleanup()
	var admin db.User
	if err := svc.DB.First(&admin, "login = ?", "operator").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.User{}).Where("id = ?", admin.ID).Update("site_admin", true).Error; err != nil {
		t.Fatal(err)
	}
	admin.SiteAdmin = true
	ctx := service.ContextWithUser(context.Background(), admin)
	epoch, err := svc.CurrentAuthorityEpoch()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CaptureLegacyAuthorityBoundary(ctx, epoch); !errors.Is(err, service.ErrAuthorityBoundaryReceiptSource) {
		t.Fatalf("unknown source error=%v", err)
	}
	svc.SourceRevision = strings.Repeat("b", 40)
	if _, err := svc.CaptureLegacyAuthorityBoundary(ctx, strings.Repeat("0", 64)); !errors.Is(err, service.ErrAuthorityBoundaryReceiptEpoch) {
		t.Fatalf("wrong epoch error=%v", err)
	}
	receipt, err := svc.CaptureLegacyAuthorityBoundary(ctx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Session(&gorm.Session{SkipHooks: true}).Model(&db.AuthorityBoundaryReceipt{}).Where("receipt_id = ?", receipt.ReceiptID).Update("payload", `{"schema":"tampered"}`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetAuthorityBoundaryReceipt(ctx, receipt.ReceiptID); !errors.Is(err, service.ErrAuthorityBoundaryReceiptCorrupt) {
		t.Fatalf("corrupt readback error=%v", err)
	}
	if err := svc.DB.Model(&db.User{}).Where("id = ?", admin.ID).Update("status", db.UserStatusSuspended).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetAuthorityBoundaryReceipt(ctx, receipt.ReceiptID); !errors.Is(err, service.ErrAuthorityBoundaryReceiptDenied) {
		t.Fatalf("inactive receipt viewer error=%v", err)
	}
	if _, err := svc.CaptureLegacyAuthorityBoundary(ctx, epoch); !errors.Is(err, service.ErrAuthorityBoundaryReceiptDenied) {
		t.Fatalf("inactive capture error=%v", err)
	}
}

func TestAuthorityBoundaryReceiptAmbiguousCommitRejectsCorruptReadback(t *testing.T) {
	svc, cleanup := setupDelegatedSessionService(t)
	defer cleanup()
	svc.SourceRevision = strings.Repeat("7", 40)
	var admin db.User
	if err := svc.DB.First(&admin, "login = ?", "operator").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.User{}).Where("id = ?", admin.ID).Update("site_admin", true).Error; err != nil {
		t.Fatal(err)
	}
	admin.SiteAdmin = true
	ctx := service.ContextWithUser(context.Background(), admin)
	epoch, err := svc.CurrentAuthorityEpoch()
	if err != nil {
		t.Fatal(err)
	}
	service.SetAuthorityBoundaryTransactionHookForTest(svc, func(_ int, run func(*gorm.DB) error) error {
		if err := svc.DB.Transaction(run); err != nil {
			return err
		}
		var row db.AuthorityBoundaryReceipt
		if err := svc.DB.Order("created_at DESC").First(&row).Error; err != nil {
			return err
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
			return err
		}
		payload["unknown"] = true
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if err := svc.DB.Session(&gorm.Session{SkipHooks: true}).Model(&db.AuthorityBoundaryReceipt{}).Where("receipt_id = ?", row.ReceiptID).Update("payload", string(encoded)).Error; err != nil {
			return err
		}
		return errors.New("write conflict after corrupt commit")
	})
	defer service.SetAuthorityBoundaryTransactionHookForTest(svc, nil)
	if _, err := svc.CaptureLegacyAuthorityBoundary(ctx, epoch); !errors.Is(err, service.ErrAuthorityBoundaryReceiptCorrupt) {
		t.Fatalf("ambiguous corrupt readback error=%v", err)
	}
}
