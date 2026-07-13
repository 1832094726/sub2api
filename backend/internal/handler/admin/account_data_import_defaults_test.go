package admin

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestApplyDataImportDefaults(t *testing.T) {
	priority := 10
	passthrough := true
	item := DataAccount{
		Platform: service.PlatformOpenAI,
		Priority: 1,
		Extra:    map[string]any{},
	}

	applyDataImportDefaults(&item, DataImportRequest{
		DefaultPriority:          &priority,
		OpenAIPassthroughEnabled: &passthrough,
	})

	require.Equal(t, 10, item.Priority)
	require.Equal(t, true, item.Extra["openai_passthrough"])
}

func TestApplyDataImportDefaultsDoesNotApplyOpenAISettingToOtherPlatforms(t *testing.T) {
	passthrough := true
	item := DataAccount{Platform: service.PlatformAnthropic}

	applyDataImportDefaults(&item, DataImportRequest{OpenAIPassthroughEnabled: &passthrough})

	require.NotContains(t, item.Extra, "openai_passthrough")
}

func TestResolveDataImportGroupIDsBindsMatchingActiveGroup(t *testing.T) {
	groupID := int64(7)
	item := DataAccount{Platform: service.PlatformOpenAI}
	group := &service.Group{
		ID:       groupID,
		Platform: service.PlatformOpenAI,
		Status:   service.StatusActive,
	}

	require.Equal(t, []int64{groupID}, resolveDataImportGroupIDs(item, &groupID, group))
}

func TestResolveDataImportGroupIDsSkipsMismatchedOrInactiveGroup(t *testing.T) {
	groupID := int64(7)
	item := DataAccount{Platform: service.PlatformOpenAI}

	require.Nil(t, resolveDataImportGroupIDs(item, &groupID, &service.Group{
		ID:       groupID,
		Platform: service.PlatformAnthropic,
		Status:   service.StatusActive,
	}))
	require.Nil(t, resolveDataImportGroupIDs(item, &groupID, &service.Group{
		ID:       groupID,
		Platform: service.PlatformOpenAI,
		Status:   service.StatusDisabled,
	}))
}
