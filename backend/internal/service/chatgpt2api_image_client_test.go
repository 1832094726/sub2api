package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

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
