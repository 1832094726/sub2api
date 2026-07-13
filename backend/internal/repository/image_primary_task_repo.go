package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type imagePrimaryTaskRepository struct {
	db *sql.DB
}

func NewImagePrimaryTaskRepository(db *sql.DB) service.ImagePrimaryTaskRepository {
	return &imagePrimaryTaskRepository{db: db}
}

func imagePrimaryTaskSelectColumns() []string {
	return []string{
		"id", "public_id", "user_id", "api_key_id", "usage_log_id", "protocol", "model", "request_hash",
		"upstream_task_id", "status", "fallback_reason", "result_locator", "image_count", "image_size",
		"primary_duration_ms", "fallback_duration_ms", "settlement_state", "created_at", "updated_at", "expires_at", "created",
	}
}

func (r *imagePrimaryTaskRepository) CreateOrGet(ctx context.Context, params service.ImagePrimaryTaskCreate) (*service.ImagePrimaryTask, bool, error) {
	columns := strings.Join(imagePrimaryTaskSelectColumns()[:20], ", ")
	query := `WITH inserted AS (
		INSERT INTO image_primary_tasks (
			public_id, user_id, api_key_id, protocol, model, request_hash, status, settlement_state, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, 'queued', 'pending', $7)
		ON CONFLICT (public_id) DO NOTHING
		RETURNING ` + columns + `, TRUE AS created
	)
	SELECT * FROM inserted
	UNION ALL
	SELECT ` + columns + `, FALSE AS created
	FROM image_primary_tasks
	WHERE public_id = $1 AND user_id = $2 AND api_key_id = $3
		AND request_hash = $6 AND NOT EXISTS (SELECT 1 FROM inserted)
	LIMIT 1`
	row := r.db.QueryRowContext(ctx, query,
		params.PublicID, params.UserID, params.APIKeyID, params.Protocol,
		params.Model, params.RequestHash, params.ExpiresAt,
	)
	return scanImagePrimaryTask(row)
}

func (r *imagePrimaryTaskRepository) GetByPublicID(ctx context.Context, userID, apiKeyID int64, publicID string) (*service.ImagePrimaryTask, error) {
	columns := strings.Join(imagePrimaryTaskSelectColumns()[:20], ", ")
	row := r.db.QueryRowContext(ctx, `SELECT `+columns+`, FALSE AS created
		FROM image_primary_tasks WHERE public_id = $1 AND user_id = $2 AND api_key_id = $3`, publicID, userID, apiKeyID)
	task, _, err := scanImagePrimaryTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return task, err
}

func (r *imagePrimaryTaskRepository) BindUpstreamTask(ctx context.Context, id int64, upstreamTaskID string) (bool, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE image_primary_tasks
		SET upstream_task_id = $2, updated_at = NOW()
		WHERE id = $1 AND upstream_task_id IS NULL`, id, upstreamTaskID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

func (r *imagePrimaryTaskRepository) Transition(
	ctx context.Context,
	id int64,
	fromStatus, toStatus string,
	update service.ImagePrimaryTaskTransition,
) (bool, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE image_primary_tasks SET
		status = $3,
		upstream_task_id = COALESCE($4, upstream_task_id),
		fallback_reason = COALESCE($5, fallback_reason),
		result_locator = COALESCE($6, result_locator),
		image_count = CASE WHEN $7 > 0 THEN $7 ELSE image_count END,
		image_size = COALESCE($8, image_size),
		primary_duration_ms = CASE WHEN $9 > 0 THEN $9 ELSE primary_duration_ms END,
		fallback_duration_ms = CASE WHEN $10 > 0 THEN $10 ELSE fallback_duration_ms END,
		updated_at = NOW()
		WHERE id = $1 AND status = $2`,
		id, fromStatus, toStatus, update.UpstreamTaskID, update.FallbackReason,
		update.ResultLocator, update.ImageCount, update.ImageSize,
		update.PrimaryDurationMS, update.FallbackDurationMS,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

func (r *imagePrimaryTaskRepository) ClaimSettlement(ctx context.Context, id int64) (bool, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE image_primary_tasks
		SET settlement_state = $2, updated_at = NOW()
		WHERE id = $1 AND settlement_state = 'pending'`, id, service.ImagePrimarySettlementClaimed)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

func (r *imagePrimaryTaskRepository) CompleteSettlement(ctx context.Context, id int64) (bool, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE image_primary_tasks
		SET settlement_state = 'settled', updated_at = NOW()
		WHERE id = $1 AND settlement_state IN ('pending', 'claimed')`, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

type imagePrimaryTaskScanner interface {
	Scan(...any) error
}

func scanImagePrimaryTask(scanner imagePrimaryTaskScanner) (*service.ImagePrimaryTask, bool, error) {
	task := &service.ImagePrimaryTask{}
	var usageLogID sql.NullInt64
	var upstreamTaskID, fallbackReason, resultLocator, imageSize sql.NullString
	var created bool
	err := scanner.Scan(
		&task.ID, &task.PublicID, &task.UserID, &task.APIKeyID, &usageLogID,
		&task.Protocol, &task.Model, &task.RequestHash, &upstreamTaskID, &task.Status,
		&fallbackReason, &resultLocator, &task.ImageCount, &imageSize,
		&task.PrimaryDurationMS, &task.FallbackDurationMS, &task.SettlementState,
		&task.CreatedAt, &task.UpdatedAt, &task.ExpiresAt, &created,
	)
	if err != nil {
		return nil, false, err
	}
	if usageLogID.Valid {
		task.UsageLogID = &usageLogID.Int64
	}
	if upstreamTaskID.Valid {
		task.UpstreamTaskID = &upstreamTaskID.String
	}
	if fallbackReason.Valid {
		task.FallbackReason = &fallbackReason.String
	}
	if resultLocator.Valid {
		task.ResultLocator = &resultLocator.String
	}
	if imageSize.Valid {
		task.ImageSize = &imageSize.String
	}
	return task, created, nil
}

var _ service.ImagePrimaryTaskRepository = (*imagePrimaryTaskRepository)(nil)
