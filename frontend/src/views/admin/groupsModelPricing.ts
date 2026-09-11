import type { PricingFormEntry } from "@/components/admin/channel/types";
import type { ChannelModelPricing } from "@/api/admin/channels";
import { apiIntervalsToForm, createDefaultTimePricingForm, formIntervalsToAPI, mTokToPerToken, perTokenToMTok, toNullableNumber } from "@/components/admin/channel/types";

export const emptyGroupPricing = (): PricingFormEntry => ({
  models: [],
  billing_mode: "token",
  input_price: null,
  output_price: null,
  cache_write_price: null,
  cache_write_1h_price: null,
  cache_read_price: null,
  image_input_price: null,
  image_output_price: null,
  per_request_price: null,
  fast_multiplier: null,
  flex_multiplier: null,
  max_reasoning_effort_multiplier: null,
  long_context_threshold: null,
  long_context_multiplier: null,
  intervals: [],
  time_pricing: createDefaultTimePricingForm(),
});



export const groupPricingFromAPI = (
  pricing: ChannelModelPricing[] | undefined,
): PricingFormEntry[] =>
  (pricing || []).map((entry) => ({
    models: entry.models || [],
    billing_mode: entry.billing_mode || "token",
    input_price: perTokenToMTok(entry.input_price),
    output_price: perTokenToMTok(entry.output_price),
    cache_write_price: perTokenToMTok(entry.cache_write_price),
    cache_write_1h_price: perTokenToMTok(entry.cache_write_1h_price),
    cache_read_price: perTokenToMTok(entry.cache_read_price),
    image_input_price: perTokenToMTok(entry.image_input_price),
    image_output_price: perTokenToMTok(entry.image_output_price),
    per_request_price: entry.per_request_price,
    fast_multiplier: entry.fast_multiplier ?? null,
    flex_multiplier: entry.flex_multiplier ?? null,
    max_reasoning_effort_multiplier: entry.max_reasoning_effort_multiplier ?? null,
    long_context_threshold: entry.long_context_threshold ?? null,
    long_context_multiplier: entry.long_context_multiplier ?? null,

    intervals: apiIntervalsToForm(entry.intervals || []),
    time_pricing: createDefaultTimePricingForm(),
  }));

export const groupPricingToAPI = (
  pricing: PricingFormEntry[],
  platform: string,
): ChannelModelPricing[] =>
  pricing
    .filter((entry) => entry.models.length > 0)
    .map((entry) => ({
      platform,
      models: entry.models,
      billing_mode: entry.billing_mode,
      input_price: mTokToPerToken(entry.input_price),
      output_price: mTokToPerToken(entry.output_price),
      cache_write_price: mTokToPerToken(entry.cache_write_price),
      cache_write_1h_price: mTokToPerToken(entry.cache_write_1h_price),
      cache_read_price: mTokToPerToken(entry.cache_read_price),
      image_input_price: mTokToPerToken(entry.image_input_price),
      image_output_price: mTokToPerToken(entry.image_output_price),
      per_request_price: toNullableNumber(entry.per_request_price),
      fast_multiplier: toNullableNumber(entry.fast_multiplier),
      flex_multiplier: toNullableNumber(entry.flex_multiplier),
      max_reasoning_effort_multiplier: toNullableNumber(entry.max_reasoning_effort_multiplier),
      long_context_threshold: toNullableNumber(entry.long_context_threshold),
      long_context_multiplier: toNullableNumber(entry.long_context_multiplier),

      intervals:
        entry.billing_mode === "token"
          ? []
          : formIntervalsToAPI(entry.intervals || []),
      time_pricing: null,
    }));

