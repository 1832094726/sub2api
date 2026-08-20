package handler

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// OpenAIRealtime exposes the public OpenAI Realtime WebSocket API through an
// OpenAI API-key account. It is deliberately separate from /v1/live, whose
// upstream is ChatGPT Live over WebRTC and whose account/session semantics are
// different.
func (h *OpenAIGatewayHandler) OpenAIRealtime(c *gin.Context) {
	if c == nil || c.Request == nil || !isOpenAIWSUpgradeRequest(c.Request) {
		h.errorResponse(c, http.StatusUpgradeRequired, "invalid_request_error", "WebSocket upgrade required (Upgrade: websocket)")
		return
	}

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil || apiKey.Group == nil || apiKey.Group.Platform != service.PlatformOpenAI {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Realtime API is not supported for this platform")
		return
	}
	if !apiKey.Group.AllowLive {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "Realtime is not enabled for this group")
		return
	}
	if !openAIRealtimeOriginAllowed(c.Request, h.cfg) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "WebSocket origin is not allowed")
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.openai_realtime",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	if err := h.billingCacheService.CheckBillingEligibility(
		c.Request.Context(),
		apiKey.User,
		apiKey,
		apiKey.Group,
		subscription,
		service.QuotaPlatform(c.Request.Context(), apiKey),
	); err != nil {
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.errorResponse(c, status, code, message)
		return
	}

	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		model = service.DefaultOpenAIRealtimeModel
	}

	// A Realtime session owns both slots for its entire WebSocket lifetime. A
	// session cannot be moved to another account after the upstream handshake,
	// because its conversation and audio buffers are connection-local state.
	var streamStarted bool
	userRelease, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userRelease != nil {
		defer userRelease()
	}

	failedAccountIDs := make(map[int64]struct{})
	profitVetoCount := 0
	var selection *service.AccountSelectionResult
	var accountRelease func()
	var err error
	for {
		selection, _, err = h.gatewayService.SelectAccountWithSchedulerForCapability(
			c.Request.Context(),
			apiKey.GroupID,
			"",
			"",
			model,
			failedAccountIDs,
			service.OpenAIUpstreamTransportHTTPSSE,
			service.OpenAIEndpointCapabilityRealtime,
			false,
			false,
			false,
			service.PlatformOpenAI,
		)
		if err != nil || selection == nil || selection.Account == nil {
			h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "No available OpenAI Realtime accounts")
			return
		}

		var slotStatus openAISlotAcquireResult
		accountRelease, slotStatus = h.acquireResponsesAccountSlot(c, apiKey.GroupID, "", selection, false, &streamStarted, reqLog)
		switch slotStatus {
		case openAISlotAcquireOK:
			// The selected account is now fixed for the lifetime of this session.
		case openAISlotAcquireProfitVetoed:
			if !recordOpenAIProfitVeto(failedAccountIDs, selection.Account.ID, &profitVetoCount) {
				h.handleOpenAIProfitVetoExhausted(c, streamStarted, reqLog, profitVetoCount)
				return
			}
			continue
		default:
			return
		}
		break
	}
	if accountRelease != nil {
		defer accountRelease()
	}

	account := selection.Account
	token, _, err := h.gatewayService.GetRequestCredential(c.Request.Context(), c, account)
	if err != nil {
		h.errorResponse(c, http.StatusBadGateway, "upstream_error", "OpenAI Realtime credential unavailable")
		return
	}

	conn, err := coderws.Accept(c.Writer, c.Request, &coderws.AcceptOptions{
		CompressionMode:    coderws.CompressionContextTakeover,
		InsecureSkipVerify: true, // Origin was checked narrowly above.
		Subprotocols:       []string{"realtime"},
	})
	if err != nil {
		reqLog.Warn("openai_realtime.websocket_accept_failed", zap.Error(err))
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(service.ResolveOpenAIWSClientReadLimitBytes(h.cfg))
	streamStarted = true

	started := time.Now()
	audioObserved, proxyErr := h.gatewayService.ProxyOpenAIRealtime(c.Request.Context(), c, conn, account, token, model)
	elapsed := time.Since(started)
	if proxyErr != nil {
		reqLog.Info("openai_realtime.proxy_closed", zap.Error(proxyErr))
		if !isExpectedOpenAIRealtimeClose(proxyErr) {
			_ = conn.Close(coderws.StatusInternalError, "upstream realtime websocket failed")
		}
	}
	if result := openAIRealtimeBillingResult(model, elapsed, audioObserved); result != nil {
		h.recordOpenAIRealtimeUsage(c, apiKey, account, subscription, result)
	}
}

