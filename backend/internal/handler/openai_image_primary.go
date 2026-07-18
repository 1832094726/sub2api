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
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	imageChannelContextKey         = "image_channel"
	imagePrimaryTaskIDContextKey   = "image_primary_task_id"
	imageFallbackReasonContextKey  = "image_fallback_reason"
	imagePrimaryDurationContextKey = "image_primary_duration_ms"
)

type imagePrimaryRouting interface {
	Route(context.Context, service.ImagePrimaryRouteRequest) service.ImagePrimaryRouteResult
	QueryOwnedTask(context.Context, int64, int64, string) (*service.ImagePrimaryTask, error)
	MarkSettled(context.Context, int64) error
}

type imagePrimaryRequestInput struct {
	Body            []byte
	ContentType     string
	Model           string
	UserID          int64
	APIKeyID        int64
	APIKey          *service.APIKey
	User            *service.User
	Subscription    *service.UserSubscription
	APIKeyService   service.APIKeyQuotaUpdater
	InboundEndpoint string
	UserAgent       string
	IPAddress       string
	Multipart       bool
	Stream          bool
}

type responsesPrimaryRequestInput = imagePrimaryRequestInput

func (h *OpenAIGatewayHandler) imagePrimaryEnabledForAPIKey(apiKey *service.APIKey) bool {
	if h.imagePrimaryRouter == nil {
		return false
	}
	// Focused handler tests construct the handler without the production config.
	if h.cfg == nil {
		return true
	}
	if !h.cfg.ChatGPT2APIImage.PrimaryEnabled || apiKey == nil || apiKey.GroupID == nil {
		return false
	}
	for _, groupID := range h.cfg.ChatGPT2APIImage.GroupIDs {
		if groupID == *apiKey.GroupID {
			return true
		}
	}
	return false
}

func (h *OpenAIGatewayHandler) handleResponsesImagePrimary(c *gin.Context, input responsesPrimaryRequestInput) bool {
	if !h.imagePrimaryEnabledForAPIKey(input.APIKey) {
		bindImageChannel(c, "openai_native", "", "")
		return false
	}

	publicID := "imgp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	payload := make(map[string]any)
	if err := json.Unmarshal(input.Body, &payload); err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse Responses request")
		return true
	}
	digest := sha256.Sum256(append([]byte(c.Request.URL.Path+"\x00"), input.Body...))
	result := h.imagePrimaryRouter.Route(c.Request.Context(), service.ImagePrimaryRouteRequest{
		PublicID: publicID, UserID: input.UserID, APIKeyID: input.APIKeyID,
		Protocol: service.ImagePrimaryProtocolResponses, Model: input.Model,
		RequestHash: hex.EncodeToString(digest[:]),
		Submit:      &service.ImagePrimarySubmit{ClientTaskID: publicID, Payload: payload},
	})
	bindImagePrimaryDuration(c, result)
	taskID := imagePrimaryTaskID(result)
	switch result.Decision {
	case service.ImagePrimarySuccess:
		bindImageChannel(c, "chatgpt2api_primary", taskID, "")
		h.recordPrimaryResponsesUsage(c.Request.Context(), input, result)
		writeResponsesPrimaryResponse(c, result.Snapshot, input.Stream)
		return true
	case service.ImagePrimaryPending:
		bindImageChannel(c, "chatgpt2api_primary", taskID, "")
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

func writeResponsesPrimaryResponse(c *gin.Context, snapshot *service.ImagePrimarySnapshot, stream bool) {
	if snapshot == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "upstream_error", "message": "Primary image result is unavailable"}})
		return
	}
	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		for _, event := range snapshot.Events {
			if len(event) == 0 {
				continue
			}
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", event)
		}
		_, _ = io.WriteString(c.Writer, "data: [DONE]\n\n")
		return
	}
	if len(snapshot.Response) == 0 {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "upstream_error", "message": "Primary Responses result is unavailable"}})
		return
	}
	c.Data(http.StatusOK, "application/json", snapshot.Response)
}

