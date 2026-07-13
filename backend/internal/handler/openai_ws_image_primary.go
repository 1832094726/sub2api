package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type wsImagePrimaryTurnInput struct {
	Payload         []byte
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
}

func stableWSImagePrimaryTaskID(apiKeyID int64, payload []byte) string {
	digest := sha256.Sum256(append([]byte(strconv.FormatInt(apiKeyID, 10)+"\x00"), payload...))
	return "imgp_ws_" + hex.EncodeToString(digest[:12])
}

func (h *OpenAIGatewayHandler) handleWSImagePrimaryTurn(
	ctx context.Context,
	input wsImagePrimaryTurnInput,
	writeEvent func([]byte) error,
) (*service.OpenAIForwardResult, bool, error) {
	if h.imagePrimaryRouter == nil || !service.IsImageGenerationIntent("/v1/responses", input.Model, input.Payload) {
		return nil, false, nil
	}
	payload := make(map[string]any)
	if err := json.Unmarshal(input.Payload, &payload); err != nil {
		return nil, true, err
	}
	payload["stream"] = true
	publicID := stableWSImagePrimaryTaskID(input.APIKeyID, input.Payload)
	digest := sha256.Sum256(input.Payload)
	startedAt := time.Now()
	routeResult := h.imagePrimaryRouter.Route(ctx, service.ImagePrimaryRouteRequest{
		PublicID: publicID, UserID: input.UserID, APIKeyID: input.APIKeyID,
		Protocol: service.ImagePrimaryProtocolResponses, Model: input.Model,
		RequestHash: hex.EncodeToString(digest[:]),
		Submit:      &service.ImagePrimarySubmit{ClientTaskID: publicID, Payload: payload},
	})
	taskID := imagePrimaryTaskID(routeResult)
	switch routeResult.Decision {
	case service.ImagePrimaryFallbackAllowed:
		primaryDuration := 0
		if routeResult.Snapshot != nil {
			primaryDuration = int(routeResult.Snapshot.DurationMS)
		}
		if primaryDuration <= 0 && routeResult.Task != nil {
			primaryDuration = int(routeResult.Task.PrimaryDurationMS)
		}
		return &service.OpenAIForwardResult{
			ImageChannel: "openai_native_fallback", PrimaryTaskID: taskID,
			PrimaryDurationMS: primaryDuration, FallbackReason: routeResult.FallbackReason,
		}, false, nil
	case service.ImagePrimaryNotApplicable:
		return nil, false, nil
	case service.ImagePrimaryPending:
		return nil, true, service.NewOpenAIWSClientCloseError(
			coderws.StatusTryAgainLater,
			fmt.Sprintf("image task %s is still running", taskID),
			routeResult.Err,
		)
	case service.ImagePrimarySuccess:
	default:
		return nil, true, errors.New("unknown image primary decision")
	}

	snapshot := routeResult.Snapshot
	if snapshot == nil {
		return nil, true, errors.New("primary image result is unavailable")
	}
	events := snapshot.Events
	if len(events) == 0 && len(snapshot.Response) > 0 {
		terminal, err := json.Marshal(map[string]any{
			"type": "response.completed", "response": json.RawMessage(snapshot.Response),
		})
		if err == nil {
			events = []json.RawMessage{terminal}
		}
	}
	responseID := ""
	for _, raw := range events {
		event := []byte(raw)
		eventType := strings.TrimSpace(gjson.GetBytes(event, "type").String())
		if responseID == "" {
			responseID = strings.TrimSpace(gjson.GetBytes(event, "response.id").String())
		}
		if isWSPrimaryTerminalEvent(eventType) {
			if annotated, err := sjson.SetBytes(event, "metadata.primary_task_id", taskID); err == nil {
				event = annotated
			}
		}
		if writeEvent != nil {
			if err := writeEvent(event); err != nil {
				return nil, true, err
			}
		}
	}
	imageCount := service.CountOpenAIResponseImageOutputs(snapshot.Response, snapshot.Events)
	result := &service.OpenAIForwardResult{
		RequestID: responseID, Model: input.Model, BillingModel: input.Model,
		Stream: true, OpenAIWSMode: true, Duration: time.Since(startedAt),
		ImageCount: imageCount, ImageSize: snapshot.Size,
		ImageChannel: "chatgpt2api_primary", PrimaryTaskID: taskID,
	}
	h.recordPrimaryResponsesUsage(ctx, responsesPrimaryRequestInput{
		Body: input.Payload, Model: input.Model, UserID: input.UserID, APIKeyID: input.APIKeyID,
		APIKey: input.APIKey, User: input.User, Subscription: input.Subscription,
		APIKeyService: input.APIKeyService, InboundEndpoint: input.InboundEndpoint,
		UserAgent: input.UserAgent, IPAddress: input.IPAddress, Stream: true,
	}, routeResult)
	return result, true, nil
}

func isWSPrimaryTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "error":
		return true
	default:
		return false
	}
}
