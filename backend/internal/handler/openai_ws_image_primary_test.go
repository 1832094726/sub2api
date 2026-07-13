package handler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestWSImageTurnCreatesStablePrimaryTaskAndAnnotatesTerminalEvent(t *testing.T) {
	router := &fakeImagePrimaryRouting{result: service.ImagePrimaryRouteResult{
		Decision: service.ImagePrimarySuccess,
		Task:     &service.ImagePrimaryTask{ID: 1, PublicID: "imgp_ws_expected"},
		Snapshot: &service.ImagePrimarySnapshot{
			ID: "imgp_ws_expected", Status: service.ImagePrimaryStatusSuccess, Mode: "response",
			Events: []json.RawMessage{
				json.RawMessage(`{"type":"response.created","response":{"id":"resp_1"}}`),
				json.RawMessage(`{"type":"response.output_item.done","item":{"type":"image_generation_call","id":"img_1","result":"final-image"}}`),
				json.RawMessage(`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`),
			},
		},
	}}
	h := &OpenAIGatewayHandler{imagePrimaryRouter: router}
	payload := []byte(`{"type":"response.create","model":"gpt-5.4","tools":[{"type":"image_generation"}],"input":"draw"}`)
	var events [][]byte

	result, handled, err := h.handleWSImagePrimaryTurn(context.Background(), wsImagePrimaryTurnInput{
		Payload: payload, Model: "gpt-5.4", UserID: 7, APIKeyID: 9,
	}, func(event []byte) error {
		events = append(events, append([]byte(nil), event...))
		return nil
	})

	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, "chatgpt2api_primary", result.ImageChannel)
	require.Equal(t, "imgp_ws_expected", result.PrimaryTaskID)
	require.Equal(t, 1, result.ImageCount)
	require.Len(t, events, 3)
	require.Equal(t, "imgp_ws_expected", gjson.GetBytes(events[2], "metadata.primary_task_id").String())
	require.Equal(t, stableWSImagePrimaryTaskID(9, payload), router.routeCall.PublicID)
}

func TestWSImageTurnPendingRemainsHandledWithoutNativeFallback(t *testing.T) {
	router := &fakeImagePrimaryRouting{result: service.ImagePrimaryRouteResult{
		Decision: service.ImagePrimaryPending,
		Task:     &service.ImagePrimaryTask{ID: 1, PublicID: "imgp_ws_pending"},
	}}
	h := &OpenAIGatewayHandler{imagePrimaryRouter: router}
	payload := []byte(`{"type":"response.create","model":"gpt-5.4","tools":[{"type":"image_generation"}],"input":"draw"}`)

	result, handled, err := h.handleWSImagePrimaryTurn(context.Background(), wsImagePrimaryTurnInput{
		Payload: payload, Model: "gpt-5.4", UserID: 7, APIKeyID: 9,
	}, nil)

	require.Nil(t, result)
	require.True(t, handled)
	require.Error(t, err)
	require.Equal(t, 1, router.routeCalls)
}
