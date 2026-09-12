package service

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

type cyberRiskTestRepo struct {
	contentModerationTestRepo
	state CyberUserRiskState
	calls int
}

func (r *cyberRiskTestRepo) RecordCyberUserStrike(context.Context, int64, string) (CyberUserRiskState, error) {
	r.calls++
	return r.state, nil
}
func (r *cyberRiskTestRepo) ResetCyberUserRisk(context.Context, int64) error { return nil }
func TestCyberUserEscalationIndependentOfGeneralBanExclusion(t *testing.T) {
	until := time.Now().Add(time.Hour)
	for _, strikes := range []int{1, 2} {
		repo := &cyberRiskTestRepo{state: CyberUserRiskState{Strikes: strikes, Applied: true}}
		if strikes == 1 {
			repo.state.BlockedUntil = &until
		}
		cfg := defaultContentModerationConfig()
		cfg.CyberUserBlockEnabled = true
		cfg.CyberPolicyExcludeFromBanCount = true
		cfg.AutoBanEnabled = false
		raw, err := json.Marshal(cfg)
		require.NoError(t, err)
		svc := NewContentModerationService(&contentModerationTestSettingRepo{values: map[string]string{SettingKeyRiskControlEnabled: "true", SettingKeyContentModerationConfig: string(raw)}}, repo, nil, nil, nil, nil, nil, nil)
		svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{UserID: 42, RequestID: "unique", Model: "gpt-6-astra", UpstreamMessage: "cyber_policy"})
		require.Equal(t, 1, repo.calls)
		logs := repo.snapshotLogs()
		require.Len(t, logs, 1)
		require.Equal(t, strikes, logs[0].ViolationCount)
		require.Equal(t, strikes == 2, logs[0].AutoBanned)
		if strikes == 1 {
			require.Contains(t, logs[0].Error, "downstream_user_api_blocked_until=")
		}
	}
}
func TestCyberUserAuthSnapshotPreservesTemporaryRestriction(t *testing.T) {
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	svc := &APIKeyService{}
	key := &APIKey{ID: 1, UserID: 42, Key: "test", Status: StatusActive, User: &User{ID: 42, Status: StatusActive, CyberBlockedUntil: &until}}
	snapshot := svc.snapshotFromAPIKey(context.Background(), key)
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var decoded APIKeyAuthSnapshot
	require.NoError(t, json.Unmarshal(raw, &decoded))
	got, used, err := svc.applyAuthCacheEntry("test", &APIKeyAuthCacheEntry{Snapshot: &decoded})
	require.NoError(t, err)
	require.True(t, used)
	require.Equal(t, until, *got.User.CyberBlockedUntil)
	require.True(t, got.User.IsActive(), "API-only suspension must not change login status")
}
