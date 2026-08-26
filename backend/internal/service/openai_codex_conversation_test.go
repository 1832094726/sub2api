package service

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newCodexConversationTestAccount() *Account {
	return &Account{
		ID:       9101,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			codexFingerprintModeExtraKey: "session",
			codexFingerprintSeedExtraKey: testCodexFingerprintSeed,
		},
		Credentials: map[string]any{
			"chatgpt_account_id": "chatgpt-conversation-test",
		},
	}
}

func newCodexConversationTestContext(apiKeyID int64) *gin.Context {
	c, _ := gin.CreateTestContext(nil)
	c.Set("api_key", &APIKey{ID: apiKeyID})
	return c
}

func expectedCodexConversationRootIDs(account *Account, apiKeyID int64, sessionID, threadID string) *codexFingerprintIDs {
	return resolveCodexFingerprintConversationRootIDs(account, apiKeyID, codexClientIdentity{
		sessionID: sessionID,
		threadID:  threadID,
	}, hashCodexConversationValue("session", sessionID))
}

func TestCodexConversationStatefulRootsAreStableAndIsolated(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newCodexConversationTestAccount()
	c := newCodexConversationTestContext(77)
	ctx := context.Background()

	rootA := []byte(`{"model":"gpt-5.6","input":"one","client_metadata":{"session_id":"root-a","thread_id":"root-a"}}`)
	rootA2 := []byte(`{"model":"gpt-5.6","input":"two","client_metadata":{"session_id":"root-a","thread_id":"root-a"}}`)
	rootB := []byte(`{"model":"gpt-5.6","input":"one","client_metadata":{"session_id":"root-b","thread_id":"root-b"}}`)

	idsA, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, rootA)
	require.NoError(t, err)
	idsA2, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, rootA2)
	require.NoError(t, err)
	idsB, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, rootB)
	require.NoError(t, err)

	require.Equal(t, idsA.sessionID, idsA.threadID)
	require.Equal(t, idsA.sessionID, idsA2.sessionID)
	require.Equal(t, idsA.threadID, idsA2.threadID)
	require.NotEqual(t, idsA.turnID, idsA2.turnID)
	require.NotEqual(t, idsA.sessionID, idsB.sessionID)
	require.Equal(t, idsA.installationID, idsB.installationID)
}

func TestCodexConversationChildThreadSharesRootSession(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newCodexConversationTestAccount()
	c := newCodexConversationTestContext(77)
	ctx := context.Background()

	rootBody := []byte(`{"client_metadata":{"session_id":"root-a","thread_id":"root-a"}}`)
	childBody := []byte(`{"client_metadata":{"session_id":"root-a","thread_id":"child-a","parent_thread_id":"root-a"}}`)
	root, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, rootBody)
	require.NoError(t, err)
	child, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, childBody)
	require.NoError(t, err)

	require.Equal(t, root.sessionID, child.sessionID)
	require.Equal(t, root.threadID, root.sessionID)
	require.NotEqual(t, child.threadID, child.sessionID)
	require.Equal(t, child.threadID, resolveConvergedThreadID(child.lineageSeed, scopeCodexAccountIdentityField(account, 77, "thread", "child-a")))
}

func TestCodexConversationIdentityFreeRequestsUseBoundedPool(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newCodexConversationTestAccount()
	c := newCodexConversationTestContext(77)
	ctx := context.Background()

	seenSlots := map[int]struct{}{}
	seenSessions := map[string]struct{}{}
	var held []*codexFingerprintIDs
	for i := 0; i < defaultCodexFingerprintPoolSize; i++ {
		body := []byte(fmt.Sprintf(`{"model":"gpt-5.6","instructions":"same-function","input":"request-%d"}`, i))
		ids, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, http.Header{}, body)
		require.NoError(t, err)
		require.GreaterOrEqual(t, ids.poolSlot, 0)
		seenSlots[ids.poolSlot] = struct{}{}
		seenSessions[ids.sessionID] = struct{}{}
		held = append(held, ids)
	}
	require.Len(t, seenSlots, defaultCodexFingerprintPoolSize)
	require.Len(t, seenSessions, defaultCodexFingerprintPoolSize)
	for _, ids := range held {
		svc.releaseCodexFingerprintLease(ids)
	}

	first, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, []byte(`{"model":"gpt-5.6","instructions":"same-function","input":"next"}`))
	require.NoError(t, err)
	svc.releaseCodexFingerprintLease(first)
	second, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, []byte(`{"model":"gpt-5.6","instructions":"same-function","input":"another"}`))
	require.NoError(t, err)
	defer svc.releaseCodexFingerprintLease(second)
	require.Equal(t, first.poolSlot, second.poolSlot)
	require.Equal(t, first.sessionID, second.sessionID)
	require.NotEqual(t, first.turnID, second.turnID)
}

