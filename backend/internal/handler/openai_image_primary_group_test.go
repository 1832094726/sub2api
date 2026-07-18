package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestImagePrimaryOnlyEnabledForConfiguredGroups(t *testing.T) {
	groupID := int64(12)
	otherGroupID := int64(34)
	h := &OpenAIGatewayHandler{
		cfg: &config.Config{ChatGPT2APIImage: config.ChatGPT2APIImageConfig{
			PrimaryEnabled: true,
			GroupIDs:       []int64{groupID},
		}},
		imagePrimaryRouter: &fakeImagePrimaryRouting{},
	}

	require.True(t, h.imagePrimaryEnabledForAPIKey(&service.APIKey{GroupID: &groupID}))
	require.False(t, h.imagePrimaryEnabledForAPIKey(&service.APIKey{GroupID: &otherGroupID}))
	require.False(t, h.imagePrimaryEnabledForAPIKey(&service.APIKey{}))
}

func TestImagePrimaryDisabledByDefaultWithoutGroups(t *testing.T) {
	groupID := int64(12)
	h := &OpenAIGatewayHandler{
		cfg:                &config.Config{ChatGPT2APIImage: config.ChatGPT2APIImageConfig{PrimaryEnabled: true}},
		imagePrimaryRouter: &fakeImagePrimaryRouting{},
	}

	require.False(t, h.imagePrimaryEnabledForAPIKey(&service.APIKey{GroupID: &groupID}))
}
