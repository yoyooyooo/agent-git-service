package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ngaut/agent-git-service/internal/attribution"
	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/gorm"
)

// ProjectionAttributionRecorder writes provenance without creating an authorized
// service-user context. Worker attribution must never become authority.
func ProjectionAttributionRecorder(database *gorm.DB) func(context.Context, attribution.Event) error {
	return func(ctx context.Context, event attribution.Event) error {
		gdb := database
		if scoped, ok := DBFromContext(ctx); ok {
			gdb = scoped
		}
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		row := db.AuditLogEntry{Action: "projection." + event.Op, ActorLogin: event.Actor.Login, RepositoryFullName: event.Actor.Repo, TargetLogin: firstNonEmpty(event.TargetLogin, event.SudoAs), Details: string(payload), CreatedAt: time.Now().UTC()}
		if event.Actor.UserID != 0 {
			id := event.Actor.UserID
			row.ActorID = &id
		}
		return gdb.WithContext(ctx).Create(&row).Error
	}
}
