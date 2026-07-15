package repository

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestAccountImportSnapshotRepositoryUpsertsLatestByAccount(t *testing.T) {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:snapshot-%d?mode=memory&cache=shared&_fk=1&_pragma=foreign_keys(1)", time.Now().UnixNano()))
	require.NoError(t, err)
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.SQLite, db)))
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	require.NoError(t, client.Schema.Create(context.Background()))

	repo := NewAccountImportSnapshotRepository(client)
	firstTime := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	require.NoError(t, repo.Upsert(context.Background(), service.AccountImportSnapshot{
		AccountID: 88, BatchID: "batch-one", EncryptedJSON: "cipher-one",
		ImportedAt: firstTime, UpdatedAt: firstTime,
	}))

	secondTime := firstTime.Add(time.Hour)
	require.NoError(t, repo.Upsert(context.Background(), service.AccountImportSnapshot{
		AccountID: 88, BatchID: "batch-two", EncryptedJSON: "cipher-two",
		ImportedAt: secondTime, UpdatedAt: secondTime,
	}))

	got, err := repo.GetByAccountID(context.Background(), 88)
	require.NoError(t, err)
	require.Equal(t, "batch-two", got.BatchID)
	require.Equal(t, "cipher-two", got.EncryptedJSON)
	require.True(t, got.ImportedAt.Equal(secondTime))

	missing, err := repo.GetByAccountID(context.Background(), 99)
	require.NoError(t, err)
	require.Nil(t, missing)
}
