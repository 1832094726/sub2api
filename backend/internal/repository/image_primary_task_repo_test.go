package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestImagePrimaryTaskRepositoryCreateOrGet(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := NewImagePrimaryTaskRepository(db)
	now := time.Now()

	mock.ExpectQuery("WITH inserted AS").
		WithArgs("imgp_1", int64(7), int64(9), "images", "gpt-image-2", "hash-1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(imagePrimaryTaskSelectColumns()).AddRow(
			int64(1), "imgp_1", int64(7), int64(9), nil, "images", "gpt-image-2", "hash-1",
			nil, "queued", nil, nil, 0, nil, 0, 0, "pending", now, now, now.Add(time.Hour), true,
		))

	task, created, err := repo.CreateOrGet(context.Background(), service.ImagePrimaryTaskCreate{
		PublicID: "imgp_1", UserID: 7, APIKeyID: 9, Protocol: "images",
		Model: "gpt-image-2", RequestHash: "hash-1", ExpiresAt: now.Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, int64(1), task.ID)
	require.Equal(t, "imgp_1", task.PublicID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestImagePrimaryTaskRepositoryClaimSettlementOnlyOnce(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := NewImagePrimaryTaskRepository(db)

	mock.ExpectExec("UPDATE image_primary_tasks").
		WithArgs(int64(11), service.ImagePrimarySettlementClaimed).
		WillReturnResult(sqlmock.NewResult(0, 1))
	claimed, err := repo.ClaimSettlement(context.Background(), 11)
	require.NoError(t, err)
	require.True(t, claimed)

	mock.ExpectExec("UPDATE image_primary_tasks").
		WithArgs(int64(11), service.ImagePrimarySettlementClaimed).
		WillReturnResult(sqlmock.NewResult(0, 0))
	claimed, err = repo.ClaimSettlement(context.Background(), 11)
	require.NoError(t, err)
	require.False(t, claimed)
	require.NoError(t, mock.ExpectationsWereMet())
}