func (h *OpenAIGatewayHandler) handleImagePrimary(c *gin.Context, input imagePrimaryRequestInput) bool {
	if !h.imagePrimaryEnabledForAPIKey(input.APIKey) {
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
	bindImagePrimaryDuration(c, result)
	taskID := imagePrimaryTaskID(result)
	switch result.Decision {
	case service.ImagePrimarySuccess:
		bindImageChannel(c, "chatgpt2api_primary", taskID, "")
		h.recordPrimaryImageUsage(c.Request.Context(), input, result)
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

func (h *OpenAIGatewayHandler) recordPrimaryImageUsage(ctx context.Context, input imagePrimaryRequestInput, result service.ImagePrimaryRouteResult) {
	if h.gatewayService == nil || result.Snapshot == nil || input.APIKey == nil || input.User == nil {
		return
	}
	imageSize := result.Snapshot.Size
	if imageSize == "" && result.Task != nil && result.Task.ImageSize != nil {
		imageSize = *result.Task.ImageSize
	}
	primaryDuration := int(result.Snapshot.DurationMS)
	publicTaskID := imagePrimaryTaskID(result)
	h.submitMandatoryUsageRecordTask(ctx, func(usageCtx context.Context) {
		err := h.gatewayService.RecordPrimaryImageUsage(usageCtx, &service.OpenAIPrimaryUsageInput{
			PublicTaskID: publicTaskID, ImageCount: len(result.Snapshot.Data),
			ImageSize: imageSize, Model: input.Model, APIKey: input.APIKey, User: input.User,
			Subscription: input.Subscription, InboundEndpoint: input.InboundEndpoint,
			UpstreamEndpoint: "/api/image-tasks", UserAgent: input.UserAgent, IPAddress: input.IPAddress,
			RequestPayloadHash: service.HashUsageRequestPayload(input.Body), APIKeyService: input.APIKeyService,
			ImageChannel: "chatgpt2api_primary", PrimaryDurationMS: primaryDuration,
		})
		if err == nil && result.Task != nil {
			_ = h.imagePrimaryRouter.MarkSettled(usageCtx, result.Task.ID)
		}
	})
}

func (h *OpenAIGatewayHandler) recordPrimaryResponsesUsage(ctx context.Context, input responsesPrimaryRequestInput, result service.ImagePrimaryRouteResult) {
	if h.gatewayService == nil || result.Snapshot == nil || input.APIKey == nil || input.User == nil {
		return
	}
	imageCount := service.CountOpenAIResponseImageOutputs(result.Snapshot.Response, result.Snapshot.Events)
	usage := primaryResponseUsage(result.Snapshot)
	imageSize := result.Snapshot.Size
	if imageSize == "" && result.Task != nil && result.Task.ImageSize != nil {
		imageSize = *result.Task.ImageSize
	}
	publicTaskID := imagePrimaryTaskID(result)
	h.submitMandatoryUsageRecordTask(ctx, func(usageCtx context.Context) {
		err := h.gatewayService.RecordPrimaryImageUsage(usageCtx, &service.OpenAIPrimaryUsageInput{
			PublicTaskID: publicTaskID, ImageCount: imageCount,
			ImageSize: imageSize, Model: input.Model, Usage: usage, APIKey: input.APIKey, User: input.User,
			Subscription: input.Subscription, InboundEndpoint: input.InboundEndpoint,
			UpstreamEndpoint: "/api/image-tasks/responses", UserAgent: input.UserAgent, IPAddress: input.IPAddress,
			RequestPayloadHash: service.HashUsageRequestPayload(input.Body), APIKeyService: input.APIKeyService,
			ImageChannel: "chatgpt2api_primary", PrimaryDurationMS: int(result.Snapshot.DurationMS),
		})
		if err == nil && result.Task != nil {
			_ = h.imagePrimaryRouter.MarkSettled(usageCtx, result.Task.ID)
		}
	})
}

func primaryResponseUsage(snapshot *service.ImagePrimarySnapshot) service.OpenAIUsage {
	if snapshot == nil {
		return service.OpenAIUsage{}
	}
	usageRaw := snapshot.Usage
	if len(usageRaw) == 0 && len(snapshot.Response) > 0 {
		usageRaw = json.RawMessage(gjson.GetBytes(snapshot.Response, "usage").Raw)
	}
	if len(usageRaw) == 0 {
		for index := len(snapshot.Events) - 1; index >= 0; index-- {
			candidate := gjson.GetBytes(snapshot.Events[index], "response.usage")
			if candidate.Exists() {
				usageRaw = json.RawMessage(candidate.Raw)
				break
			}
		}
	}
	if len(usageRaw) == 0 {
		return service.OpenAIUsage{}
	}
	return service.OpenAIUsage{
		InputTokens:              int(gjson.GetBytes(usageRaw, "input_tokens").Int()),
		OutputTokens:             int(gjson.GetBytes(usageRaw, "output_tokens").Int()),
		CacheReadInputTokens:     int(gjson.GetBytes(usageRaw, "input_tokens_details.cached_tokens").Int()),
		CacheCreationInputTokens: int(gjson.GetBytes(usageRaw, "input_tokens_details.cache_creation_tokens").Int()),
		ImageInputTokens:         int(gjson.GetBytes(usageRaw, "input_tokens_details.image_tokens").Int()),
		ImageOutputTokens:        int(gjson.GetBytes(usageRaw, "output_tokens_details.image_tokens").Int()),
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
	payload := map[string]any{"created": time.Now().Unix(), "data": snapshot.Data}
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

func imageChannelMetadata(c *gin.Context) (channel, taskID, fallbackReason string) {
	if value, ok := c.Get(imageChannelContextKey); ok {
		channel, _ = value.(string)
	}
	if value, ok := c.Get(imagePrimaryTaskIDContextKey); ok {
		taskID, _ = value.(string)
	}
	if value, ok := c.Get(imageFallbackReasonContextKey); ok {
		fallbackReason, _ = value.(string)
	}
	return
}

func bindImagePrimaryDuration(c *gin.Context, result service.ImagePrimaryRouteResult) {
	var duration int64
	if result.Snapshot != nil {
		duration = result.Snapshot.DurationMS
	}
	if duration <= 0 && result.Task != nil {
		duration = result.Task.PrimaryDurationMS
	}
	if duration > 0 {
		c.Set(imagePrimaryDurationContextKey, int(duration))
	}
}

func imagePrimaryDurationMetadata(c *gin.Context) int {
	value, ok := c.Get(imagePrimaryDurationContextKey)
	if !ok {
		return 0
	}
	duration, _ := value.(int)
	return duration
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
