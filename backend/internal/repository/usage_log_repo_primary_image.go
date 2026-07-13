package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *usageLogRepository) CreatePrimaryImage(ctx context.Context, log *service.UsageLog) (bool, error) {
	if log == nil {
		return false, errors.New("primary image usage log is nil")
	}
	createdAt := log.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	requestedModel := strings.TrimSpace(log.RequestedModel)
	if requestedModel == "" {
		requestedModel = strings.TrimSpace(log.Model)
	}
	requestType := int16(log.EffectiveRequestType())
	args := []any{
		log.UserID, log.APIKeyID, nil, strings.TrimSpace(log.RequestID), log.Model, requestedModel,
		createdAt, log.BillingType, requestType, log.ImageCount, nullString(log.ImageSize),
		log.TotalCost, log.ActualCost, log.RateMultiplier, nullString(log.BillingMode),
		nullString(log.ImageChannel), nullString(log.PrimaryTaskID), nullInt(log.PrimaryDurationMS),
		nullString(log.FallbackReason), nullInt(log.FallbackDurationMS),
		nullString(log.InboundEndpoint), nullString(log.UpstreamEndpoint),
	}
	query := `INSERT INTO usage_logs (
		user_id, api_key_id, account_id, request_id, model, requested_model,
		created_at, billing_type, request_type, image_count, image_size,
		total_cost, actual_cost, rate_multiplier, billing_mode,
		image_channel, primary_task_id, primary_duration_ms, fallback_reason, fallback_duration_ms,
		inbound_endpoint, upstream_endpoint
	) VALUES (
		$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
		$12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22
	) ON CONFLICT (request_id, api_key_id) DO NOTHING
	RETURNING id, created_at`
	err := scanSingleRow(ctx, r.sql, query, args, &log.ID, &log.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = scanSingleRow(ctx, r.sql,
			"SELECT id, created_at FROM usage_logs WHERE request_id = $1 AND api_key_id = $2",
			[]any{log.RequestID, log.APIKeyID}, &log.ID, &log.CreatedAt,
		)
		return false, err
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

var _ service.PrimaryImageUsageLogRepository = (*usageLogRepository)(nil)
