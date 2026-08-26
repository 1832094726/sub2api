package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newCodexFingerprintCacheTestStore(t *testing.T) service.CodexFingerprintStateStore {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := NewGatewayCache(client)
	store, ok := cache.(service.CodexFingerprintStateStore)
	require.True(t, ok)
	return store
}

func TestGatewayCacheCodexFingerprintPoolLeaseIsExclusive(t *testing.T) {
	store := newCodexFingerprintCacheTestStore(t)
	ctx := context.Background()
	candidates := []int{2, 3}

	slot, acquired, err := store.ClaimCodexFingerprintPoolSlot(ctx, "scope-a", candidates, "owner-a", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)
	require.Equal(t, 2, slot)

	slot, acquired, err = store.ClaimCodexFingerprintPoolSlot(ctx, "scope-a", candidates, "owner-b", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)
	require.Equal(t, 3, slot)

	_, acquired, err = store.ClaimCodexFingerprintPoolSlot(ctx, "scope-a", candidates, "owner-c", time.Minute)
	require.NoError(t, err)
	require.False(t, acquired)

	require.NoError(t, store.ReleaseCodexFingerprintPoolSlot(ctx, "scope-a", 2, "wrong-owner"))
	_, acquired, err = store.ClaimCodexFingerprintPoolSlot(ctx, "scope-a", []int{2}, "owner-c", time.Minute)
	require.NoError(t, err)
	require.False(t, acquired)

	require.NoError(t, store.ReleaseCodexFingerprintPoolSlot(ctx, "scope-a", 2, "owner-a"))
	slot, acquired, err = store.ClaimCodexFingerprintPoolSlot(ctx, "scope-a", []int{2}, "owner-c", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)
	require.Equal(t, 2, slot)
}

func TestGatewayCacheCodexFingerprintResponseRootRoundTrip(t *testing.T) {
	store := newCodexFingerprintCacheTestStore(t)
	ctx := context.Background()

	root, err := store.GetCodexFingerprintResponseRoot(ctx, "scope-b", "resp-1")
	require.NoError(t, err)
	require.Empty(t, root)

	require.NoError(t, store.SetCodexFingerprintResponseRoot(ctx, "scope-b", "resp-1", "pool:4", time.Hour))
	root, err = store.GetCodexFingerprintResponseRoot(ctx, "scope-b", "resp-1")
	require.NoError(t, err)
	require.Equal(t, "pool:4", root)

	other, err := store.GetCodexFingerprintResponseRoot(ctx, "scope-c", "resp-1")
	require.NoError(t, err)
	require.Empty(t, other)
}
