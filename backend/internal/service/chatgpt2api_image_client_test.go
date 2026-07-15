package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatGPT2APIClientGetResponseTaskLoadsEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/image-tasks":
			require.Equal(t, "response-task-1", r.URL.Query().Get("ids"))
			_, _ = w.Write([]byte(`{"items":[{"id":"response-task-1","status":"success","mode":"response"}]}`))
		case "/api/image-tasks/response-task-1/events":
			require.Equal(t, "0", r.URL.Query().Get("after"))
			_, _ = w.Write([]byte(`{"events":[{"type":"response.created"},{"type":"response.completed"}],"next_cursor":2}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewChatGPT2APIImageClient(ChatGPT2APIImageClientConfig{
		BaseURL: server.URL, APIKey: "secret", HTTPClient: server.Client(),
	})
	require.NoError(t, err)

	snapshot, err := client.GetTask(context.Background(), "response-task-1")
	require.NoError(t, err)
	require.Len(t, snapshot.Events, 2)
	var event map[string]any
	require.NoError(t, json.Unmarshal(snapshot.Events[1], &event))
	require.Equal(t, "response.completed", event["type"])
}

func TestChatGPT2APIClientRedactsAuthorization(t *testing.T) {
	const secret = "secret-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+secret, r.Header.Get("Authorization"))
		http.Error(w, "upstream rejected Authorization: Bearer "+secret, http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	client, err := NewChatGPT2APIImageClient(ChatGPT2APIImageClientConfig{
		BaseURL:    server.URL,
		APIKey:     secret,
		HTTPClient: server.Client(),
	})
	require.NoError(t, err)

	_, err = client.GetTask(context.Background(), "task-1")
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret)
}

func TestChatGPT2APIClientOverridesGenerationModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/images/generations", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "codex-gpt-image-2", body["model"])
		require.Equal(t, "imgp_model_override", body["client_task_id"])
		_, _ = w.Write([]byte(`{"id":"imgp_model_override","status":"running"}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewChatGPT2APIImageClient(ChatGPT2APIImageClientConfig{
		BaseURL: server.URL, APIKey: "secret", Model: "codex-gpt-image-2", HTTPClient: server.Client(),
	})
	require.NoError(t, err)

	_, err = client.SubmitImages(context.Background(), &ImagePrimarySubmit{
		ClientTaskID: "imgp_model_override",
		Payload:      map[string]any{"model": "gpt-image-2", "prompt": "draw"},
	})
	require.NoError(t, err)
}

func TestChatGPT2APIClientResolvesGenerationModelForEverySubmission(t *testing.T) {
	models := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		models <- body["model"].(string)
		_, _ = w.Write([]byte(`{"id":"dynamic-model","status":"running"}`))
	}))
	t.Cleanup(server.Close)

	selectedModel := "codex-gpt-image-2"
	client, err := NewChatGPT2APIImageClient(ChatGPT2APIImageClientConfig{
		BaseURL: server.URL,
		APIKey:  "secret",
		ModelResolver: func(context.Context) string {
			return selectedModel
		},
		HTTPClient: server.Client(),
	})
	require.NoError(t, err)

	submit := &ImagePrimarySubmit{ClientTaskID: "dynamic-model", Payload: map[string]any{"prompt": "draw"}}
	_, err = client.SubmitImages(context.Background(), submit)
	require.NoError(t, err)
	selectedModel = "gpt-image-2"
	_, err = client.SubmitImages(context.Background(), submit)
	require.NoError(t, err)

	require.Equal(t, "codex-gpt-image-2", <-models)
	require.Equal(t, "gpt-image-2", <-models)
}
