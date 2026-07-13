package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefaultDailyHealthScheduleStaggersAcrossDay(t *testing.T) {
	now := time.Date(2026, time.July, 13, 15, 0, 0, 0, time.UTC)

	cronExpression, nextRun := defaultDailyHealthSchedule(125, now)

	require.Equal(t, "25 14 * * *", cronExpression)
	require.Equal(t, time.Date(2026, time.July, 14, 14, 25, 0, 0, time.UTC), nextRun)
}

func TestDefaultDailyHealthScheduleUsesTodayWhenSlotIsAhead(t *testing.T) {
	now := time.Date(2026, time.July, 13, 1, 0, 0, 0, time.UTC)

	_, nextRun := defaultDailyHealthSchedule(125, now)

	require.Equal(t, time.Date(2026, time.July, 13, 14, 25, 0, 0, time.UTC), nextRun)
}

func TestDefaultDailyHealthScheduleSpreadsSequentialAccountsAcrossDay(t *testing.T) {
	now := time.Date(2026, time.July, 13, 0, 0, 0, 0, time.UTC)
	minMinute := 24 * 60
	maxMinute := 0
	seen := make(map[string]struct{})
	for accountID := int64(1); accountID <= 79; accountID++ {
		cronExpression, nextRun := defaultDailyHealthSchedule(accountID, now)
		seen[cronExpression] = struct{}{}
		minuteOfDay := nextRun.Hour()*60 + nextRun.Minute()
		minMinute = min(minMinute, minuteOfDay)
		maxMinute = max(maxMinute, minuteOfDay)
	}
	require.Len(t, seen, 79)
	require.Greater(t, maxMinute-minMinute, 20*60)
}
