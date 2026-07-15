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

	require.Equal(t, []int64{groupID}, resolveValidatedDataImportGroupIDs(
		item,
		[]int64{groupID},
		map[int64]*service.Group{groupID: group},
	))
}

func TestResolveDataImportGroupIDsSkipsMismatchedOrInactiveGroup(t *testing.T) {
	groupID := int64(7)
	item := DataAccount{Platform: service.PlatformOpenAI}

	require.Nil(t, resolveValidatedDataImportGroupIDs(item, []int64{groupID}, map[int64]*service.Group{
		groupID: {ID: groupID, Platform: service.PlatformAnthropic, Status: service.StatusActive},
	}))
	require.Nil(t, resolveValidatedDataImportGroupIDs(item, []int64{groupID}, map[int64]*service.Group{
		groupID: {ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusDisabled},
	}))
}

func TestResolveImportGroupIDsNewArrayWinsAndDeduplicates(t *testing.T) {
	legacy := int64(9)
	require.Equal(t, []int64{2, 3}, resolveImportGroupIDs([]int64{2, 3, 2}, &legacy))
}

func TestResolveImportGroupIDsFallsBackToLegacyField(t *testing.T) {
	legacy := int64(9)
	require.Equal(t, []int64{9}, resolveImportGroupIDs(nil, &legacy))
	require.Nil(t, resolveImportGroupIDs(nil, nil))
}

func TestValidateDataImportGroupCompatibilityRejectsAnyMismatchedAccount(t *testing.T) {
	groups := map[int64]*service.Group{
		2: {ID: 2, Platform: service.PlatformOpenAI, Status: service.StatusActive},
	}
	accounts := []DataAccount{
		{Name: "openai", Platform: service.PlatformOpenAI},
		{Name: "anthropic", Platform: service.PlatformAnthropic},
	}

	err := validateDataImportGroupCompatibility(accounts, []int64{2}, groups)
	require.ErrorContains(t, err, "platform")
}

func TestResolveDataImportGroupIDsReturnsAllValidatedGroups(t *testing.T) {
	item := DataAccount{Platform: service.PlatformOpenAI}
	groups := map[int64]*service.Group{
		2: {ID: 2, Platform: service.PlatformOpenAI, Status: service.StatusActive},
		3: {ID: 3, Platform: service.PlatformOpenAI, Status: service.StatusActive},
	}

	require.Equal(t, []int64{2, 3}, resolveValidatedDataImportGroupIDs(item, []int64{2, 3}, groups))
}