func isExpectedOpenAIRealtimeClose(err error) bool {
	return isExpectedGrokRealtimeClose(err)
}

func openAIRealtimeBillingResult(model string, elapsed time.Duration, audioObserved bool) *service.OpenAIForwardResult {
	if !audioObserved || elapsed <= 0 {
		return nil
	}
	return &service.OpenAIForwardResult{
		RequestID: service.StableOpenAIRealtimeBillingRequestID(""),
		Model:     strings.TrimSpace(model),
		Stream:    true,
		Duration:  elapsed,
		AudioUsage: &service.AudioUsage{
			Mode:            "realtime",
			DurationOrUnits: elapsed.Minutes(),
		},
	}
}

func (h *OpenAIGatewayHandler) recordOpenAIRealtimeUsage(
	c *gin.Context,
	apiKey *service.APIKey,
	account *service.Account,
	subscription *service.UserSubscription,
	result *service.OpenAIForwardResult,
) {
	if h == nil || c == nil || apiKey == nil || apiKey.User == nil || account == nil || result == nil || result.AudioUsage == nil {
		return
	}
	result.RequestID = service.StableOpenAIRealtimeBillingRequestID(result.RequestID)
	userAgent := c.GetHeader("User-Agent")
	clientIP := ip.GetClientIP(c)
	sessionID := service.ExtractClientSessionID(c)
	inboundEndpoint := GetInboundEndpoint(c)
	upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)
	quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)
	requestPayloadHash := service.HashUsageRequestPayload([]byte(inboundEndpoint + "|" + result.Model))

	h.submitMandatoryUsageRecordTask(c.Request.Context(), func(ctx context.Context) {
		if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
			Result:             result,
			APIKey:             apiKey,
			User:               apiKey.User,
			Account:            account,
			Subscription:       subscription,
			InboundEndpoint:    inboundEndpoint,
			UpstreamEndpoint:   upstreamEndpoint,
			UserAgent:          userAgent,
			IPAddress:          clientIP,
			SessionID:          sessionID,
			RequestPayloadHash: requestPayloadHash,
			APIKeyService:      h.apiKeyService,
			QuotaPlatform:      quotaPlatform,
			ChannelUsageFields: clientRequestedUsageFields(c, service.ChannelMappingResult{}, result.Model, result.UpstreamModel),
		}); err != nil {
			logger.L().With(
				zap.String("component", "handler.openai_gateway.openai_realtime"),
				zap.Int64("user_id", apiKey.User.ID),
				zap.Int64("api_key_id", apiKey.ID),
				zap.Any("group_id", apiKey.GroupID),
				zap.Int64("account_id", account.ID),
			).Error("openai_realtime.record_usage_failed", zap.Error(err))
		}
	})
}

// openAIRealtimeOriginAllowed performs the only Origin relaxation used by the
// public Realtime ingress. Regular web origins must still be same-origin or an
// exact configured CORS origin; file:// and null are accepted for authenticated
// packaged Electron clients only on this endpoint.
func openAIRealtimeOriginAllowed(r *http.Request, cfg *config.Config) bool {
	if r == nil {
		return false
	}
	originValues := r.Header.Values("Origin")
	if len(originValues) == 0 {
		return true
	}
	if len(originValues) != 1 {
		return false
	}
	origin := strings.TrimSpace(originValues[0])
	if origin == "" {
		return false
	}
	if origin == "null" || strings.EqualFold(origin, "file://") {
		return true
	}

	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return false
	}
	requestScheme := "http"
	if r.TLS != nil {
		requestScheme = "https"
	} else if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); strings.EqualFold(forwarded, "http") || strings.EqualFold(forwarded, "https") {
		requestScheme = strings.ToLower(forwarded)
	}
	expected := requestScheme + "://" + r.Host
	if strings.EqualFold(strings.TrimRight(origin, "/"), strings.TrimRight(expected, "/")) {
		return true
	}
	if cfg == nil {
		return false
	}
	for _, allowed := range cfg.CORS.AllowedOrigins {
		allowed = strings.TrimSpace(allowed)
		if allowed != "" && allowed != "*" && strings.EqualFold(strings.TrimRight(origin, "/"), strings.TrimRight(allowed, "/")) {
			return true
		}
	}
	return false
}
