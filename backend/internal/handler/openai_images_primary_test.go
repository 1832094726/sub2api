package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fakeImagePrimaryRouting struct {
	result    service.ImagePrimaryRouteResult
	ownedTask *service.ImagePrimaryTask
	routeCall service.ImagePrimaryRouteRequest
}

func (f *fakeImagePrimaryRouting) Route(_ context.Context, request service.ImagePrimaryRouteRequest) service.ImagePrimaryRouteResult {
	f.routeCall = request
	return f.result
}

func (f *fakeImagePrimaryRouting) QueryOwnedTask(context.Context, int64, int64, string) (*service.ImagePrimaryTask, error) {
	return f.ownedTask, nil
}

func TestImagesPrimarySuccessWritesResponseAndTaskID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := &fakeImagePrimaryRouting{result: service.ImagePrimaryRouteResult{
		Decision: service.ImagePrimarySuccess,
		Task:     &service.ImagePrimaryTask{PublicID: "imgp_1"},
		Snapshot: &service.ImagePrimarySnapshot{
			ID: "imgp_1", Status: service.ImagePrimaryStatusSuccess,
			Data: []json.RawMessage{json.RawMessage(`{"b64_json":"result"}`)},
		},
	}}
	h := &OpenAIGatewayHandler{imagePrimaryRouter: router}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)

	handled := h.handleImagePrimary(c, imagePrimaryRequestInput{
		Body:  []byte(`{"model":"gpt-image-2","prompt":"cat"}`),
		Model: "gpt-image-2", UserID: 7, APIKeyID: 9,
	})

	require.True(t, handled)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "imgp_1", recorder.Header().Get("X-Image-Task-Id"))
	require.Contains(t, recorder.Body.String(), `"b64_json":"result"`)
}

func TestImagesPrimaryPendingReturnsTaskWithoutFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := &fakeImagePrimaryRouting{result: service.ImagePrimaryRouteResult{
		Decision: service.ImagePrimaryPending,
		Task:     &service.ImagePrimaryTask{PublicID: "imgp_pending"},
	}}
	h := &OpenAIGatewayHandler{imagePrimaryRouter: router}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)

	handled := h.handleImagePrimary(c, imagePrimaryRequestInput{
		Body:  []byte(`{"model":"gpt-image-2","prompt":"cat"}`),
		Model: "gpt-image-2", UserID: 7, APIKeyID: 9,
	})

	require.True(t, handled)
	require.Equal(t, http.StatusGatewayTimeout, recorder.Code)
	require.Equal(t, "imgp_pending", recorder.Header().Get("X-Image-Task-Id"))
	require.Contains(t, recorder.Body.String(), "image_primary_pending_timeout")
}

func TestImageTaskQueryRejectsDifferentAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{imagePrimaryRouter: &fakeImagePrimaryRouting{ownedTask: nil}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/images/tasks/imgp_owned_by_1", nil)
	c.Params = gin.Params{{Key: "task_id", Value: "imgp_owned_by_1"}}
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 2})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7})

	h.ImageTask(c)

	require.Equal(t, http.StatusNotFound, recorder.Code)
}
