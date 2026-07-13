package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type primaryImageUsageLogRepoStub struct {
	*openAIRecordUsageLogRepoStub
	logs []*UsageLog
}

func (s *primaryImageUsageLogRepoStub) CreatePrimaryImage(_ context.Context, log *UsageLog) (bool, error) {
	for _, existing := range s.logs {
		if existing.RequestID == log.RequestID && existing.APIKeyID == log.APIKeyID {
			return false, nil
		}
	}
	s.logs = append(s.logs, log)
	return true, nil
}

type dedupPrimaryBillingRepo struct {
	*openAIRecordUsageBillingRepoStub
	seen map[string]struct{}
}

func (s *dedupPrimaryBillingRepo) Apply(_ context.Context, cmd *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	key := cmd.RequestID
	if _, ok := s.seen[key]; ok {
		return &UsageBillingApplyResult{Applied: false}, nil
	}
	s.seen[key] = struct{}{}
	s.calls++
	s.lastCmd = cmd
	return &UsageBillingApplyResult{Applied: true}, nil
}

func TestRecordPrimaryImageUsageBillsOnceWithoutAccount(t *testing.T) {
	usageRepo := &primaryImageUsageLogRepoStub{openAIRecordUsageLogRepoStub: &openAIRecordUsageLogRepoStub{}}
	billingRepo := &dedupPrimaryBillingRepo{openAIRecordUsageBillingRepoStub: &openAIRecordUsageBillingRepoStub{}}
	cfg := &config.Config{Default: config.DefaultConfig{RateMultiplier: 1}}
	svc := &OpenAIGatewayService{
		usageLogRepo: usageRepo, usageBillingRepo: billingRepo,
		billingService: NewBillingService(cfg, nil), cfg: cfg,
	}
	apiKey := &APIKey{ID: 9, User: &User{ID: 7}}
	input := &OpenAIPrimaryUsageInput{
		PublicTaskID: "imgp_1", ImageCount: 1, ImageSize: "2K", Model: "gpt-image-2",
		APIKey: apiKey, User: apiKey.User, ImageChannel: "chatgpt2api_primary",
	}

	require.NoError(t, svc.RecordPrimaryImageUsage(context.Background(), input))
	require.NoError(t, svc.RecordPrimaryImageUsage(context.Background(), input))
	require.Equal(t, 1, billingRepo.calls)
	require.Len(t, usageRepo.logs, 1)
	require.Zero(t, usageRepo.logs[0].AccountID)
	require.Equal(t, "chatgpt2api_primary", valueOrEmpty(usageRepo.logs[0].ImageChannel))
	require.Equal(t, "image", valueOrEmpty(usageRepo.logs[0].BillingMode))
}

func TestRecordPrimaryResponseUsageWithoutImageUsesTokenBilling(t *testing.T) {
	usageRepo := &primaryImageUsageLogRepoStub{openAIRecordUsageLogRepoStub: &openAIRecordUsageLogRepoStub{}}
	billingRepo := &dedupPrimaryBillingRepo{openAIRecordUsageBillingRepoStub: &openAIRecordUsageBillingRepoStub{}}
	cfg := &config.Config{Default: config.DefaultConfig{RateMultiplier: 1}}
	svc := &OpenAIGatewayService{
		usageLogRepo: usageRepo, usageBillingRepo: billingRepo,
		billingService: NewBillingService(cfg, nil), cfg: cfg,
	}
	apiKey := &APIKey{ID: 9, User: &User{ID: 7}}
	input := &OpenAIPrimaryUsageInput{
		PublicTaskID: "imgp_text_1", Model: "gpt-5.4", APIKey: apiKey, User: apiKey.User,
		Usage: OpenAIUsage{InputTokens: 100, OutputTokens: 20}, ImageChannel: "chatgpt2api_primary",
	}

	require.NoError(t, svc.RecordPrimaryImageUsage(context.Background(), input))
	require.Len(t, usageRepo.logs, 1)
	require.Equal(t, "token", valueOrEmpty(usageRepo.logs[0].BillingMode))
	require.Equal(t, 100, usageRepo.logs[0].InputTokens)
	require.Equal(t, 20, usageRepo.logs[0].OutputTokens)
	require.Zero(t, usageRepo.logs[0].ImageCount)
	require.Equal(t, 100, billingRepo.lastCmd.InputTokens)
}
