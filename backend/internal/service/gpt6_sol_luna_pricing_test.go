package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGPT6SolLunaFallbackPricesAndLongContext(t *testing.T) {
	svc := NewBillingService(&config.Config{Gateway: config.GatewayConfig{UniversalLongContextBilling: true}}, nil)
	for _, tc := range []struct {
		model string
		input, cached, cacheWrite, output float64
	}{
		{"gpt-6-sol", 2e-6, 0.2e-6, 2.5e-6, 10e-6},
		{"gpt-6-luna", 0.1e-6, 0.01e-6, 0.125e-6, 0.5e-6},
	} {
		pricing, err := svc.GetModelPricing(tc.model)
		require.NoError(t, err)
		require.InDelta(t, tc.input, pricing.InputPricePerToken, 1e-12)
		require.InDelta(t, tc.cached, pricing.CacheReadPricePerToken, 1e-12)
		require.InDelta(t, tc.cacheWrite, pricing.CacheCreationPricePerToken, 1e-12)
		require.InDelta(t, tc.output, pricing.OutputPricePerToken, 1e-12)
		require.Equal(t, tc.model, normalizeKnownOpenAICodexModel(tc.model))
		base := svc.computeTokenBreakdown(pricing, UsageTokens{InputTokens: 272000, OutputTokens: 1}, 1, "", true)
		long := svc.computeTokenBreakdown(pricing, UsageTokens{InputTokens: 272001, OutputTokens: 1}, 1, "", true)
		require.False(t, base.LongContextBillingApplied)
		require.True(t, long.LongContextBillingApplied)
		require.InDelta(t, (272001*tc.input+tc.output)*1.5, long.TotalCost, 1e-9)
	}
}

func TestGPT6SolLunaManifestReasoningLevels(t *testing.T) {
	for _, tc := range []struct{ model string; ultra bool }{
		{"gpt-6-sol", true}, {"gpt-6-luna", false},
	} {
		levels := configuredCodexGPTReasoningLevels(tc.model)
		hasMax, hasUltra := false, false
		for _, level := range levels {
			hasMax = hasMax || level.Effort == "max"
			hasUltra = hasUltra || level.Effort == "ultra"
		}
		require.True(t, hasMax, tc.model)
		require.Equal(t, tc.ultra, hasUltra, tc.model)
	}
}
