package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type handlerSnapshotRepo struct {
	snapshot *service.AccountImportSnapshot
}

func (r *handlerSnapshotRepo) Upsert(_ context.Context, snapshot service.AccountImportSnapshot) error {
	r.snapshot = &snapshot
	return nil
}

func (r *handlerSnapshotRepo) GetByAccountID(_ context.Context, accountID int64) (*service.AccountImportSnapshot, error) {
	if r.snapshot == nil || r.snapshot.AccountID != accountID {
		return nil, nil
	}
	copy := *r.snapshot
	return &copy, nil
}

type handlerSnapshotEncryptor struct{}

func (handlerSnapshotEncryptor) Encrypt(value string) (string, error) {
	return base64.StdEncoding.EncodeToString([]byte(value)), nil
}

func (handlerSnapshotEncryptor) Decrypt(value string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	return string(decoded), err
}

func newAccountImportSnapshotTestHandler(t *testing.T) *AccountHandler {
	t.Helper()
	repo := &handlerSnapshotRepo{}
	snapshotService := service.NewAccountImportSnapshotService(repo, handlerSnapshotEncryptor{})
	require.NoError(t, snapshotService.Save(context.Background(), 88, "batch-88", map[string]any{
		"email":       "visible@example.test",
		"credentials": map[string]any{"refresh_token": "super-secret-token"},
	}))
	return &AccountHandler{accountImportSnapshotService: snapshotService}
}

func TestAccountHandlerGetImportSnapshotReturnsMaskedJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := newAccountImportSnapshotTestHandler(t)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/88/import-snapshot", nil)
	ctx.Params = gin.Params{{Key: "id", Value: "88"}}

	handler.GetImportSnapshot(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "super-secret-token")
	require.Contains(t, recorder.Body.String(), "sup***en")
}

func TestAccountHandlerRevealImportSnapshotDisablesCaching(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := newAccountImportSnapshotTestHandler(t)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/88/import-snapshot/reveal", nil)
	ctx.Params = gin.Params{{Key: "id", Value: "88"}}

	handler.RevealImportSnapshot(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	require.Contains(t, recorder.Body.String(), "super-secret-token")
	var payload map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.NotNil(t, payload["data"])
}
