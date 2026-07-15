package repository

import (
	"context"

	"entgo.io/ent/dialect/sql"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/accountimportsnapshot"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type accountImportSnapshotRepository struct {
	client *ent.Client
}

func NewAccountImportSnapshotRepository(client *ent.Client) service.AccountImportSnapshotRepository {
	return &accountImportSnapshotRepository{client: client}
}

func (r *accountImportSnapshotRepository) Upsert(ctx context.Context, snapshot service.AccountImportSnapshot) error {
	return r.client.AccountImportSnapshot.Create().
		SetAccountID(snapshot.AccountID).
		SetBatchID(snapshot.BatchID).
		SetEncryptedJSON(snapshot.EncryptedJSON).
		SetImportedAt(snapshot.ImportedAt).
		SetUpdatedAt(snapshot.UpdatedAt).
		OnConflict(
			sql.ConflictColumns(accountimportsnapshot.FieldAccountID),
			sql.ResolveWithNewValues(),
		).
		Exec(ctx)
}

func (r *accountImportSnapshotRepository) GetByAccountID(ctx context.Context, accountID int64) (*service.AccountImportSnapshot, error) {
	row, err := r.client.AccountImportSnapshot.Query().
		Where(accountimportsnapshot.AccountIDEQ(accountID)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &service.AccountImportSnapshot{
		AccountID: row.AccountID, BatchID: row.BatchID, EncryptedJSON: row.EncryptedJSON,
		ImportedAt: row.ImportedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

var _ service.AccountImportSnapshotRepository = (*accountImportSnapshotRepository)(nil)
