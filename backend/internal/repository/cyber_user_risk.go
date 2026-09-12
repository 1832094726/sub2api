package repository

import (
	"context"
	"database/sql"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"time"
)

var _ service.CyberUserRiskRepository = (*contentModerationRepository)(nil)

func (r *contentModerationRepository) RecordCyberUserStrike(ctx context.Context, userID int64, requestID string) (state service.CyberUserRiskState, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return state, err
	}
	defer tx.Rollback()
	var role, status string
	// Cross-process serialization: two independent upstream events count twice;
	// duplicate delivery of the same event counts once.
	err = tx.QueryRowContext(ctx, "SELECT role,status FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", userID).Scan(&role, &status)
	if err != nil {
		return state, err
	}
	if role == service.RoleAdmin {
		return state, tx.Commit()
	}
	result, err := tx.ExecContext(ctx, "INSERT INTO downstream_cyber_events(user_id,request_id) VALUES($1,$2) ON CONFLICT DO NOTHING", userID, requestID)
	if err != nil {
		return state, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return state, err
	}
	var until sql.NullTime
	if n == 0 {
		err = tx.QueryRowContext(ctx, "SELECT strikes,blocked_until FROM downstream_cyber_risk WHERE user_id=$1", userID).Scan(&state.Strikes, &until)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		if err != nil {
			return state, err
		}
	} else {
		err = tx.QueryRowContext(ctx, `INSERT INTO downstream_cyber_risk(user_id,strikes,blocked_until)
 VALUES($1,1,NOW()+INTERVAL '1 hour')
 ON CONFLICT(user_id) DO UPDATE SET strikes=LEAST(downstream_cyber_risk.strikes+1,2),blocked_until=NULL,updated_at=NOW()
 RETURNING strikes,blocked_until`, userID).Scan(&state.Strikes, &until)
		if err != nil {
			return state, err
		}
		if state.Strikes >= 2 {
			if _, err = tx.ExecContext(ctx, "UPDATE users SET status=$2,updated_at=NOW() WHERE id=$1", userID, service.StatusDisabled); err != nil {
				return state, err
			}
		}
		state.Applied = true
	}
	if until.Valid {
		t := until.Time
		state.BlockedUntil = &t
	}
	if err = tx.Commit(); err != nil {
		return service.CyberUserRiskState{}, err
	}
	return state, nil
}

func (r *contentModerationRepository) ResetCyberUserRisk(ctx context.Context, userID int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err = tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", userID).Scan(&id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM downstream_cyber_risk WHERE user_id=$1", userID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE users SET status=$2,updated_at=$3 WHERE id=$1", userID, service.StatusActive, time.Now()); err != nil {
		return err
	}
	// Retain deduplication IDs so delayed delivery cannot punish an unbanned user.
	return tx.Commit()
}
