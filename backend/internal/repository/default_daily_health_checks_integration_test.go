//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefaultDailyHealthCheckPlanLifecycle(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()

	insertAccount := func(name, status string) int64 {
		var id int64
		err := tx.QueryRowContext(ctx, `
			INSERT INTO accounts (name, platform, type, credentials, extra, status, created_at, updated_at)
			VALUES ($1, 'openai', 'oauth', '{}', '{}', $2, NOW(), NOW())
			RETURNING id
		`, name, status).Scan(&id)
		require.NoError(t, err)
		return id
	}

	activeID := insertAccount("daily-health-active", "active")
	inactiveID := insertAccount("daily-health-inactive", "inactive")

	assertPlan := func(accountID int64, enabled bool) {
		var cronExpression string
		var planEnabled bool
		var autoRecover bool
		var nextRunAt time.Time
		err := tx.QueryRowContext(ctx, `
			SELECT cron_expression, enabled, auto_recover, next_run_at
			FROM scheduled_test_plans
			WHERE account_id = $1 AND system_generated = TRUE
		`, accountID).Scan(&cronExpression, &planEnabled, &autoRecover, &nextRunAt)
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("%d %d * * *", accountID%60, (accountID/60)%24), cronExpression)
		require.Equal(t, enabled, planEnabled)
		require.True(t, autoRecover)
		require.True(t, nextRunAt.After(time.Now().Add(-time.Minute)))
		require.True(t, nextRunAt.Before(time.Now().Add(25*time.Hour)))
	}

	assertPlan(activeID, true)
	assertPlan(inactiveID, false)

	_, err := tx.ExecContext(ctx, `UPDATE accounts SET status = 'active' WHERE id = $1`, inactiveID)
	require.NoError(t, err)
	assertPlan(inactiveID, true)

	var count int
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM scheduled_test_plans
		WHERE account_id = $1 AND system_generated = TRUE
	`, activeID).Scan(&count))
	require.Equal(t, 1, count)
}
