package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	svc.releaseCodexFingerprintLease(idsA)
	idsA2, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, rootA2)
	require.NoError(t, err)
	svc.releaseCodexFingerprintLease(idsA2)
	idsB, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, rootB)
	require.NoError(t, err)
	defer svc.releaseCodexFingerprintLease(idsB)

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
	svc.releaseCodexFingerprintLease(root)
	child, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, childBody)
	require.NoError(t, err)
	defer svc.releaseCodexFingerprintLease(child)

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
	svc.releaseCodexFingerprintLease(first)
	second, err := svc.resolveCodexFingerprintIDsForRequest(ctx, c, account, nil, []byte(`{"client_metadata":{"installation_id":"device-a","turn_id":"turn-2","turn_started_at_unix_ms":2}}`))
	require.NoError(t, err)
	defer svc.releaseCodexFingerprintLease(second)
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
	svc.releaseCodexFingerprintLease(first)
	second, err := svc.resolveCodexFingerprintIDsForRequest(ctx, newCodexConversationTestContext(77), account, nil, body)
	require.NoError(t, err)
	defer svc.releaseCodexFingerprintLease(second)
	otherKey, err := svc.resolveCodexFingerprintIDsForRequest(ctx, newCodexConversationTestContext(78), account, nil, body)
	require.NoError(t, err)
	defer svc.releaseCodexFingerprintLease(otherKey)

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

func TestCodexConversationStatefulRootLeaseSerializesTurns(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newCodexConversationTestAccount()
	body := []byte(`{"model":"gpt-5.6","client_metadata":{"session_id":"serialized-root","thread_id":"serialized-root"}}`)

	first, err := svc.resolveCodexFingerprintIDsForRequest(context.Background(), newCodexConversationTestContext(77), account, nil, body)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	_, err = svc.resolveCodexFingerprintIDsForRequest(waitCtx, newCodexConversationTestContext(77), account, nil, body)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "same root must wait for the active turn")

	svc.releaseCodexFingerprintLease(first)
	next, err := svc.resolveCodexFingerprintIDsForRequest(context.Background(), newCodexConversationTestContext(77), account, nil, body)
	require.NoError(t, err)
	defer svc.releaseCodexFingerprintLease(next)
	require.Equal(t, first.sessionID, next.sessionID)
	require.NotEqual(t, first.turnID, next.turnID)
}

func TestCodexConversationStatefulRootLeaseSerializesAcrossUpstreamAccounts(t *testing.T) {
	svc := &OpenAIGatewayService{}
	accountA := newCodexConversationTestAccount()
	accountB := newCodexConversationTestAccount()
	accountB.ID++
	accountB.Credentials = map[string]any{"chatgpt_account_id": "chatgpt-conversation-test-b"}
	body := []byte(`{"model":"gpt-5.6","client_metadata":{"session_id":"shared-downstream-root","thread_id":"shared-downstream-root"}}`)

	first, err := svc.resolveCodexFingerprintIDsForRequest(context.Background(), newCodexConversationTestContext(77), accountA, nil, body)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	_, err = svc.resolveCodexFingerprintIDsForRequest(waitCtx, newCodexConversationTestContext(77), accountB, nil, body)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "same downstream root must not split into concurrent turns across upstream accounts")

	svc.releaseCodexFingerprintLease(first)
	next, err := svc.resolveCodexFingerprintIDsForRequest(context.Background(), newCodexConversationTestContext(77), accountB, nil, body)
	require.NoError(t, err)
	defer svc.releaseCodexFingerprintLease(next)
	require.Equal(t, first.rootLeaseScope, next.rootLeaseScope)
	require.NotEqual(t, first.conversationRootKey, next.conversationRootKey, "upstream identity generation remains account-scoped")
}

