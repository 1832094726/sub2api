package service

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newStatelessChatSessionContext(t *testing.T, userAgent, origin string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if userAgent != "" {
		c.Request.Header.Set("User-Agent", userAgent)
	}
	if origin != "" {
		c.Request.Header.Set("Origin", origin)
	}
	return c
}

func TestResolveChatCompletionsSession_DynamicStatelessRequestsUseBoundedPool(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c := newStatelessChatSessionContext(t, "Mozilla/5.0 Chrome/140.0.0.0 Safari/537.36", "chrome-extension://openjobautofill")
	hashes := make(map[string]struct{})
	upstreamKeys := make(map[string]struct{})

	for i := 0; i < 1000; i++ {
		body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","messages":[{"role":"system","content":"OJAF_MAP_FIELDS"},{"role":"user","content":"dynamic page %d"}],"stream":false}`, i))
		hash, upstreamKey := svc.ResolveChatCompletionsSession(c, body, 42)
		require.NotEmpty(t, hash)
		require.Contains(t, upstreamKey, openAIStatelessChatSessionPrefix)
		hashes[hash] = struct{}{}
		upstreamKeys[upstreamKey] = struct{}{}
	}

	require.LessOrEqual(t, len(hashes), int(openAIStatelessChatSessionPoolSize))
	require.LessOrEqual(t, len(upstreamKeys), int(openAIStatelessChatSessionPoolSize))
	require.Greater(t, len(upstreamKeys), 1, "dynamic requests should spread across concurrent lanes")
}

func TestResolveChatCompletionsSession_ExplicitSessionUnaffected(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c := newStatelessChatSessionContext(t, "Mozilla/5.0 Chrome/140.0.0.0", "")
	c.Request.Header.Set("session_id", "explicit-session")
	body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hello"}]}`)

	hash, upstreamKey := svc.ResolveChatCompletionsSession(c, body, 42)
	wantHash, _ := deriveOpenAISessionHashes("explicit-session")
	require.Equal(t, wantHash, hash)
	require.Equal(t, "explicit-session", upstreamKey)
}

func TestResolveChatCompletionsSession_MultiTurnKeepsStableBoundedLane(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c := newStatelessChatSessionContext(t, "ai-sdk/openai/2.0.16 node/v22", "")
	first := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"system","content":"solve the screenshot"},{"role":"user","content":[{"type":"text","text":"screenshot"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUESTION_A"}}]}]}`)
	followUp := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"system","content":"combine all screenshots"},{"role":"user","content":[{"type":"text","text":"screenshot"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUESTION_A"}}]},{"role":"assistant","content":"first answer"},{"role":"user","content":[{"type":"text","text":"voice transcript and next screenshot"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUESTION_A_PART_2"}}]}]}`)

	firstHash, firstKey := svc.ResolveChatCompletionsSession(c, first, 42)
	followUpHash, followUpKey := svc.ResolveChatCompletionsSession(c, followUp, 42)
	require.NotEmpty(t, firstKey)
	require.Contains(t, firstKey, openAIStatelessChatSessionPrefix)
	require.Equal(t, firstKey, followUpKey)
	require.Equal(t, firstHash, followUpHash)
}

func TestResolveChatCompletionsSession_ThousandMultiTurnQuestionsUseAtMostEightRoots(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c := newStatelessChatSessionContext(t, "ai-sdk/openai/2.0.16 node/v22", "")
	hashes := make(map[string]struct{})
	upstreamKeys := make(map[string]struct{})

	for i := 0; i < 1000; i++ {
		first := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","messages":[{"role":"system","content":"solve screenshot"},{"role":"user","content":[{"type":"text","text":"question %d"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUESTION_%d"}}]}]}`, i, i))
		followUp := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","messages":[{"role":"system","content":"combine all screenshots"},{"role":"user","content":[{"type":"text","text":"question %d"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUESTION_%d"}}]},{"role":"assistant","content":"answer %d"},{"role":"user","content":"voice transcript and follow up %d"}]}`, i, i, i, i))
		firstHash, firstKey := svc.ResolveChatCompletionsSession(c, first, 42)
		followUpHash, followUpKey := svc.ResolveChatCompletionsSession(c, followUp, 42)
		require.NotEmpty(t, firstHash)
		require.Contains(t, firstKey, openAIStatelessChatSessionPrefix)
		require.Equal(t, firstKey, followUpKey)
		require.Equal(t, firstHash, followUpHash)
		hashes[firstHash] = struct{}{}
		upstreamKeys[firstKey] = struct{}{}
	}

	require.LessOrEqual(t, len(hashes), int(openAIStatelessChatSessionPoolSize))
	require.LessOrEqual(t, len(upstreamKeys), int(openAIStatelessChatSessionPoolSize))
	require.Greater(t, len(upstreamKeys), 1, "independent questions should spread across concurrent lanes")
}

func TestResolveChatCompletionsSession_PreviousResponseDoesNotEnterPool(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c := newStatelessChatSessionContext(t, "Mozilla/5.0 Chrome/140.0.0.0", "")
	body := []byte(`{"model":"gpt-5.6-sol","previous_response_id":"resp_123","messages":[{"role":"user","content":"continue"}]}`)

	_, upstreamKey := svc.ResolveChatCompletionsSession(c, body, 42)
	require.Empty(t, upstreamKey)
}

func TestResolveChatCompletionsSession_IsolatesAPIKeyModelAndClient(t *testing.T) {
	svc := &OpenAIGatewayService{}
	body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"same request"}]}`)
	chrome := newStatelessChatSessionContext(t, "Mozilla/5.0 Chrome/140.0.0.0", "")
	edge := newStatelessChatSessionContext(t, "Mozilla/5.0 Chrome/140.0.0.0 Edg/140.0.0.0", "")

	_, base := svc.ResolveChatCompletionsSession(chrome, body, 42)
	_, otherKey := svc.ResolveChatCompletionsSession(chrome, body, 43)
	_, otherClient := svc.ResolveChatCompletionsSession(edge, body, 42)
	otherModelBody := []byte(`{"model":"gpt-5.6-terra","messages":[{"role":"user","content":"same request"}]}`)
	_, otherModel := svc.ResolveChatCompletionsSession(chrome, otherModelBody, 42)

	require.NotEmpty(t, base)
	require.NotEqual(t, base, otherKey)
	require.NotEqual(t, base, otherClient)
	require.NotEqual(t, base, otherModel)
}

func TestIsPoolableOpenAIChatCompletionsRequest_RequiresFirstUserAnchor(t *testing.T) {
	require.True(t, isPoolableOpenAIChatCompletionsRequest([]byte(`{"model":"gpt-5.6-sol","messages":[{"role":"system","content":"map"},{"role":"user","content":"page"}]}`)))
	require.True(t, isPoolableOpenAIChatCompletionsRequest([]byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"one"},{"role":"assistant","content":"answer"},{"role":"user","content":"two"}]}`)))
	require.False(t, isPoolableOpenAIChatCompletionsRequest([]byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":""},{"role":"assistant","content":"answer"}]}`)))
	require.False(t, isPoolableOpenAIChatCompletionsRequest([]byte(`{"model":"gpt-5.6-sol","input":"responses shape","messages":[{"role":"user","content":"page"}]}`)))
}
