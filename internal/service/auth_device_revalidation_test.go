package service_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Approval is historical evidence, not continuing authority to mint a token.
// Use a real isolated database; do not contact a deployed authority or provider.
func TestExchangeDeviceCodeRevalidatesApproverBeforeTokenWrite(t *testing.T) {
	for _, status := range []string{db.UserStatusActive, db.UserStatusBanned, db.UserStatusSuspended, db.UserStatusDeleted} {
		for _, existing := range []bool{false, true} {
			name := status + "/new-token"
			if existing {
				name = status + "/existing-token"
			}
			t.Run(name, func(t *testing.T) {
				database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "auth.db")), &gorm.Config{})
				if err != nil {
					t.Fatal(err)
				}
				sqlDB, err := database.DB()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = sqlDB.Close() })
				if err := database.AutoMigrate(&db.User{}, &db.Token{}, &db.DeviceCode{}); err != nil {
					t.Fatal(err)
				}
				approver := db.User{Login: "approval-owner", Type: db.TypeUser, Status: db.UserStatusActive}
				if err := database.Create(&approver).Error; err != nil {
					t.Fatal(err)
				}
				code := db.DeviceCode{DeviceCode: "revalidation-device", UserCode: "REVALIDATE", State: db.DeviceCodeStateApproved, ApprovedBy: &approver.ID, AccessToken: "fixture-device-access", ExpiresAt: time.Now().UTC().Add(time.Minute)}
				if err := database.Create(&code).Error; err != nil {
					t.Fatal(err)
				}
				previousUse := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
				if existing {
					if err := database.Create(&db.Token{UserID: approver.ID, Value: code.AccessToken, LastUsedAt: &previousUse}).Error; err != nil {
						t.Fatal(err)
					}
				}
				if err := database.Model(&approver).Update("status", status).Error; err != nil {
					t.Fatal(err)
				}
				svc := &service.Service{DB: database}
				token, err := svc.ExchangeDeviceCode(context.Background(), code.DeviceCode)
				if status == db.UserStatusActive {
					if err != nil || token != code.AccessToken {
						t.Fatalf("active approver failed: %v", err)
					}
					return
				}
				if !errors.Is(err, service.ErrForbidden) || token != "" {
					t.Errorf("inactive approver returned token=%t error=%v", token != "", err)
				}
				var rows []db.Token
				if err := database.Where("value = ?", code.AccessToken).Find(&rows).Error; err != nil {
					t.Fatal(err)
				}
				if !existing && len(rows) != 0 {
					t.Fatal("inactive approver created a token")
				}
				if existing && (len(rows) != 1 || rows[0].LastUsedAt == nil || !rows[0].LastUsedAt.Equal(previousUse)) {
					t.Fatal("inactive approver changed an existing token")
				}
			})
		}
	}
}
