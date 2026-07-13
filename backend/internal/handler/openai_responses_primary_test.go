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

func TestResponsesImageIntentUsesPrimaryNonStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	responseBody := json.RawMessage(`{"id":"resp_1","status":"completed","output":[{"type":"image_generation_call","result":"final"}]}`)
	router := &fakeImagePrimaryRouting{result: service.ImagePrimaryRouteResult{
		Decision: service.ImagePrimarySuccess,
		Task:     &service.ImagePrimaryTask{ID: 1, PublicID: "imgp_resp_1"},
		Snapshot: &service.ImagePrimarySnapshot{ID: "imgp_resp_1", Status: "success", Mode: "response", Response: responseBody},
	}}
	h := &OpenAIGatewayHandler{imagePrimaryRouter: router}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	handled := h.handleResponsesImagePrimary(c, responsesPrimaryRequestInput{
		Body:  []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation"}],"input":"draw"}`),
		Model: "gpt-5.4", UserID: 7, APIKeyID: 9,
	})

	require.True(t, handled)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, string(responseBody), recorder.Body.String())
	require.Equal(t, "imgp_resp_1", recorder.Header().Get("X-Image-Task-Id"))
}

func TestResponsesImageIntentUsesPrimarySSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	events := []json.RawMessage{
		json.RawMessage(`{"type":"response.created"}`),
		json.RawMessage(`{"type":"response.output_item.done","item":{"type":"image_generation_call","result":"final"}}`),
		json.RawMessage(`{"type":"response.completed","response":{"status":"completed"}}`),
	}
	router := &fakeImagePrimaryRouting{result: service.ImagePrimaryRouteResult{
		Decision: service.ImagePrimarySuccess,
		Task:     &service.ImagePrimaryTask{ID: 1, PublicID: "imgp_resp_stream"},
		Snapshot: &service.ImagePrimarySnapshot{ID: "imgp_resp_stream", Status: "success", Mode: "response", Events: events},
	}}
	h := &OpenAIGatewayHandler{imagePrimaryRouter: router}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	handled := h.handleResponsesImagePrimary(c, responsesPrimaryRequestInput{
		Body:  []byte(`{"model":"gpt-5.4","stream":true,"tools":[{"type":"image_generation"}],"input":"draw"}`),
		Model: "gpt-5.4", UserID: 7, APIKeyID: 9, Stream: true,
	})

	require.True(t, handled)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Header().Get("Content-Type"), "text/event-stream")
	require.Contains(t, recorder.Body.String(), `data: {"type":"response.output_item.done"`)
	require.Contains(t, recorder.Body.String(), "data: [DONE]")
}
