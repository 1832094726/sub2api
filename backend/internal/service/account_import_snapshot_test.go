package service

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

type snapshotMemoryRepo struct {
	byAccount map[int64]*AccountImportSnapshot
}

func (r *snapshotMemoryRepo) Upsert(_ context.Context, snapshot AccountImportSnapshot) error {
	copy := snapshot
	r.byAccount[snapshot.AccountID] = &copy
	return nil
}

func (r *snapshotMemoryRepo) GetByAccountID(_ context.Context, accountID int64) (*AccountImportSnapshot, error) {
	snapshot, ok := r.byAccount[accountID]
	if !ok {
		return nil, nil
	}
	copy := *snapshot
	return &copy, nil
}

type snapshotTestEncryptor struct{}

func (snapshotTestEncryptor) Encrypt(plaintext string) (string, error) {
	return base64.StdEncoding.EncodeToString([]byte(plaintext)), nil
}

func (snapshotTestEncryptor) Decrypt(ciphertext string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(ciphertext)
	return string(decoded), err
}

func TestMaskImportJSONRecursivelyMasksSecrets(t *testing.T) {
	in := map[string]any{
		"email": "visible@example.test",
		"credentials": map[string]any{
			"Refresh_Token": "abcdefghijk",
			"nested": []any{
				map[string]any{"PASSWORD": "password-value"},
			},
		},
	}

	got := MaskImportJSON(in).(map[string]any)
	require.Equal(t, "visible@example.test", got["email"])
	credentials := got["credentials"].(map[string]any)
	require.Equal(t, "abc***jk", credentials["Refresh_Token"])
	nested := credentials["nested"].([]any)
	require.Equal(t, "pas***ue", nested[0].(map[string]any)["PASSWORD"])
}

func TestAccountImportSnapshotServiceEncryptsAndRevealsLatestSnapshot(t *testing.T) {
	repo := &snapshotMemoryRepo{byAccount: map[int64]*AccountImportSnapshot{}}
	svc := NewAccountImportSnapshotService(repo, snapshotTestEncryptor{})
	ctx := context.Background()

	require.NoError(t, svc.Save(ctx, 88, "batch-one", map[string]any{
		"name":        "first",
		"credentials": map[string]any{"access_token": "first-secret"},
	}))
	firstEncrypted := repo.byAccount[88].EncryptedJSON
	require.NotContains(t, firstEncrypted, "first-secret")

	require.NoError(t, svc.Save(ctx, 88, "batch-two", map[string]any{
		"name":        "second",
		"credentials": map[string]any{"access_token": "second-secret"},
	}))

	view, err := svc.GetMasked(ctx, 88)
	require.NoError(t, err)
	require.True(t, view.Exists)
	require.Equal(t, "batch-two", view.BatchID)
	require.Equal(t, "sec***et", view.JSON.(map[string]any)["credentials"].(map[string]any)["access_token"])

	revealed, err := svc.Reveal(ctx, 88)
	require.NoError(t, err)
	require.Equal(t, "second-secret", revealed.JSON.(map[string]any)["credentials"].(map[string]any)["access_token"])
	require.NotEqual(t, firstEncrypted, repo.byAccount[88].EncryptedJSON)
}

func TestAccountImportSnapshotServiceReturnsAbsentForManualAccount(t *testing.T) {
	svc := NewAccountImportSnapshotService(
		&snapshotMemoryRepo{byAccount: map[int64]*AccountImportSnapshot{}},
		snapshotTestEncryptor{},
	)

	view, err := svc.GetMasked(context.Background(), 99)
	require.NoError(t, err)
	require.False(t, view.Exists)
	require.Nil(t, view.JSON)
}

func TestAccountImportSnapshotServiceRejectsInvalidSource(t *testing.T) {
	repo := &snapshotMemoryRepo{byAccount: map[int64]*AccountImportSnapshot{}}
	svc := NewAccountImportSnapshotService(repo, snapshotTestEncryptor{})

	err := svc.Save(context.Background(), 0, "batch", map[string]any{"name": "invalid"})
	require.Error(t, err)
	err = svc.Save(context.Background(), 1, "", func() {})
	require.Error(t, err)
}