func TestCodexConversationActiveResponseCanInterruptDetachedTurn(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newCodexConversationTestAccount()
	c := newCodexConversationTestContext(77)
	SetOpenAIHTTPResponseOwner(c, 1201, 77)
	body := []byte(`{"model":"gpt-5.6","client_metadata":{"session_id":"interrupt-root","thread_id":"interrupt-root"}}`)
	ids, err := svc.resolveCodexFingerprintIDsForRequest(context.Background(), c, account, nil, body)
	require.NoError(t, err)
	stageCodexFingerprintIDs(c, ids)

	parent, parentCancel := context.WithCancel(context.Background())
	turnCtx, finish, err := svc.beginCodexFingerprintActiveTurn(parent, c, ids, false)
	require.NoError(t, err)
	parentCancel()
	select {
	case <-turnCtx.Done():
		t.Fatal("ordinary downstream disconnect must keep draining the upstream turn")
	case <-time.After(20 * time.Millisecond):
	}

	svc.observeCodexFingerprintResponseID(c, account, "resp_interrupt_1")
	done, found := svc.CancelCodexFingerprintActiveResponse("resp_interrupt_1", 1201, 77)
	require.True(t, found)
	select {
	case <-turnCtx.Done():
		require.ErrorIs(t, context.Cause(turnCtx), errCodexFingerprintTurnInterrupted)
	case <-time.After(time.Second):
		t.Fatal("interrupt did not cancel the active turn")
	}
	finish()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("active turn cleanup acknowledgement did not arrive")
	}
	require.Empty(t, svc.getCodexFingerprintResponseRoot(context.Background(), ids.poolScope, "resp_interrupt_1"), "interrupted response must not become a continuation anchor")
	svc.releaseCodexFingerprintLease(ids)

	next, err := svc.resolveCodexFingerprintIDsForRequest(context.Background(), c, account, nil, body)
	require.NoError(t, err)
	defer svc.releaseCodexFingerprintLease(next)
	require.Equal(t, ids.sessionID, next.sessionID)
	require.NotEqual(t, ids.turnID, next.turnID)
}

func TestCodexConversationOfficialClientDisconnectCancelsTurn(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newCodexConversationTestAccount()
	c := newCodexConversationTestContext(77)
	body := []byte(`{"model":"gpt-5.6","client_metadata":{"session_id":"official-root","thread_id":"official-root"}}`)
	ids, err := svc.resolveCodexFingerprintIDsForRequest(context.Background(), c, account, nil, body)
	require.NoError(t, err)
	defer svc.releaseCodexFingerprintLease(ids)

	parent, cancel := context.WithCancel(context.Background())
	turnCtx, finish, err := svc.beginCodexFingerprintActiveTurn(parent, c, ids, true)
	require.NoError(t, err)
	defer finish()
	cancel()
	select {
	case <-turnCtx.Done():
		require.ErrorIs(t, turnCtx.Err(), context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("official client disconnect did not cancel the active turn")
	}
}

func TestOpenAIResponsesCancelResponseID(t *testing.T) {
	c, _ := gin.CreateTestContext(nil)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/resp_abc-123/cancel", nil)
	responseID, ok := OpenAIResponsesCancelResponseID(c)
	require.True(t, ok)
	require.Equal(t, "resp_abc-123", responseID)

	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	_, ok = OpenAIResponsesCancelResponseID(c)
	require.False(t, ok)
}

func TestOpenAIResponseContinuationEligibilityRejectsIncompleteAnchors(t *testing.T) {
	require.True(t, openAIResponseContinuationEligible([]byte(`{"id":"resp_ok","status":"completed"}`), ""))
	require.True(t, openAIResponseContinuationEligible([]byte(`{"response":{"id":"resp_ok"}}`), "response.completed"))
	require.False(t, openAIResponseContinuationEligible([]byte(`{"id":"resp_cancel","status":"cancelled"}`), ""))
	require.False(t, openAIResponseContinuationEligible([]byte(`{"response":{"id":"resp_incomplete","status":"incomplete"}}`), ""))
	require.False(t, openAIResponseContinuationEligible([]byte(`{"response":{"id":"resp_failed"}}`), "response.failed"))
}
