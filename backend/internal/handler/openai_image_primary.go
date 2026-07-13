package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	imageChannelContextKey        = "image_channel"
	imagePrimaryTaskIDContextKey  = "image_primary_task_id"
	imageFallbackReasonContextKey = "image_fallback_reason"
)

type imagePrimaryRouting interface {
	Route(context.Context, service.ImagePrimaryRouteRequest) service.ImagePrimaryRouteResult
	QueryOwnedTask(context.Context, int64, int64, string) (*service.ImagePrimaryTask, error)
}

type imagePrimaryRequestInput struct {
	Body        []byte
	ContentType string
	Model       string
	UserID      int64
	APIKeyID    int64
	Multipart   bool
	Stream      bool
}

func (h *OpenAIGatewayHandler) handleImagePrimary(c *gin.Context, input imagePrimaryRequestInput) bool {
	if h.imagePrimaryRouter == nil {
		bindImageChannel(c, "openai_native", "", "")
		return false
	}
	publicID := "imgp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	submit := &service.ImagePrimarySubmit{ClientTaskID: publicID}
	protocol := service.ImagePrimaryProtocolImages
	if input.Multipart {
		protocol = service.ImagePrimaryProtocolEdits
		body, contentType, err := addMultipartClientTaskID(input.Body, input.ContentType, publicID)
		if err != nil {
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to prepare image edit request")
			return true
		}
		submit.Body = body
		submit.ContentType = contentType
	} else {
		if err := json.Unmarshal(input.Body, &submit.Payload); err != nil {
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse image request")
			return true
		}
	}

	hashInput := append([]byte(c.Request.URL.Path+"\x00"+input.ContentType+"\x00"), input.Body...)
	digest := sha256.Sum256(hashInput)
	result := h.imagePrimaryRouter.Route(c.Request.Context(), service.ImagePrimaryRouteRequest{
		PublicID: publicID, UserID: input.UserID, APIKeyID: input.APIKeyID,
		Protocol: protocol, Model: input.Model, RequestHash: hex.EncodeToString(digest[:]), Submit: submit,
	})
	taskID := imagePrimaryTaskID(result)
	switch result.Decision {
	case service.ImagePrimarySuccess:
		bindImageChannel(c, "chatgpt2api_primary", taskID, "")
		writeImagePrimaryResponse(c, result.Snapshot, input.Stream)
		return true
	case service.ImagePrimaryPending:
		bindImageChannel(c, "chatgpt2api_primary", taskID, "")
		if taskID != "" {
			c.Header("X-Image-Task-Id", taskID)
		}
		h.errorResponse(c, http.StatusGatewayTimeout, "image_primary_pending_timeout", "Image task is still running")
		return true
	case service.ImagePrimaryFallbackAllowed:
		bindImageChannel(c, "openai_native_fallback", taskID, result.FallbackReason)
		return false
	default:
		bindImageChannel(c, "openai_native", "", "")
		return false
	}
}

func (h *OpenAIGatewayHandler) ImageTask(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok || h.imagePrimaryRouter == nil {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Image task not found")
		return
	}
	task, err := h.imagePrimaryRouter.QueryOwnedTask(c.Request.Context(), subject.UserID, apiKey.ID, c.Param("task_id"))
	if err != nil || task == nil {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Image task not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id": task.PublicID, "status": task.Status, "protocol": task.Protocol,
		"model": task.Model, "image_count": task.ImageCount, "image_size": task.ImageSize,
		"primary_duration_ms": task.PrimaryDurationMS, "fallback_duration_ms": task.FallbackDurationMS,
		"fallback_reason": task.FallbackReason, "created_at": task.CreatedAt, "updated_at": task.UpdatedAt,
	})
}

func writeImagePrimaryResponse(c *gin.Context, snapshot *service.ImagePrimarySnapshot, stream bool) {
	if snapshot == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "upstream_error", "message": "Primary image result is unavailable"}})
		return
	}
	c.Header("X-Image-Task-Id", snapshot.ID)
	if stream {
		c.Header("Content-Type", "text/event-stream")
		for _, raw := range snapshot.Data {
			var item map[string]any
			if json.Unmarshal(raw, &item) != nil {
				continue
			}
			item["type"] = "image_generation.completed"
			payload, _ := json.Marshal(item)
			_, _ = fmt.Fprintf(c.Writer, "event: image_generation.completed\ndata: %s\n\n", payload)
		}
		_, _ = io.WriteString(c.Writer, "data: [DONE]\n\n")
		return
	}
	payload := map[string]any{"data": snapshot.Data}
	if len(snapshot.Usage) > 0 {
		payload["usage"] = snapshot.Usage
	}
	c.JSON(http.StatusOK, payload)
}

func bindImageChannel(c *gin.Context, channel, taskID, fallbackReason string) {
	c.Set(imageChannelContextKey, channel)
	if taskID != "" {
		c.Set(imagePrimaryTaskIDContextKey, taskID)
		c.Header("X-Image-Task-Id", taskID)
	}
	if fallbackReason != "" {
		c.Set(imageFallbackReasonContextKey, fallbackReason)
	}
}

func imagePrimaryTaskID(result service.ImagePrimaryRouteResult) string {
	if result.Task != nil && result.Task.PublicID != "" {
		return result.Task.PublicID
	}
	if result.Snapshot != nil {
		return result.Snapshot.ID
	}
	return ""
}

func addMultipartClientTaskID(body []byte, contentType, taskID string) ([]byte, string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
		return nil, "", fmt.Errorf("invalid multipart content type")
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var output bytes.Buffer
	writer := multipart.NewWriter(&output)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		if part.FormName() == "client_task_id" {
			_ = part.Close()
			continue
		}
		target, err := writer.CreatePart(part.Header)
		if err != nil {
			return nil, "", err
		}
		if _, err := io.Copy(target, part); err != nil {
			return nil, "", err
		}
		_ = part.Close()
	}
	if err := writer.WriteField("client_task_id", taskID); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return output.Bytes(), writer.FormDataContentType(), nil
}
