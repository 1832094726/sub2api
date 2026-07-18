package admin

import (
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestValidateOpenAIRefreshPrerequisitesRequiresRefreshToken(t *testing.T) {
	account := &service.Account{
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "existing-access-token",
		},
	}

	err := validateOpenAIRefreshPrerequisites(account)
	require.Error(t, err)
	require.Equal(t, "OPENAI_OAUTH_NO_REFRESH_TOKEN", infraerrors.Reason(err))
}

func TestValidateOpenAIRefreshPrerequisitesAllowsRefreshToken(t *testing.T) {
	account := &service.Account{
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "existing-access-token",
			"refresh_token": "refresh-token",
		},
	}

	require.NoError(t, validateOpenAIRefreshPrerequisites(account))
}

func TestClassifyAccessTokenRefreshRejectsUnchangedToken(t *testing.T) {
	err := validateAccessTokenChanged("same-token", "same-token")
	require.Error(t, err)
	require.Equal(t, "OPENAI_OAUTH_TOKEN_UNCHANGED", infraerrors.Reason(err))
}

func TestClassifyAccessTokenRefreshAcceptsChangedToken(t *testing.T) {
	require.NoError(t, validateAccessTokenChanged("old-token", "new-token"))
}

func TestBatchRefreshResultCodePreservesActionableReasons(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "missing refresh token",
			err:  infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_NO_REFRESH_TOKEN", "missing"),
			want: "missing_refresh_token",
		},
		{
			name: "unchanged token",
			err:  infraerrors.New(http.StatusConflict, "OPENAI_OAUTH_TOKEN_UNCHANGED", "unchanged"),
			want: "token_unchanged",
		},
		{
			name: "verification failed",
			err:  infraerrors.New(http.StatusUnauthorized, "OPENAI_OAUTH_REFRESH_VERIFICATION_FAILED", "401"),
			want: "refreshed_but_unverified",
		},
		{
			name: "repository reports invalid refresh token",
			err:  infraerrors.New(http.StatusUnauthorized, "OPENAI_OAUTH_REFRESH_TOKEN_INVALID", "invalid"),
			want: "refresh_token_invalidated",
		},
		{
			name: "microsoft refresh token is unsupported",
			err:  infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_MICROSOFT_TOKEN_UNSUPPORTED", "unsupported"),
			want: "unsupported_refresh_token",
		},
		{
			name: "proxy transport failed",
			err:  infraerrors.New(http.StatusBadGateway, "OPENAI_OAUTH_REQUEST_FAILED", "connection reset"),
			want: "transport_failed",
		},
		{
			name: "empty access token",
			err:  infraerrors.New(http.StatusBadGateway, "OPENAI_OAUTH_EMPTY_ACCESS_TOKEN", "empty"),
			want: "empty_access_token",
		},
		{
			name: "unknown refresh failure",
			err:  infraerrors.New(http.StatusBadGateway, "OPENAI_OAUTH_UNKNOWN", "unknown"),
			want: "refresh_failed",
		},
		{
			name: "refresh token invalidated",
			err: infraerrors.New(
				http.StatusBadGateway,
				"OPENAI_OAUTH_TOKEN_REFRESH_FAILED",
				`token refresh failed: {"code":"refresh_token_invalidated"}`,
			),
			want: "refresh_token_invalidated",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, batchRefreshResultCode(tt.err))
		})
	}
}

func TestIsInvalidOpenAIWithoutRefreshToken(t *testing.T) {
	invalid := &service.Account{
		Platform:     service.PlatformOpenAI,
		Type:         service.AccountTypeOAuth,
		Status:       service.StatusError,
		ErrorMessage: `Authentication failed (401): {"error":{"code":"token_invalidated"}}`,
		Credentials:  map[string]any{"access_token": "expired"},
	}
	require.True(t, isInvalidOpenAIWithoutRefreshToken(invalid))

	withRefreshToken := *invalid
	withRefreshToken.Credentials = map[string]any{"access_token": "expired", "refresh_token": "refresh"}
	require.False(t, isInvalidOpenAIWithoutRefreshToken(&withRefreshToken))

	networkError := *invalid
	networkError.ErrorMessage = "connection reset by peer"
	require.False(t, isInvalidOpenAIWithoutRefreshToken(&networkError))
}
