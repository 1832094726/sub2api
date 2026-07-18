package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type chatCompletionsPrimaryRequestInput = imagePrimaryRequestInput

func (h *OpenAIGatewayHandler) handleChatCompletionsImagePrimary(c *gin.Context, input chatCompletionsPrimaryRequestInput) bool {
	if !h.imagePrimaryEnabledForAPIKey(input.APIKey) {
		bindImageChannel(c, "openai_native", "", "")
		return false
	}
	payload, err := chatCompletionsPrimaryPayload(input.Body, input.Stream)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to convert Chat Completions image request")
		return true
	}

	publicID := "imgp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
		writeChatCompletionsPrimaryResponse(c, result.Snapshot, input.Model, input.Stream)
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

func chatCompletionsPrimaryPayload(body []byte, stream bool) (map[string]any, error) {
	var request apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	responsesRequest, err := apicompat.ChatCompletionsToResponses(&request)
	if err != nil {
		return nil, err
	}
	responsesRequest.Stream = stream
	converted, err := json.Marshal(responsesRequest)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		return nil, err
	}

	imageTool := map[string]any{"type": "image_generation"}
	var original map[string]any
	if json.Unmarshal(body, &original) == nil {
		if tools, ok := original["tools"].([]any); ok {
			for _, item := range tools {
				tool, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if tool["type"] == "image_generation" {
					imageTool = tool
					break
				}
			}
		}
	}
	tools, _ := payload["tools"].([]any)
	payload["tools"] = append(tools, imageTool)
	payload["stream"] = stream
	return payload, nil
}

func writeChatCompletionsPrimaryResponse(c *gin.Context, snapshot *service.ImagePrimarySnapshot, model string, stream bool) {
	if snapshot == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "upstream_error", "message": "Primary image result is unavailable"}})
		return
	}
	if !stream {
		var response apicompat.ResponsesResponse
		if len(snapshot.Response) == 0 || json.Unmarshal(snapshot.Response, &response) != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "upstream_error", "message": "Primary Responses result is unavailable"}})
			return
		}
		c.JSON(http.StatusOK, apicompat.ResponsesToChatCompletions(&response, model))
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	state := apicompat.NewResponsesEventToChatState()
	state.Model = model
	state.IncludeUsage = true
	for _, raw := range snapshot.Events {
		var event apicompat.ResponsesStreamEvent
		if json.Unmarshal(raw, &event) != nil {
			continue
		}
		for _, chunk := range apicompat.ResponsesEventToChatChunks(&event, state) {
			sse, err := apicompat.ChatChunkToSSE(chunk)
			if err == nil {
				_, _ = fmt.Fprint(c.Writer, sse)
			}
		}
	}
	for _, chunk := range apicompat.FinalizeResponsesChatStream(state) {
		sse, err := apicompat.ChatChunkToSSE(chunk)
		if err == nil {
			_, _ = fmt.Fprint(c.Writer, sse)
		}
	}
	_, _ = io.WriteString(c.Writer, "data: [DONE]\n\n")
}
