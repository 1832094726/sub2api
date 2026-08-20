package handler

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIRealtimeBillingResult(t *testing.T) {
	require.Nil(t, openAIRealtimeBillingResult("gpt-realtime-2.1-mini", time.Second, false))
	require.Nil(t, openAIRealtimeBillingResult("gpt-realtime-2.1-mini", 0, true))

	first := openAIRealtimeBillingResult("gpt-realtime-2.1-mini", 90*time.Second, true)
	second := openAIRealtimeBillingResult("gpt-realtime-2.1-mini", 90*time.Second, true)
	require.NotNil(t, first)
	require.NotNil(t, second)
	require.True(t, strings.HasPrefix(first.RequestID, "openai_realtime:"))
	require.NotEqual(t, first.RequestID, second.RequestID)
	require.True(t, first.Stream)
	require.Equal(t, "realtime", first.AudioUsage.Mode)
	require.Equal(t, 1.5, first.AudioUsage.DurationOrUnits)
}

func TestOpenAIRealtimeOriginAllowed(t *testing.T) {
	cfg := &config.Config{}
	cfg.CORS.AllowedOrigins = []string{"https://desktop.example"}

	tests := []struct {
		name      string
		origin    string
		host      string
		forwarded string
		tls       bool
		duplicate bool
		want      bool
	}{
		{name: "server client without origin", host: "api.huanvel.com", want: true},
		{name: "same origin behind tls proxy", origin: "https://api.huanvel.com", host: "api.huanvel.com", forwarded: "https", want: true},
		{name: "same origin direct tls", origin: "https://api.huanvel.com", host: "api.huanvel.com", tls: true, want: true},
		{name: "configured desktop origin", origin: "https://desktop.example", host: "api.huanvel.com", forwarded: "https", want: true},
		{name: "electron null origin", origin: "null", host: "api.huanvel.com", want: true},
		{name: "electron file origin", origin: "file://", host: "api.huanvel.com", want: true},
		{name: "foreign web origin", origin: "https://evil.example", host: "api.huanvel.com", forwarded: "https", want: false},
		{name: "origin path rejected", origin: "https://api.huanvel.com/path", host: "api.huanvel.com", forwarded: "https", want: false},
		{name: "duplicate origin rejected", origin: "https://api.huanvel.com", host: "api.huanvel.com", forwarded: "https", duplicate: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://"+tt.host+"/v1/realtime", nil)
			req.Host = tt.host
			if tt.origin != "" {
				req.Header.Add("Origin", tt.origin)
				if tt.duplicate {
					req.Header.Add("Origin", tt.origin)
				}
			}
			if tt.forwarded != "" {
				req.Header.Set("X-Forwarded-Proto", tt.forwarded)
			}
			if tt.tls {
				req.TLS = &tls.ConnectionState{}
			}
			require.Equal(t, tt.want, openAIRealtimeOriginAllowed(req, cfg))
		})
	}
}

func TestOpenAIRealtimeEarlyGuards(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("non websocket request", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
		(&OpenAIGatewayHandler{}).OpenAIRealtime(c)
		require.Equal(t, http.StatusUpgradeRequired, recorder.Code)
	})

	t.Run("non openai group", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = realtimeUpgradeRequest()
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
			Group: &service.Group{Platform: service.PlatformGrok, AllowLive: true},
		})
		(&OpenAIGatewayHandler{}).OpenAIRealtime(c)
		require.Equal(t, http.StatusNotFound, recorder.Code)
	})

	t.Run("live disabled", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = realtimeUpgradeRequest()
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
			Group: &service.Group{Platform: service.PlatformOpenAI, AllowLive: false},
		})
		(&OpenAIGatewayHandler{}).OpenAIRealtime(c)
		require.Equal(t, http.StatusForbidden, recorder.Code)
	})
}

func realtimeUpgradeRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "keep-alive, Upgrade")
	return req
}
