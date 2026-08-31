package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newOpenAIRootPoolTestContext(apiKeyID int64, logicalID string) *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.Header.Set(openCodeSessionAffinityHeader, logicalID)
	c.Request.Header.Set(openAIRootPoolHeader, "interview-coder")
	c.Request.Header.Set(openAIRootPoolClientHeader, "gank-interview-desktop")
	c.Set("api_key", &APIKey{ID: apiKeyID})
	return c
}

func TestOpenAIRootPoolCapsOneThousandLogicalConversationsAtEightRoots(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	extracted := make(map[string]struct{})
	hashes := make(map[string]struct{})
	explicitHashes := make(map[string]struct{})
	forwardedPromptCacheKeys := make(map[string]struct{})

	for i := 0; i < 1000; i++ {
		c := newOpenAIRootPoolTestContext(901, fmt.Sprintf("problem-%04d", i))
		body := []byte(fmt.Sprintf(
			`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"question-%04d"}]}`,
			i,
		))
		extracted[svc.ExtractSessionID(c, body)] = struct{}{}
		hashes[svc.GenerateSessionHash(c, body)] = struct{}{}
		explicitHashes[svc.GenerateExplicitSessionHash(c, body)] = struct{}{}
		forwardedBody, forwardedKey, err := applyOpenAIRootPoolPromptCacheKey(c, body)
		require.NoError(t, err)
		require.Equal(t, forwardedKey, gjson.GetBytes(forwardedBody, "prompt_cache_key").String())
		require.Equal(t, 1, int(gjson.GetBytes(forwardedBody, "messages.#").Int()))
		require.False(t, gjson.GetBytes(forwardedBody, "previous_response_id").Exists())
		forwardedPromptCacheKeys[forwardedKey] = struct{}{}
	}

	require.Len(t, extracted, 8)
	require.Len(t, hashes, 8)
	require.Len(t, explicitHashes, 8)
	require.Len(t, forwardedPromptCacheKeys, 8)
}

func TestOpenAIRootPoolKeepsOneLogicalConversationInOneSlotAcrossTurns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	c := newOpenAIRootPoolTestContext(901, "problem-stable")
	firstBody := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"first screenshot"}]}`)
	secondBody := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"first screenshot"},{"role":"assistant","content":"answer"},{"role":"user","content":"follow up"}]}`)

	require.Equal(t, svc.ExtractSessionID(c, firstBody), svc.ExtractSessionID(c, secondBody))
	require.Equal(t, svc.GenerateSessionHash(c, firstBody), svc.GenerateSessionHash(c, secondBody))
	require.NotContains(t, svc.ExtractSessionID(c, firstBody), "problem-stable")
}

func TestOpenAIRootPoolRequiresBothOptInHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"question"}]}`)

	c := newOpenAIRootPoolTestContext(901, "logical-session")
	c.Request.Header.Del(openAIRootPoolClientHeader)
	require.Equal(t, "logical-session", svc.ExtractSessionID(c, body))

	c = newOpenAIRootPoolTestContext(901, "logical-session")
	c.Request.Header.Del(openAIRootPoolHeader)
	require.Equal(t, "logical-session", svc.ExtractSessionID(c, body))
}

func TestOpenAIRootPoolRewritesOnlySessionIdentityAndKeepsCompleteMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := newOpenAIRootPoolTestContext(901, "problem-stable")
	body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"question"},{"role":"assistant","content":"answer"},{"role":"user","content":"follow up"}]}`)

	forwardedBody, forwardedKey, err := applyOpenAIRootPoolPromptCacheKey(c, body)
	require.NoError(t, err)
	require.NotEmpty(t, forwardedKey)
	require.Equal(t, 3, int(gjson.GetBytes(forwardedBody, "messages.#").Int()))
	require.Equal(t, "question", gjson.GetBytes(forwardedBody, "messages.0.content").String())
	require.Equal(t, "answer", gjson.GetBytes(forwardedBody, "messages.1.content").String())
	require.Equal(t, "follow up", gjson.GetBytes(forwardedBody, "messages.2.content").String())
	require.False(t, gjson.GetBytes(forwardedBody, "previous_response_id").Exists())
}

func TestOpenAIRootPoolIsIsolatedByAPIKeyClientAndPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"question"}]}`)

	base := newOpenAIRootPoolTestContext(901, "logical-session")
	baseID := svc.ExtractSessionID(base, body)

	otherKey := newOpenAIRootPoolTestContext(902, "logical-session")
	require.NotEqual(t, baseID, svc.ExtractSessionID(otherKey, body))

	otherClient := newOpenAIRootPoolTestContext(901, "logical-session")
	otherClient.Request.Header.Set(openAIRootPoolClientHeader, "another-client")
	require.NotEqual(t, baseID, svc.ExtractSessionID(otherClient, body))

	otherPool := newOpenAIRootPoolTestContext(901, "logical-session")
	otherPool.Request.Header.Set(openAIRootPoolHeader, "another-pool")
	require.NotEqual(t, baseID, svc.ExtractSessionID(otherPool, body))
}
