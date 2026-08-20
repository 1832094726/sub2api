//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func openAIRealtimeTestService() *OpenAIGatewayService {
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	return &OpenAIGatewayService{cfg: cfg}
}

func TestAccountSupportsOpenAIRealtimeOnlyForAPIKey(t *testing.T) {
	require.True(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}).SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityRealtime))
	require.False(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}).SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityRealtime))
	require.False(t, (&Account{Platform: PlatformGrok, Type: AccountTypeAPIKey}).SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityRealtime))
}

func TestBuildOpenAIRealtimeURL(t *testing.T) {
	svc := openAIRealtimeTestService()
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://api.openai.com",
		},
	}

	got, err := svc.buildOpenAIRealtimeURL(account, "gpt-realtime-2.1-mini")
	require.NoError(t, err)
	require.Equal(t, "wss://api.openai.com/v1/realtime?model=gpt-realtime-2.1-mini", got)
}

func TestBuildOpenAIRealtimeURLUsesVersionedCompatibleBase(t *testing.T) {
	svc := openAIRealtimeTestService()
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://gateway.example/v1",
		},
	}

	got, err := svc.buildOpenAIRealtimeURL(account, "gpt-realtime")
	require.NoError(t, err)
	require.Equal(t, "wss://gateway.example/v1/realtime?model=gpt-realtime", got)
}

func TestBuildOpenAIRealtimeURLRejectsOAuth(t *testing.T) {
	svc := openAIRealtimeTestService()
	_, err := svc.buildOpenAIRealtimeURL(&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, "gpt-realtime")
	require.Error(t, err)
}
