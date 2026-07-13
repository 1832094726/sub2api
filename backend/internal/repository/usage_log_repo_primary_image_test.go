package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageLogRepositoryCreatePrimaryImageUsesNullAccount(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := NewUsageLogRepository(nil, db).(service.PrimaryImageUsageLogRepository)
	channel := "chatgpt2api_primary"
	taskID := "imgp_1"
	mode := "image"
	log := &service.UsageLog{
		UserID: 7, APIKeyID: 9, AccountID: 0, RequestID: taskID,
		Model: "gpt-image-2", RequestedModel: "gpt-image-2", BillingMode: &mode,
		ImageCount: 1, ImageChannel: &channel, PrimaryTaskID: &taskID,
	}

	mock.ExpectQuery("INSERT INTO usage_logs").
		WithArgs(
			log.UserID, log.APIKeyID, nil, log.RequestID, log.Model, log.RequestedModel,
			sqlmock.AnyArg(), log.BillingType, sqlmock.AnyArg(), log.ImageCount, sqlmock.AnyArg(),
			log.TotalCost, log.ActualCost, log.RateMultiplier, sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))

	inserted, err := repo.CreatePrimaryImage(context.Background(), log)
	require.NoError(t, err)
	require.True(t, inserted)
	require.Equal(t, int64(1), log.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}