func TestCodexConversationPreviousResponseFindsOriginalPoolRoot(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newCodexConversationTestAccount()
	c := newCodexConversationTestContext(77)
	ctx := context.Background()

	first, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, []byte(`{"model":"gpt-5.6","input":"first"}`))
	require.NoError(t, err)
	stageCodexFingerprintIDs(c, first)
	svc.bindCodexFingerprintResponseRoot(ctx, c, account, "resp_upstream_1")
	svc.releaseCodexFingerprintLease(first)

	next, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, []byte(`{"model":"gpt-5.6","input":"delta","previous_response_id":"resp_upstream_1","client_metadata":{"session_id":"changed-client-session"}}`))
	require.NoError(t, err)
	defer svc.releaseCodexFingerprintLease(next)
	require.Equal(t, first.poolSlot, next.poolSlot)
	require.Equal(t, first.sessionID, next.sessionID)
	require.Equal(t, "resp_upstream_1", extractCodexFingerprintIngressIdentity(nil, []byte(`{"previous_response_id":"resp_upstream_1"}`)).previousResponseID)
}

func TestCodexConversationMetadataDigestIgnoresPerTurnFields(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newCodexConversationTestAccount()
	c := newCodexConversationTestContext(77)
	ctx := context.Background()

	first, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, []byte(`{"client_metadata":{"installation_id":"device-a","turn_id":"turn-1","turn_started_at_unix_ms":1}}`))
	require.NoError(t, err)
	second, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, []byte(`{"client_metadata":{"installation_id":"device-a","turn_id":"turn-2","turn_started_at_unix_ms":2}}`))
	require.NoError(t, err)
	require.Equal(t, -1, first.poolSlot)
	require.Equal(t, first.sessionID, second.sessionID)
	require.NotEqual(t, first.turnID, second.turnID)
}

func TestCodexConversationPromptCacheKeyCreatesDedicatedRoot(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newCodexConversationTestAccount()
	ctx := context.Background()
	body := []byte(`{"model":"gpt-5.6","input":"hello","prompt_cache_key":"feature-a"}`)

	first, err := svc.resolveCodexFingerprintIDsForRequest(ctx, newCodexConversationTestContext(77), account, nil, body)
	require.NoError(t, err)
	second, err := svc.resolveCodexFingerprintIDsForRequest(ctx, newCodexConversationTestContext(77), account, nil, body)
	require.NoError(t, err)
	otherKey, err := svc.resolveCodexFingerprintIDsForRequest(ctx, newCodexConversationTestContext(78), account, nil, body)
	require.NoError(t, err)

	require.Equal(t, -1, first.poolSlot)
	require.Equal(t, first.sessionID, second.sessionID)
	require.NotEqual(t, first.sessionID, otherKey.sessionID)
	require.NotEmpty(t, first.rootPromptCacheKey)

	scopedBody, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account, 77)
	require.NoError(t, err)
	require.True(t, changed)
	convergedBody, changed, err := applyCodexFingerprintClientMetadataRaw(scopedBody, first)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, first.sessionID, gjson.GetBytes(convergedBody, "prompt_cache_key").String())
}

func TestCodexFingerprintPoolSizeIsBounded(t *testing.T) {
	account := newCodexConversationTestAccount()
	require.Equal(t, defaultCodexFingerprintPoolSize, codexFingerprintPoolSize(account))
	account.Extra[codexFingerprintPoolSizeExtraKey] = 8
	require.Equal(t, 8, codexFingerprintPoolSize(account))
	account.Extra[codexFingerprintPoolSizeExtraKey] = 100
	require.Equal(t, maxCodexFingerprintPoolSize, codexFingerprintPoolSize(account))
	account.Extra[codexFingerprintPoolSizeExtraKey] = 2
	require.Equal(t, minCodexFingerprintPoolSize, codexFingerprintPoolSize(account))
}
