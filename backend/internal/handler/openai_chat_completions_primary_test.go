package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestChatCompletionsImageIntentUsesPrimaryNonStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := &fakeImagePrimaryRouting{result: service.ImagePrimaryRouteResult{
		Decision: service.ImagePrimarySuccess,
		Task:     &service.ImagePrimaryTask{ID: 1, PublicID: "imgp_chat_1"},
		Snapshot: &service.ImagePrimarySnapshot{
			ID: "imgp_chat_1", Status: "success", Mode: "response",
			Response: json.RawMessage(`{"id":"resp_1","model":"gpt-5.4","status":"completed","output":[{"type":"image_generation_call","id":"img_1","result":"base64-image-result"}]}`),
		},
	}}
	h := &OpenAIGatewayHandler{imagePrimaryRouter: router}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	handled := h.handleChatCompletionsImagePrimary(c, chatCompletionsPrimaryRequestInput{
		Body:  []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"draw"}],"tools":[{"type":"image_generation"}]}`),
		Model: "gpt-5.4", UserID: 7, APIKeyID: 9,
	})

	require.True(t, handled)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "data:image/png;base64,base64-image-result")
	tools, ok := router.routeCall.Submit.Payload["tools"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, tools)
	require.Equal(t, "image_generation", tools[len(tools)-1].(map[string]any)["type"])
	require.Equal(t, false, router.routeCall.Submit.Payload["stream"])
}

func TestChatCompletionsImageIntentUsesPrimarySSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := &fakeImagePrimaryRouting{result: service.ImagePrimaryRouteResult{
		Decision: service.ImagePrimarySuccess,
		Task:     &service.ImagePrimaryTask{ID: 1, PublicID: "imgp_chat_stream"},
		Snapshot: &service.ImagePrimarySnapshot{
			ID: "imgp_chat_stream", Status: "success", Mode: "response",
			Events: []json.RawMessage{
				json.RawMessage(`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4"}}`),
				json.RawMessage(`{"type":"response.output_item.done","item":{"type":"image_generation_call","id":"img_1","result":"base64-image-result"}}`),
				json.RawMessage(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.4","status":"completed"}}`),
			},
		},
	}}
	h := &OpenAIGatewayHandler{imagePrimaryRouter: router}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	handled := h.handleChatCompletionsImagePrimary(c, chatCompletionsPrimaryRequestInput{
		Body:  []byte(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"draw"}],"tools":[{"type":"image_generation"}]}`),
		Model: "gpt-5.4", UserID: 7, APIKeyID: 9, Stream: true,
	})

	require.True(t, handled)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Header().Get("Content-Type"), "text/event-stream")
	require.Contains(t, recorder.Body.String(), "data:image/png;base64,base64-image-result")
	require.Contains(t, recorder.Body.String(), "data: [DONE]")
}
