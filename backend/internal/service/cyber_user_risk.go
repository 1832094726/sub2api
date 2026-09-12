package service

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"log/slog"
	"time"
)

// State is committed together with the downstream user disable operation.
// Implementations serialize by user and deduplicate stable request IDs.
type CyberUserRiskState struct {
	Strikes      int
	BlockedUntil *time.Time
	Applied      bool
}
type CyberUserRiskRepository interface {
	RecordCyberUserStrike(context.Context, int64, string) (CyberUserRiskState, error)
	ResetCyberUserRisk(context.Context, int64) error
}

func (s *ContentModerationService) applyCyberUserEscalation(ctx context.Context, log *ContentModerationLog) bool {
	if log.UserID == nil || *log.UserID <= 0 {
		return false
	}
	repo, ok := s.repo.(CyberUserRiskRepository)
	if !ok {
		return false
	}
	requestID := log.RequestID
	if requestID == "" {
		requestID = uuid.NewString()
	}
	state, err := repo.RecordCyberUserStrike(ctx, *log.UserID, requestID)
	if err != nil {
		slog.Error("cyber.downstream_user_escalation_failed", "user_id", *log.UserID, "error", err)
		return false
	}
	log.ViolationCount = state.Strikes
	log.AutoBanned = state.Strikes >= 2
	if state.BlockedUntil != nil {
		log.Error += fmt.Sprintf("\ndownstream_user_api_blocked_until=%s", state.BlockedUntil.UTC().Format(time.RFC3339))
	}
	if state.Applied && s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, *log.UserID)
	}
	return state.Applied && log.AutoBanned
}

func (s *ContentModerationService) IsCyberUserEscalationEnabled(ctx context.Context) bool {
	if s == nil {
		return false
	}
	snapshot, err := s.loadRuntimeSnapshot(ctx)
	return err == nil && snapshot != nil && snapshot.riskControlEnabled && snapshot.config.CyberUserBlockEnabled
}
