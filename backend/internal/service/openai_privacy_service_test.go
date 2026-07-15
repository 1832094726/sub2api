//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestForceOpenAIPrivacySendsCodexAccountHeaders(t *testing.T) {
	t.Parallel()

	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	repo := &privacyAccountRepoStub{}
	svc := &adminServiceImpl{
		accountRepo:          repo,
		privacyClientFactory: newQuotaRedirectingFactory(srv),
	}
	account := &Account{
		ID:       88,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "access-token",
			"chatgpt_account_id": "preferred-account",
			"organization_id":    "legacy-account",
		},
	}

	mode, err := svc.ForceOpenAIPrivacy(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, PrivacyModeTrainingOff, mode)
	require.Equal(t, "preferred-account", got.Get("chatgpt-account-id"))
	require.Equal(t, openaiQuotaCodexBeta, got.Get("openai-beta"))
	require.Equal(t, openaiQuotaCodexLanguageTag, got.Get("oai-language"))
	require.Equal(t, openaiQuotaCodexOriginator, got.Get("originator"))
}

func TestForceOpenAIPrivacyFallsBackToOrganizationID(t *testing.T) {
	t.Parallel()

	var accountID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accountID = r.Header.Get("chatgpt-account-id")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	svc := &adminServiceImpl{
		accountRepo:          &privacyAccountRepoStub{},
		privacyClientFactory: newQuotaRedirectingFactory(srv),
	}
	account := &Account{
		ID:       89,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":    "access-token",
			"organization_id": "legacy-account",
		},
	}

	_, err := svc.ForceOpenAIPrivacy(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, "legacy-account", accountID)
}

func TestForceOpenAIPrivacyResolvesShadowToParentCredentials(t *testing.T) {
	t.Parallel()

	var accountID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accountID = r.Header.Get("chatgpt-account-id")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	parentID := int64(88)
	parent := &Account{
		ID:       parentID,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "parent-access-token",
			"chatgpt_account_id": "parent-account-id",
		},
	}
	repo := &privacyAccountRepoStub{account: parent}
	svc := &adminServiceImpl{
		accountRepo:          repo,
		privacyClientFactory: newQuotaRedirectingFactory(srv),
	}
	shadow := &Account{
		ID:              89,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
	}

	mode, err := svc.ForceOpenAIPrivacy(context.Background(), shadow)

	require.NoError(t, err)
	require.Equal(t, PrivacyModeTrainingOff, mode)
	require.Equal(t, "parent-account-id", accountID)
	require.Equal(t, parentID, repo.updatedAccountID)
}

func TestForceOpenAIPrivacyRejectsMissingAccountID(t *testing.T) {
	t.Parallel()

	svc := &adminServiceImpl{
		accountRepo: &privacyAccountRepoStub{},
		privacyClientFactory: func(string) (*req.Client, error) {
			t.Fatal("missing account id must fail before creating an upstream client")
			return nil, nil
		},
	}
	account := &Account{
		ID:          90,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "access-token"},
	}

	_, err := svc.ForceOpenAIPrivacy(context.Background(), account)

	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "OPENAI_PRIVACY_MISSING_ACCOUNT_ID", infraerrors.Reason(err))
}

func TestForceOpenAIPrivacyClassifiesUpstreamFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantReason string
		wantMode   string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":"expired"}`, wantStatus: http.StatusUnauthorized, wantReason: "OPENAI_PRIVACY_UNAUTHORIZED", wantMode: PrivacyModeFailed},
		{name: "cloudflare challenge", status: http.StatusForbidden, body: `<html><title>Just a moment...</title><script>window._cf_chl_opt={}</script></html>`, wantStatus: http.StatusBadGateway, wantReason: "OPENAI_PRIVACY_CF_BLOCKED", wantMode: PrivacyModeCFBlocked},
		{name: "ordinary forbidden", status: http.StatusForbidden, body: `{"error":"forbidden"}`, wantStatus: http.StatusForbidden, wantReason: "OPENAI_PRIVACY_FORBIDDEN", wantMode: PrivacyModeFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			svc := &adminServiceImpl{
				accountRepo:          &privacyAccountRepoStub{},
				privacyClientFactory: newQuotaRedirectingFactory(srv),
			}
			account := &Account{
				ID:       91,
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token":       "access-token",
					"chatgpt_account_id": "account-id",
				},
			}

			mode, err := svc.ForceOpenAIPrivacy(context.Background(), account)

			require.Error(t, err)
			require.Equal(t, tt.wantStatus, infraerrors.Code(err))
			require.Equal(t, tt.wantReason, infraerrors.Reason(err))
			require.Equal(t, tt.wantMode, mode)
		})
	}
}

func TestForceOpenAIPrivacyMapsCanceledRequestToBadGateway(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc := &adminServiceImpl{
		accountRepo:          &privacyAccountRepoStub{},
		privacyClientFactory: newQuotaRedirectingFactory(srv),
	}
	account := &Account{
		ID:       92,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "access-token",
			"chatgpt_account_id": "account-id",
		},
	}

	_, err := svc.ForceOpenAIPrivacy(ctx, account)

	require.Error(t, err)
	require.Equal(t, http.StatusBadGateway, infraerrors.Code(err))
	require.Equal(t, "OPENAI_PRIVACY_REQUEST_FAILED", infraerrors.Reason(err))
}

type privacyAccountRepoStub struct {
	AccountRepository
	account          *Account
	updatedAccountID int64
	updates          map[string]any
}

func (r *privacyAccountRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.account == nil || r.account.ID != id {
		return nil, ErrAccountNotFound
	}
	return r.account, nil
}

func (r *privacyAccountRepoStub) UpdateExtra(_ context.Context, accountID int64, updates map[string]any) error {
	r.updatedAccountID = accountID
	r.updates = updates
	return nil
}
