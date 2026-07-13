package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefaultDailyHealthScheduleStaggersAcrossDay(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 34, 0, 0, time.UTC)

	cronExpression, nextRun := defaultDailyHealthSchedule(125, now)

	require.Equal(t, "5 2 * * *", cronExpression)
	require.Equal(t, time.Date(2026, time.July, 14, 2, 5, 0, 0, time.UTC), nextRun)
}

func TestDefaultDailyHealthScheduleUsesTodayWhenSlotIsAhead(t *testing.T) {
	now := time.Date(2026, time.July, 13, 1, 0, 0, 0, time.UTC)

	_, nextRun := defaultDailyHealthSchedule(125, now)

	require.Equal(t, time.Date(2026, time.July, 13, 2, 5, 0, 0, time.UTC), nextRun)
}
