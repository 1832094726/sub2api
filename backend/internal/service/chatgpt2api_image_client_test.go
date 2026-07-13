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
