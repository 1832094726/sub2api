package service

import (
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestUniversalLongContextBillingBoundariesAndStacking(t *testing.T) {
	fast := 2.5
	pricing := &ModelPricing{InputPricePerToken: 10e-6, OutputPricePerToken: 50e-6,
		CacheReadPricePerToken: 2.5e-6, CacheCreationPricePerToken: 12.5e-6,
		ImageInputPricePerToken: 50e-6, ImageOutputPricePerToken: 50e-6,
		LongContextInputThreshold: 200000, LongContextInputMultiplier: 4, LongContextOutputMultiplier: 3,
		FastMultiplier: &fast}
	svc := &BillingService{cfg: &config.Config{Gateway: config.GatewayConfig{UniversalLongContextBilling: true}}}
	for _, contextTokens := range []int{271999, 272000, 272001, 500000} {
		for _, tier := range []string{"", "priority", "fast", "flex"} {
			tokens := UsageTokens{InputTokens: contextTokens - 200, CacheReadTokens: 100, CacheCreationTokens: 100, OutputTokens: 100, ImageInputTokens: 5, ImageOutputTokens: 5}
			standard := svc.computeTokenBreakdownBase(pricing, tokens, 1.7, tier, false)
			got := svc.computeTokenBreakdown(pricing, tokens, 1.7, tier, true)
			multiple := 1.0
			if contextTokens > 272000 {
				multiple = 1.5
			}
			expected := *standard
			applyCostBreakdownMultiplier(&expected, multiple)
			expected.LongContextBillingApplied = contextTokens > 272000
			require.Equal(t, expected, *got)
			require.Equal(t, 4.0, pricing.LongContextInputMultiplier, "shared pricing must not be mutated")
		}
	}
	// Large output alone does not trigger the input-context threshold.
	got := svc.computeTokenBreakdown(pricing, UsageTokens{InputTokens: 100, OutputTokens: 300000}, 1, "", true)
	require.False(t, got.LongContextBillingApplied)
	// Astra cache-only bill: 300K * $2.5/MTok * Fast 2.5 * long-context 1.5.
	got = svc.computeTokenBreakdown(pricing, UsageTokens{CacheReadTokens: 300000}, 1, "priority", true)
	require.InDelta(t, 2.8125, got.CacheReadCost, 1e-10)
}

func TestUniversalLongContextBillingAppliesAcrossModelsAndReplacesIntervals(t *testing.T) {
	svc := NewBillingService(&config.Config{Gateway: config.GatewayConfig{UniversalLongContextBilling: true}}, nil)
	resolver := &ModelPricingResolver{billingService: svc}
	high := 1.0
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol", "claude-sonnet-4", "grok-4", "unknown-model"} {
		resolved := &ResolvedPricing{Mode: BillingModeToken, Source: PricingSourceChannel,
			BasePricing:               &ModelPricing{InputPricePerToken: 1e-6, OutputPricePerToken: 2e-6},
			longContextPricingEnabled: true,
			Intervals:                 []PricingInterval{{MinTokens: 100000, InputPrice: &high, OutputPrice: &high}}}
		tokens := UsageTokens{InputTokens: 272001, OutputTokens: 10}
		got, err := svc.calculateTokenCost(resolved, CostInput{Model: model, Tokens: tokens, RateMultiplier: 1, Resolver: resolver})
		require.NoError(t, err)
		require.InDelta(t, (272001e-6+20e-6)*1.5, got.TotalCost, 1e-10, model)
		require.True(t, got.LongContextBillingApplied)
	}
}

func TestUniversalLongContextBillingDisabledPreservesExistingPolicy(t *testing.T) {
	svc := &BillingService{}
	pricing := &ModelPricing{InputPricePerToken: 1e-6, OutputPricePerToken: 2e-6, LongContextInputThreshold: 200000, LongContextInputMultiplier: 2, LongContextOutputMultiplier: 1.5}
	tokens := UsageTokens{InputTokens: 250000, OutputTokens: 100}
	require.Equal(t, svc.computeTokenBreakdownBase(pricing, tokens, 1, "", true), svc.computeTokenBreakdown(pricing, tokens, 1, "", true))
}

func TestUniversalLongContextGroupCustomPolicy(t *testing.T) {
	threshold, multiplier := 100000, 3.0
	for _, global := range []bool{false, true} {
		svc := &BillingService{cfg: &config.Config{Gateway: config.GatewayConfig{UniversalLongContextBilling: global}}}
		pricing := &ModelPricing{InputPricePerToken: 1e-6, UniformLongContextThreshold: &threshold, UniformLongContextMultiplier: &multiplier, FastMultiplier: pricingMultiplier(2.5)}
		resolver := &ModelPricingResolver{billingService: svc}
		for _, enabled := range []bool{false, true} {
			resolved := &ResolvedPricing{Mode: BillingModeToken, Source: PricingSourceGroup, BasePricing: pricing, longContextPricingEnabled: enabled}
			for _, n := range []int{100000, 100001} {
				accountEnabled := true
				got, err := svc.calculateTokenCost(resolved, CostInput{Model: "custom", Tokens: UsageTokens{InputTokens: n}, RateMultiplier: 1, ServiceTier: "priority", Resolver: resolver, LongContextBillingEnabled: &accountEnabled})
				require.NoError(t, err)
				want := float64(n) * 1e-6 * 2.5
				if enabled && n > threshold {
					want *= multiplier
				}
				require.InDelta(t, want, got.TotalCost, 1e-10)
				require.Equal(t, enabled && n > threshold, got.LongContextBillingApplied)
			}
		}
	}
}

func TestUniversalLongContextValidation(t *testing.T) {
	invalid := 0
	require.Error(t, checkPricesNotNegative(ChannelModelPricing{LongContextThreshold: &invalid}))
	require.Error(t, checkPricesNotNegative(ChannelModelPricing{LongContextMultiplier: pricingMultiplier(0)}))
	require.NoError(t, checkPricesNotNegative(ChannelModelPricing{LongContextMultiplier: pricingMultiplier(2.5)}))
}
