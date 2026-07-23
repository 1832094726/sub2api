package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// OpenAIGatewayHandler handles OpenAI API gateway requests
type OpenAIGatewayHandler struct {
	gatewayService             *service.OpenAIGatewayService
	billingCacheService        *service.BillingCacheService
	apiKeyService              *service.APIKeyService
	usageRecordWorkerPool      *service.UsageRecordWorkerPool
	errorPassthroughService    *service.ErrorPassthroughService
	contentModerationService   *service.ContentModerationService
	opsService                 *service.OpsService
	securityAuditCoordinator   *securityaudit.Coordinator
	grokMediaEligibilityProber grokMediaEligibilityProber
	concurrencyHelper          *ConcurrencyHelper
	imageLimiter               *imageConcurrencyLimiter
	maxAccountSwitches         int
	cfg                        *config.Config
	imagePrimaryRouter         imagePrimaryRouting
}

type wsPrimaryTurnDecision struct {
	result  *service.OpenAIForwardResult
	handled bool
	err     error
}

type grokMediaEligibilityProber interface {
	ProbeMediaEligibility(ctx context.Context, accountID int64) (bool, string, error)
}

const maxOpenAIFirstOutputTimeoutSwitches = 1

func openAIForwardSucceededForScheduling(result *service.OpenAIForwardResult) bool {
	return result.SucceededForScheduling()
}

func resolveOpenAIMessagesDispatchMappedModel(apiKey *service.APIKey, requestedModel string) string {
	if apiKey == nil || apiKey.Group == nil {
		return ""
	}
	return strings.TrimSpace(apiKey.Group.ResolveMessagesDispatchModel(requestedModel))
}

type openAIModelBodyReplaceFunc func([]byte, string) []byte

func openAIModelMappedBody(body []byte, mapped bool, mappedModel string, replace openAIModelBodyReplaceFunc) []byte {
	if !mapped || replace == nil {
		return body
	}
	return replace(body, mappedModel)
}

func seedOpenAIForwardImageIntentHint(c *gin.Context, channelMapped bool, imageIntent bool) {
	if channelMapped {
		// 娓犻亾鏄犲皠鏀瑰彉浜嗚鑼冭姹傦紝淇濇寔 unknown锛岀敱 Forward 鎸夋槧灏勫悗鐨?model/body 鍒濆鍖栥€?
		return
	}
	service.SetOpenAIImageIntentHint(c, imageIntent)
}

func newOpenAIModelMappedBodyCache(body []byte, replace openAIModelBodyReplaceFunc) func(bool, string) []byte {
	replacedBodies := make(map[string][]byte)
	return func(mapped bool, mappedModel string) []byte {
		if !mapped {
			return body
		}
		if cachedBody, ok := replacedBodies[mappedModel]; ok {
			return cachedBody
		}
		replacedBody := openAIModelMappedBody(body, true, mappedModel, replace)
		replacedBodies[mappedModel] = replacedBody
		return replacedBody
	}
}

func usageRecordContext(parent context.Context, base context.Context) context.Context {
	if base == nil {
		base = context.Background()
	}
	if parent == nil {
		return base
	}
	if clientRequestID, _ := parent.Value(ctxkey.ClientRequestID).(string); strings.TrimSpace(clientRequestID) != "" {
		base = context.WithValue(base, ctxkey.ClientRequestID, strings.TrimSpace(clientRequestID))
	}
	if requestID, _ := parent.Value(ctxkey.RequestID).(string); strings.TrimSpace(requestID) != "" {
		base = context.WithValue(base, ctxkey.RequestID, strings.TrimSpace(requestID))
	}
	return base
}

func wrapUsageRecordTaskContext(parent context.Context, task service.UsageRecordTask) service.UsageRecordTask {
	if task == nil {
		return nil
	}
	return func(ctx context.Context) {
		task(usageRecordContext(parent, ctx))
	}
}

func openAICompatibleRequestPlatform(ctx context.Context, apiKey *service.APIKey) string {
	if platform, ok := service.ResolvedTargetPlatformFromContext(ctx); ok {
		if platform == service.PlatformGrok {
			return service.PlatformGrok
		}
		return service.PlatformOpenAI
	}
	if apiKey != nil && apiKey.Group != nil && apiKey.Group.Platform == service.PlatformGrok {
		return service.PlatformGrok
	}
	return service.PlatformOpenAI
}

func openAIResponsesRequiredCapability(imageIntent bool, platform string) service.OpenAIEndpointCapability {
	if imageIntent && platform == service.PlatformOpenAI {
		return service.OpenAIEndpointCapabilityResponses
	}
	return service.OpenAIEndpointCapabilityChatCompletions
}

func allowOpenAICompatibleMessagesDispatch(apiKey *service.APIKey) bool {
	if apiKey == nil || apiKey.Group == nil {
		return true
	}
	if apiKey.Group.Platform == service.PlatformGrok {
		return true
	}
	return apiKey.Group.AllowMessagesDispatch
}

func openAICompatibleTextTargetAllowed(c *gin.Context, apiKey *service.APIKey, model string) bool {
	return compositeTargetPlatformAllowed(c, apiKey, model, service.PlatformOpenAI, service.PlatformGrok)
}

// NewOpenAIGatewayHandler creates a new OpenAIGatewayHandler
func NewOpenAIGatewayHandler(
	gatewayService *service.OpenAIGatewayService,
	concurrencyService *service.ConcurrencyService,
	billingCacheService *service.BillingCacheService,
	apiKeyService *service.APIKeyService,
	usageRecordWorkerPool *service.UsageRecordWorkerPool,
	errorPassthroughService *service.ErrorPassthroughService,
	contentModerationService *service.ContentModerationService,
	opsService *service.OpsService,
	cfg *config.Config,
) *OpenAIGatewayHandler {
	pingInterval := time.Duration(0)
	maxAccountSwitches := 3
	if cfg != nil {
		pingInterval = time.Duration(cfg.Concurrency.PingInterval) * time.Second
		if cfg.Gateway.MaxAccountSwitches > 0 {
			maxAccountSwitches = cfg.Gateway.MaxAccountSwitches
		}
	}
	return &OpenAIGatewayHandler{
		gatewayService:           gatewayService,
		billingCacheService:      billingCacheService,
		apiKeyService:            apiKeyService,
		usageRecordWorkerPool:    usageRecordWorkerPool,
		errorPassthroughService:  errorPassthroughService,
		contentModerationService: contentModerationService,
		opsService:               opsService,
		concurrencyHelper:        NewConcurrencyHelper(concurrencyService, SSEPingFormatComment, pingInterval),
		imageLimiter:             &imageConcurrencyLimiter{},
		maxAccountSwitches:       maxAccountSwitches,
		cfg:                      cfg,
	}
}

// Responses handles OpenAI Responses API endpoint
// POST /openai/v1/responses
func (h *OpenAIGatewayHandler) Responses(c *gin.Context) {
	// 灞€閮ㄥ厹搴曪細纭繚璇?handler 鍐呴儴浠讳綍 panic 閮戒笉浼氬嚮绌垮埌杩涚▼绾с€?
	streamStarted := false
	defer h.recoverResponsesPanic(c, &streamStarted)
	compactStartedAt := time.Now()
	defer h.logOpenAIRemoteCompactOutcome(c, compactStartedAt)
	setOpenAIClientTransportHTTP(c)

	requestStart := time.Now()

	// Get apiKey and user from context (set by ApiKeyAuth middleware)
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.responses",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	// Read request body
	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}

	if len(body) == 0 {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}

	setOpsRequestContext(c, "", false)
	sessionHashBody := body
	body, ok = h.normalizeOpenAIResponsesCompactRequest(c, reqLog, body)
	if !ok {
		return
	}
	// body-signal compact锛氫笂娓?unary 绛夊緟鏈熼棿鍚戜笅娓稿彂 SSE 娉ㄩ噴琛屽績璺筹紝闃叉
	// 鍙嶅悜浠ｇ悊绌洪棽瓒呮椂鎺愭柇闀垮帇缂╄繛鎺ワ紙#3887锛夈€傞鎷嶅欢杩熶竴涓績璺抽棿闅旓紝蹇€?
	// 澶辫触浠嶈蛋 JSON+鐘舵€佺爜閾捐矾锛涙湭鏍囪瀹㈡埛绔祦寮忔垨闂撮殧涓?0 鏃舵槸 no-op銆?
	stopCompactKeepalive := service.StartOpenAICompactSSEKeepalive(c, h.openAICompactKeepaliveInterval())
	defer stopCompactKeepalive()

	// 鏍￠獙璇锋眰浣?JSON 鍚堟硶鎬?
	if !gjson.ValidBytes(body) {
		logRequestBodyParseFailure(reqLog, body, nil)
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}

	// 浣跨敤 gjson 鍙鎻愬彇瀛楁鍋氭牎楠岋紝閬垮厤瀹屾暣 Unmarshal
	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || modelResult.String() == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	reqModel := modelResult.String()
	ensureCompositeTargetPlatform(c, apiKey, reqModel)
	if !compositeTargetPlatformAllowed(c, apiKey, reqModel, service.PlatformOpenAI, service.PlatformGrok) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Model is not supported by this OpenAI-compatible endpoint for composite groups")
		return
	}
	if apiKey.Group != nil && apiKey.Group.Platform == service.PlatformOpenAI {
		if cappedBody, changed := service.ApplyOpenAIReasoningEffortPolicy(body, apiKey.Group.MaxReasoningEffort, apiKey.Group.ReasoningEffortMappings); changed {
			body = cappedBody
		}
	}

	reqStream, ok := parseOpenAICompatibleStream(body)
	if !ok {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", invalidStreamFieldTypeMessage)
		return
	}
	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream))
	previousResponseID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	if previousResponseID != "" {
		previousResponseIDKind := service.ClassifyOpenAIPreviousResponseIDKind(previousResponseID)
		reqLog = reqLog.With(
			zap.Bool("has_previous_response_id", true),
			zap.String("previous_response_id_kind", previousResponseIDKind),
			zap.Int("previous_response_id_len", len(previousResponseID)),
		)
		if previousResponseIDKind == service.OpenAIPreviousResponseIDKindMessageID {
			reqLog.Warn("openai.request_validation_failed",
				zap.String("reason", "previous_response_id_looks_like_message_id"),
			)
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "previous_response_id must be a response.id (resp_*), not a message id")
			return
		}
		reqLog.Warn("openai.request_validation_failed",
			zap.String("reason", "previous_response_id_requires_wsv2"),
		)
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "previous_response_id is only supported on Responses WebSocket v2")
		return
	}

	setOpsRequestContext(c, reqModel, reqStream)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	if decision := h.checkSecurityAudit(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, reqModel, body); decision != nil && !decision.AllowNextStage {
		h.openAISecurityAuditError(c, decision)
		return
	}

	// 浣跨敤 IsExplicitImageGenerationIntent 鎺掗櫎琚姩 image_gen namespace 澹版槑銆?
	// Codex 鍦ㄦ墍鏈夎姹備腑琚姩澹版槑 image_gen namespace锛屽娉涙娴嬩細瀵艰嚧绂佷簡鐢熷浘鐨?
	// 鍒嗙粍涓墍鏈?Codex 璇锋眰琚?403锛?4447锛夛紝骞惰鍗犵敓鍥惧苟鍙戞Ы浣嶃€?
	imageIntent := service.IsExplicitImageGenerationIntent("/v1/responses", reqModel, body)
	if imageIntent && !service.GroupAllowsImageGeneration(apiKey.Group) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", service.ImageGenerationPermissionMessage())
		return
	}
	var imageReleaseFunc func()
	if imageIntent {
		var imageAcquired bool
		imageReleaseFunc, imageAcquired = h.acquireImageGenerationSlot(c, streamStarted)
		if !imageAcquired {
			return
		}
		if imageReleaseFunc != nil {
			defer imageReleaseFunc()
		}
	}

	// 瑙ｆ瀽娓犻亾绾фā鍨嬫槧灏?
	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
	forwardBody := openAIModelMappedBody(body, channelMapping.Mapped, channelMapping.MappedModel, h.gatewayService.ReplaceModelInBody)
	seedOpenAIForwardImageIntentHint(c, channelMapping.Mapped, imageIntent)

	// 鎻愬墠鏍￠獙 function_call_output 鏄惁鍏峰鍙叧鑱斾笂涓嬫枃锛岄伩鍏嶄笂娓?400銆?
	if !h.validateFunctionCallOutputRequest(c, body, reqLog) {
		return
	}

	// 缁戝畾閿欒閫忎紶鏈嶅姟锛屽厑璁?service 灞傚湪闈?failover 閿欒鍦烘櫙澶嶇敤瑙勫垯銆?
	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	// Get subscription info (may be nil)
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	requestPlatform := openAICompatibleRequestPlatform(c.Request.Context(), apiKey)

	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())
	routingStart := time.Now()

	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, reqStream, &streamStarted, reqLog)
	if !acquired {
		return
	}
	// 纭繚璇锋眰鍙栨秷鏃朵篃浼氶噴鏀炬Ы浣嶏紝閬垮厤闀胯繛鎺ヨ鍔ㄤ腑鏂€犳垚娉勬紡
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	// 2. Re-check billing eligibility after wait
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.handleStreamingAwareError(c, status, code, message, streamStarted)
		return
	}

	if imageIntent && h.handleResponsesImagePrimary(c, responsesPrimaryRequestInput{
		Body: forwardBody, Model: reqModel, UserID: subject.UserID, APIKeyID: apiKey.ID,
		APIKey: apiKey, User: apiKey.User, Subscription: subscription, APIKeyService: h.apiKeyService,
		InboundEndpoint: GetInboundEndpoint(c), UserAgent: c.GetHeader("User-Agent"), IPAddress: ip.GetClientIP(c),
		Stream: reqStream,
	}) {
		return
	}

	// Generate session hash (header first; fallback to prompt_cache_key)
	sessionHash := h.gatewayService.GenerateSessionHash(c, sessionHashBody)
	if h.rejectIfCyberSessionBlocked(c, apiKey, sessionHashBody, reqModel, cyberBlockFormatResponses) {
		return
	}
	requireCompact := isOpenAIRemoteCompactPath(c)

	maxAccountSwitches := h.maxAccountSwitches
	switchCount := 0
	firstOutputTimeoutSwitchCount := 0
	failedAccountIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	var lastFailoverErr *service.UpstreamFailoverError
	var oauth429FailoverState service.OpenAIOAuth429FailoverState

	// 鐢熷浘鎰忓浘鐨?/v1/responses 璇锋眰蹇呴』璋冨害鍒扮‘瀹炴敮鎸?Responses API 鐨勮处鍙凤紝鍚﹀垯
	// 浼氬湪 forward 闃舵琚潤榛橀檷绾т负鏃犳硶鐢熷浘鐨?Chat Completions 鐩磋浆锛?4417锛夈€?
	// 浠呭 OpenAI 骞冲彴鐢熸晥锛欸rok 鐢熷浘璧扮嫭绔嬬殑 forwardGrokResponses 璺緞锛屼笉搴旇杩囨护銆?

	// 澶嶇敤鍓嶇疆鏉冮檺涓庡苟鍙戦樁娈靛湪鏈慨鏀?body 涓婄‘璁ょ殑鏄惧紡鐢熷浘鎰忓浘锛岄伩鍏嶅ぇ tools 璇锋眰閲嶅鎵弿銆?
	// 璇ュ垽鏂凡鎺掗櫎 Codex 琚姩 image_gen namespace锛岄伩鍏?CC-only 璐﹀彿琚杩囨护锛?4476锛夈€?
	requiredCapability := openAIResponsesRequiredCapability(imageIntent, requestPlatform)

	for {
		// Streaming Forward intentionally detaches the upstream request so usage can
		// be drained after a disconnect. Re-check the client context before every
		// account attempt so a canceled request never starts a failover replay.
		if !openAIRequestAllowsFailoverReplay(c) {
			return
		}
		// Select account supporting the requested model
		reqLog.Debug("openai.account_selecting", zap.Int("excluded_account_count", len(failedAccountIDs)))
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
			c.Request.Context(),
			apiKey.GroupID,
			previousResponseID,
			sessionHash,
			reqModel,
			failedAccountIDs,
			service.OpenAIUpstreamTransportAny,
			requiredCapability,
			requireCompact,
			false,
			!imageIntent,
			requestPlatform,
		)
		if err != nil {
			if failoverClientGone(c) {
				reqLog.Info("openai.account_select_aborted_client_disconnected", zap.Error(err))
				return
			}
			reqLog.Warn("openai.account_select_failed",
				zap.Error(openAICompatibleSelectionErrorForLog(err, requestPlatform)),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if len(failedAccountIDs) == 0 {
				if errors.Is(err, service.ErrNoAvailableCompactAccounts) {
					markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
					h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "compact_not_supported", "No available accounts support /responses/compact", streamStarted)
					return
				}
				cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel, requestPlatform)
				if !cls.ModelNotFound {
					markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
				}
				h.handleStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
				return
			}
			if lastFailoverErr != nil {
				h.handleFailoverExhausted(c, lastFailoverErr, streamStarted)
			} else {
				h.handleFailoverExhaustedSimple(c, 502, streamStarted)
			}
			return
		}
		if selection == nil || selection.Account == nil {
			cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel, requestPlatform)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimited(c)
			}
			h.handleStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
			return
		}
		if previousResponseID != "" && selection != nil && selection.Account != nil {
			reqLog.Debug("openai.account_selected_with_previous_response_id", zap.Int64("account_id", selection.Account.ID))
		}
		reqLog.Debug("openai.account_schedule_decision",
			zap.String("layer", scheduleDecision.Layer),
			zap.Bool("sticky_previous_hit", scheduleDecision.StickyPreviousHit),
			zap.Bool("sticky_session_hit", scheduleDecision.StickySessionHit),
			zap.Int("candidate_count", scheduleDecision.CandidateCount),
			zap.Int("top_k", scheduleDecision.TopK),
			zap.Int64("latency_ms", scheduleDecision.LatencyMs),
			zap.Float64("load_skew", scheduleDecision.LoadSkew),
		)
		account := selection.Account
		sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
		reqLog.Debug("openai.account_selected", zap.Int64("account_id", account.ID), zap.String("account_name", account.Name))
		setOpsSelectedAccount(c, account.ID, account.Platform)

		accountReleaseFunc, acquired := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)
		if !acquired {
			return
		}

		// Forward request
		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		forwardStart := time.Now()
		// 鐢ㄦ墸闄?compact 蹇冭烦瀛楄妭鐨勫彛寰勫揩鐓э細蹇冭烦娉ㄩ噴涓嶆瀯鎴愯涔夊搷搴旓紝
		// 涓嶈兘鍥犲績璺冲瓧鑺傚彉鍖栬€屾斁寮?failover 鎹㈠彿锛?3887锛夈€?
		writerSizeBeforeForward := service.OpenAICompactKeepaliveAdjustedWrittenSize(c)
		result, err := func() (*service.OpenAIForwardResult, error) {
			defer func() {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
			}()
			return h.gatewayService.Forward(c.Request.Context(), c, account, forwardBody)
		}()
		cyberBlockKeyHTTP := ""
		if service.GetOpsCyberPolicy(c) != nil {
			cyberBlockKeyHTTP = service.CyberSessionBlockKey(apiKey.ID, c, sessionHashBody)
		}
		h.recordCyberPolicyIfMarked(c, apiKey, account, subscription, reqModel, err != nil, cyberBlockKeyHTTP, clientRequestedUsageFields(c, channelMapping, reqModel, ""), service.HashUsageRequestPayload(body))
		forwardDurationMs := time.Since(forwardStart).Milliseconds()
		upstreamLatencyMs, _ := getContextInt64(c, service.OpsUpstreamLatencyMsKey)
		responseLatencyMs := forwardDurationMs
		if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
			responseLatencyMs = forwardDurationMs - upstreamLatencyMs
		}
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, responseLatencyMs)
		if err == nil && result != nil && result.FirstTokenMs != nil {
			service.SetOpsLatencyMs(c, service.OpsTimeToFirstTokenMsKey, int64(*result.FirstTokenMs))
		}
		if err != nil {
			if result != nil && result.ImageCount > 0 {
				reqLog.Warn("openai.forward_partial_error_with_image_result",
					zap.Int64("account_id", account.ID),
					zap.Int("image_count", result.ImageCount),
					zap.Error(err),
				)
			} else {
				var failoverErr *service.UpstreamFailoverError
				if errors.As(err, &failoverErr) {
					if failoverClientGone(c) {
						reqLog.Info("openai.failover_aborted_client_disconnected",
							zap.Int64("account_id", account.ID),
							zap.Int("upstream_status", failoverErr.StatusCode),
						)
						return
					}
					if !openAIForwardMayFailover(c, writerSizeBeforeForward, failoverErr) {
						h.handleFailoverExhausted(c, failoverErr, true)
						return
					}
					if failoverErr.SafeToFailoverAfterWrite && c.Writer.Written() {
						streamStarted = true
					}
					if failoverErr.ShouldReportAccountScheduleFailure() {
						h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, account.GetMappedModel(reqModel), false, nil)
					}
					if !failoverErr.ShouldRetryNextAccount() {
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					if openAIFirstOutputFailoverExhausted(failoverErr, &firstOutputTimeoutSwitchCount) {
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					// 姹犳ā寮忥細鍚岃处鍙烽噸璇?
					if failoverErr.RetryableOnSameAccount {
						retryLimit := account.GetPoolModeRetryCount()
						if sameAccountRetryCount[account.ID] < retryLimit {
							sameAccountRetryCount[account.ID]++
							reqLog.Warn("openai.pool_mode_same_account_retry",
								zap.Int64("account_id", account.ID),
								zap.Int("upstream_status", failoverErr.StatusCode),
								zap.Int("retry_limit", retryLimit),
								zap.Int("retry_count", sameAccountRetryCount[account.ID]),
							)
							select {
							case <-c.Request.Context().Done():
								return
							case <-time.After(sameAccountRetryDelay):
							}
							continue
						}
					}
					h.gatewayService.RecordOpenAIAccountSwitch()
					failedAccountIDs[account.ID] = struct{}{}
					lastFailoverErr = failoverErr
					if switchCount >= maxAccountSwitches {
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					switchCount++
					if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount, &oauth429FailoverState) {
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					failoverSwitchFields := []zap.Field{
						zap.Int64("account_id", account.ID),
						zap.Int("upstream_status", failoverErr.StatusCode),
						zap.Int("switch_count", switchCount),
						zap.Int("max_switches", maxAccountSwitches),
					}
					if account.Proxy != nil {
						failoverSwitchFields = append(failoverSwitchFields,
							zap.Int64("proxy_id", account.Proxy.ID),
							zap.String("proxy_name", account.Proxy.Name),
							zap.String("proxy_host", account.Proxy.Host),
							zap.Int("proxy_port", account.Proxy.Port),
						)
					} else if account.ProxyID != nil {
						failoverSwitchFields = append(failoverSwitchFields, zap.Int64p("proxy_id", account.ProxyID))
					}
					reqLog.Warn("openai.upstream_failover_switching", failoverSwitchFields...)
					continue
				}
				h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, account.GetMappedModel(reqModel), false, nil)
				upstreamErrorAlreadyCommunicated := openAIForwardErrorAlreadyCommunicated(c, writerSizeBeforeForward, err)
				wroteFallback := false
				if !upstreamErrorAlreadyCommunicated {
					wroteFallback = h.ensureForwardErrorResponse(c, streamStarted)
				}
				fields := []zap.Field{
					zap.Int64("account_id", account.ID),
					zap.Bool("fallback_error_response_written", wroteFallback),
					zap.Bool("upstream_error_response_already_written", upstreamErrorAlreadyCommunicated),
					zap.Error(err),
				}
				if shouldLogOpenAIForwardFailureAsWarn(c, wroteFallback) {
					reqLog.Warn("openai.forward_failed", fields...)
					return
				}
				reqLog.Error("openai.forward_failed", fields...)
				return
			}
		}
		if result != nil {
			// 鎺掗櫎 spark 褰卞瓙:鍏?codex_* 浠呯敱 QueryUsage(/wham/usage bengalfox)鏇存柊(澶栧绗?杞?P1)銆?
			if account.Type == service.AccountTypeOAuth && !account.IsShadow() {
				h.gatewayService.UpdateCodexUsageSnapshotFromHeaders(c.Request.Context(), account.ID, result.ResponseHeaders)
			}
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, account.GetMappedModel(reqModel), openAIForwardSucceededForScheduling(result), result.FirstTokenMs)
		} else {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, account.GetMappedModel(reqModel), openAIForwardSucceededForScheduling(result), nil)
		}

		// 鎹曡幏璇锋眰淇℃伅锛堢敤浜庡紓姝ヨ褰曪紝閬垮厤鍦?goroutine 涓闂?gin.Context锛?
		userAgent := c.GetHeader("User-Agent")
		clientIP := ip.GetClientIP(c)
		requestPayloadHash := service.HashUsageRequestPayload(body)
		inboundEndpoint := GetInboundEndpoint(c)
		upstreamEndpoint := resolveOpenAIUpstreamEndpoint(c, account, result)
		quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)
		imageChannel, primaryTaskID, fallbackReason := imageChannelMetadata(c)
		primaryDurationMS := imagePrimaryDurationMetadata(c)

		// 浣跨敤閲忚褰曢€氳繃鏈夌晫 worker 姹犳彁浜わ紝閬垮厤璇锋眰鐑矾寰勫垱寤烘棤鐣?goroutine銆?
		cyberBlocked := service.GetOpsCyberPolicy(c) != nil
		h.submitOpenAIUsageRecordTask(c.Request.Context(), result, func(ctx context.Context) {
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
				RequestPayloadHash: requestPayloadHash,
				APIKeyService:      h.apiKeyService,
				QuotaPlatform:      quotaPlatform,
				ImageChannel:       imageChannel,
				PrimaryTaskID:      primaryTaskID,
				PrimaryDurationMS:  primaryDurationMS,
				FallbackReason:     fallbackReason,
				FallbackDurationMS: int(forwardDurationMs),
				ChannelUsageFields: clientRequestedUsageFields(c, channelMapping, reqModel, result.UpstreamModel),
				CyberBlocked:       cyberBlocked,
			}); err != nil {
				logger.L().With(
					zap.String("component", "handler.openai_gateway.responses"),
					zap.Int64("user_id", subject.UserID),
					zap.Int64("api_key_id", apiKey.ID),
					zap.Any("group_id", apiKey.GroupID),
					zap.String("model", reqModel),
					zap.Int64("account_id", account.ID),
				).Error("openai.record_usage_failed", zap.Error(err))
			}
		})
		reqLog.Debug("openai.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
		)
		return
	}
}

func isOpenAIRemoteCompactPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	normalizedPath := strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/")
	return strings.HasSuffix(normalizedPath, "/responses/compact")
}

// isBareOpenAIResponsesPath 浠呭尮閰嶈８ /responses 绔偣锛堟棤 /compact 绛夊瓙璺緞锛夛紝
// body-signal 鎻愬崌鍙厑璁稿彂鐢熷湪杩欓噷锛岄伩鍏嶈浼?/responses/{id}/... 褰㈡€佺殑璇锋眰銆?
func isBareOpenAIResponsesPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	normalizedPath := strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/")
	return strings.HasSuffix(normalizedPath, "/responses")
}

func isOpenAIRemoteCompactionV2Request(c *gin.Context, body []byte) bool {
	stream, valid := parseOpenAICompatibleStream(body)
	if !valid || !stream || c == nil || c.Request == nil {
		return false
	}
	for _, header := range c.Request.Header.Values("x-codex-beta-features") {
		for _, feature := range strings.Split(header, ",") {
			if strings.TrimSpace(feature) == "remote_compaction_v2" {
				return true
			}
		}
	}
	return false
}

// normalizeOpenAIResponsesCompactRequest keeps Codex remote compaction v2 on
// its native streaming /responses wire and preserves the legacy body-signal
// promotion for clients that do not explicitly advertise that protocol.
// 杩斿洖褰掍竴鍖栧悗鐨?body锛沷k=false 琛ㄧず閿欒鍝嶅簲宸插啓鍑猴紝璋冪敤鏂瑰簲鐩存帴 return銆?
func (h *OpenAIGatewayHandler) normalizeOpenAIResponsesCompactRequest(c *gin.Context, reqLog *zap.Logger, body []byte) ([]byte, bool) {
	isCompactRequest := service.IsOpenAIResponsesCompactPathForTest(c)
	if !isCompactRequest && isBareOpenAIResponsesPath(c) && service.HasCompactionTriggerInInput(body) {
		if isOpenAIRemoteCompactionV2Request(c, body) {
			return body, true
		}
		c.Request.URL.Path = strings.TrimRight(c.Request.URL.Path, "/") + "/compact"
		isCompactRequest = true
		clientStream := gjson.GetBytes(body, "stream").Bool()
		if clientStream {
			service.MarkOpenAICompactClientStream(c)
		}
		reqLog.Info("codex.remote_compact.detected_body_signal", zap.Bool("client_stream", clientStream))
	}
	if !isCompactRequest {
		return body, true
	}
	if compactSeed := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); compactSeed != "" {
		c.Set(service.OpenAICompactSessionSeedKeyForTest(), compactSeed)
	}
	normalizedCompactBody, normalizedCompact, compactErr := service.NormalizeOpenAICompactRequestBodyForTest(body)
	if compactErr != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to normalize compact request body")
		return nil, false
	}
	if normalizedCompact {
		body = normalizedCompactBody
	}
	return body, true
}

func (h *OpenAIGatewayHandler) logOpenAIRemoteCompactOutcome(c *gin.Context, startedAt time.Time) {
	if !isOpenAIRemoteCompactPath(c) {
		return
	}

	var (
		ctx    = context.Background()
		path   string
		status int
	)
	if c != nil {
		if c.Request != nil {
			ctx = c.Request.Context()
			if c.Request.URL != nil {
				path = strings.TrimSpace(c.Request.URL.Path)
			}
		}
		if c.Writer != nil {
			status = c.Writer.Status()
		}
	}

	outcome := "failed"
	if status >= 200 && status < 300 {
		outcome = "succeeded"
	}
	// compact 蹇冭烦鎻愪氦鍚庡け璐ョ殑 wire 鐘舵€佺爜鍥哄寲涓?200锛岀湡瀹炵粨灞€浠ユ祦鍐呴敊璇?
	// 鏍囪涓哄噯锛坮esponse.failed 闄嶇骇璺緞浼?MarkOpsStreamError锛夈€?
	if outcome == "succeeded" && c != nil {
		if _, hasStreamErr := service.GetOpsStreamError(c); hasStreamErr {
			outcome = "failed"
		}
	}
	latencyMs := time.Since(startedAt).Milliseconds()
	if latencyMs < 0 {
		latencyMs = 0
	}

	fields := []zap.Field{
		zap.String("component", "handler.openai_gateway.responses"),
		zap.Bool("remote_compact", true),
		zap.String("compact_outcome", outcome),
		zap.Int("status_code", status),
		zap.Int64("latency_ms", latencyMs),
		zap.String("path", path),
		zap.Bool("force_codex_cli", h != nil && h.cfg != nil && h.cfg.Gateway.ForceCodexCLI),
	}

	if c != nil {
		if userAgent := strings.TrimSpace(c.GetHeader("User-Agent")); userAgent != "" {
			fields = append(fields, zap.String("request_user_agent", userAgent))
		}
		if v, ok := c.Get(opsModelKey); ok {
			if model, ok := v.(string); ok && strings.TrimSpace(model) != "" {
				fields = append(fields, zap.String("request_model", strings.TrimSpace(model)))
			}
		}
		if v, ok := c.Get(opsAccountIDKey); ok {
			if accountID, ok := v.(int64); ok && accountID > 0 {
				fields = append(fields, zap.Int64("account_id", accountID))
			}
		}
		if c.Writer != nil {
			if upstreamRequestID := strings.TrimSpace(c.Writer.Header().Get("x-request-id")); upstreamRequestID != "" {
				fields = append(fields, zap.String("upstream_request_id", upstreamRequestID))
			} else if upstreamRequestID := strings.TrimSpace(c.Writer.Header().Get("X-Request-Id")); upstreamRequestID != "" {
				fields = append(fields, zap.String("upstream_request_id", upstreamRequestID))
			}
		}
	}

	log := logger.FromContext(ctx).With(fields...)
	if outcome == "succeeded" {
		log.Info("codex.remote_compact.succeeded")
		return
	}
	log.Warn("codex.remote_compact.failed")
}

// Messages handles Anthropic Messages API requests routed to OpenAI platform.
// POST /v1/messages (when group platform is OpenAI)
func (h *OpenAIGatewayHandler) Messages(c *gin.Context) {
	streamStarted := false
	defer h.recoverAnthropicMessagesPanic(c, &streamStarted)

	requestStart := time.Now()

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.anthropicErrorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.anthropicErrorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.messages",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)

	// 妫€鏌ュ垎缁勬槸鍚﹀厑璁?/v1/messages 璋冨害
	if !allowOpenAICompatibleMessagesDispatch(apiKey) {
		h.anthropicErrorResponse(c, http.StatusForbidden, "permission_error",
			"This group does not allow /v1/messages dispatch")
		return
	}

	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.anthropicErrorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	if len(body) == 0 {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}

	if !gjson.ValidBytes(body) {
		logRequestBodyParseFailure(reqLog, body, nil)
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}

	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || modelResult.String() == "" {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	reqModel := modelResult.String()
	ensureCompositeTargetPlatform(c, apiKey, reqModel)
	if !openAICompatibleTextTargetAllowed(c, apiKey, reqModel) {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Model is not supported by this OpenAI-compatible endpoint for composite groups")
		return
	}
	routingModel := service.NormalizeOpenAICompatRequestedModel(reqModel)
	preferredMappedModel := resolveOpenAIMessagesDispatchMappedModel(apiKey, reqModel)
	reqStream := gjson.GetBytes(body, "stream").Bool()

	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream))

	setOpsRequestContext(c, reqModel, reqStream)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	if decision := h.checkSecurityAudit(c, reqLog, apiKey, subject, service.ContentModerationProtocolAnthropicMessages, reqModel, body); decision != nil && !decision.AllowNextStage {
		h.anthropicSecurityAuditError(c, decision)
		return
	}

	// 瑙ｆ瀽娓犻亾绾фā鍨嬫槧灏?
	channelMappingMsg, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
	mappedBodyForMessages := newOpenAIModelMappedBodyCache(body, h.gatewayService.ReplaceModelInBody)

	// 缁戝畾閿欒閫忎紶鏈嶅姟锛屽厑璁?service 灞傚湪闈?failover 閿欒鍦烘櫙澶嶇敤瑙勫垯銆?
	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	requestPlatform := openAICompatibleRequestPlatform(c.Request.Context(), apiKey)

	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())
	routingStart := time.Now()

	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, reqStream, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai_messages.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.anthropicStreamingAwareError(c, status, code, message, streamStarted)
		return
	}

	sessionHash := h.gatewayService.GenerateSessionHash(c, body)
	promptCacheKey := h.gatewayService.ExtractSessionID(c, body)
	sessionHash, promptCacheKey = resolveOpenAIMessagesMetadataSession(sessionHash, promptCacheKey, reqModel, body)
	if h.rejectIfCyberSessionBlocked(c, apiKey, body, reqModel, cyberBlockFormatAnthropic) {
		return
	}

	maxAccountSwitches := h.maxAccountSwitches
	switchCount := 0
	failedAccountIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	var lastFailoverErr *service.UpstreamFailoverError
	var oauth429FailoverState service.OpenAIOAuth429FailoverState
	effectiveMappedModel := preferredMappedModel

	for {
		if failoverClientGone(c) {
			return
		}
		currentRoutingModel := routingModel
		if effectiveMappedModel != "" {
			currentRoutingModel = effectiveMappedModel
		}
		reqLog.Debug("openai_messages.account_selecting", zap.Int("excluded_account_count", len(failedAccountIDs)))
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
			c.Request.Context(),
			apiKey.GroupID,
			"", // no previous_response_id
			sessionHash,
			currentRoutingModel,
			failedAccountIDs,
			service.OpenAIUpstreamTransportAny,
			service.OpenAIEndpointCapabilityChatCompletions,
			false,
			false,
			true,
			requestPlatform,
		)
		if err != nil {
			if failoverClientGone(c) {
				reqLog.Info("openai_messages.account_select_aborted_client_disconnected", zap.Error(err))
				return
			}
			reqLog.Warn("openai_messages.account_select_failed",
				zap.Error(openAICompatibleSelectionErrorForLog(err, requestPlatform)),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if len(failedAccountIDs) == 0 {
				if err != nil {
					cls := classifyOpenAICompatibleNoAccountErrorFromGin(c, h.gatewayService, apiKey, currentRoutingModel, reqModel)
					if !cls.ModelNotFound {
						markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
					}
					h.anthropicStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
					return
				}
			} else {
				if lastFailoverErr != nil {
					h.handleAnthropicFailoverExhausted(c, lastFailoverErr, streamStarted)
				} else {
					h.anthropicStreamingAwareError(c, http.StatusBadGateway, "api_error", "Upstream request failed", streamStarted)
				}
				return
			}
		}
		if selection == nil || selection.Account == nil {
			cls := classifyOpenAICompatibleNoAccountErrorFromGin(c, h.gatewayService, apiKey, currentRoutingModel, reqModel)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimited(c)
			}
			h.anthropicStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
			return
		}
		account := selection.Account
		sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
		reqLog.Debug("openai_messages.account_selected", zap.Int64("account_id", account.ID), zap.String("account_name", account.Name))
		_ = scheduleDecision
		setOpsSelectedAccount(c, account.ID, account.Platform)

		accountReleaseFunc, acquired := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)
		if !acquired {
			return
		}

		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		forwardStart := time.Now()

		defaultMappedModel := strings.TrimSpace(effectiveMappedModel)
		// 搴旂敤娓犻亾妯″瀷鏄犲皠鍒拌姹備綋
		forwardBody := mappedBodyForMessages(channelMappingMsg.Mapped, channelMappingMsg.MappedModel)
		writerSizeBeforeForward := c.Writer.Size()
		result, err := func() (*service.OpenAIForwardResult, error) {
			defer func() {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
			}()
			return h.gatewayService.ForwardAsAnthropic(c.Request.Context(), c, account, forwardBody, promptCacheKey, defaultMappedModel)
		}()
		cyberBlockKeyMsg := ""
		if service.GetOpsCyberPolicy(c) != nil {
			cyberBlockKeyMsg = service.CyberSessionBlockKey(apiKey.ID, c, body)
		}
		h.recordCyberPolicyIfMarked(c, apiKey, account, subscription, reqModel, err != nil, cyberBlockKeyMsg, clientRequestedUsageFields(c, channelMappingMsg, reqModel, ""), service.HashUsageRequestPayload(body))
		forwardDurationMs := time.Since(forwardStart).Milliseconds()
		upstreamLatencyMs, _ := getContextInt64(c, service.OpsUpstreamLatencyMsKey)
		responseLatencyMs := forwardDurationMs
		if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
			responseLatencyMs = forwardDurationMs - upstreamLatencyMs
		}
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, responseLatencyMs)
		if err == nil && result != nil && result.FirstTokenMs != nil {
			service.SetOpsLatencyMs(c, service.OpsTimeToFirstTokenMsKey, int64(*result.FirstTokenMs))
		}
		if err != nil {
			if result != nil && result.ImageCount > 0 {
				reqLog.Warn("openai_messages.forward_partial_error_with_image_result",
					zap.Int64("account_id", account.ID),
					zap.Int("image_count", result.ImageCount),
					zap.Error(err),
				)
			} else {
				var failoverErr *service.UpstreamFailoverError
				if errors.As(err, &failoverErr) {
					if failoverClientGone(c) {
						reqLog.Info("openai_messages.failover_aborted_client_disconnected",
							zap.Int64("account_id", account.ID),
							zap.Int("upstream_status", failoverErr.StatusCode),
						)
						return
					}
					if c.Writer.Size() != writerSizeBeforeForward {
						h.handleAnthropicFailoverExhausted(c, failoverErr, true)
						return
					}
					if failoverErr.ShouldReportAccountScheduleFailure() {
						h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, account.GetMappedModel(currentRoutingModel), false, nil)
					}
					if !failoverErr.ShouldRetryNextAccount() {
						h.handleAnthropicFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					// 姹犳ā寮忥細鍚岃处鍙烽噸璇?
					if failoverErr.RetryableOnSameAccount {
						retryLimit := account.GetPoolModeRetryCount()
						if sameAccountRetryCount[account.ID] < retryLimit {
							sameAccountRetryCount[account.ID]++
							reqLog.Warn("openai_messages.pool_mode_same_account_retry",
								zap.Int64("account_id", account.ID),
								zap.Int("upstream_status", failoverErr.StatusCode),
								zap.Int("retry_limit", retryLimit),
								zap.Int("retry_count", sameAccountRetryCount[account.ID]),
							)
							select {
							case <-c.Request.Context().Done():
								return
							case <-time.After(sameAccountRetryDelay):
							}
							continue
						}
					}
					h.gatewayService.RecordOpenAIAccountSwitch()
					failedAccountIDs[account.ID] = struct{}{}
					lastFailoverErr = failoverErr
					if switchCount >= maxAccountSwitches {
						h.handleAnthropicFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					switchCount++
					if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount, &oauth429FailoverState) {
						h.handleAnthropicFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					reqLog.Warn("openai_messages.upstream_failover_switching",
						zap.Int64("account_id", account.ID),
						zap.Int("upstream_status", failoverErr.StatusCode),
						zap.Int("switch_count", switchCount),
						zap.Int("max_switches", maxAccountSwitches),
					)
					continue
				}
				if result != nil && result.ClientDisconnect {
					reqLog.Info("openai_messages.client_disconnected",
						zap.Int64("account_id", account.ID),
						zap.Error(err),
					)
					return
				}
				h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, account.GetMappedModel(currentRoutingModel), false, nil)
				wroteFallback := h.ensureAnthropicErrorResponse(c, streamStarted)
				reqLog.Warn("openai_messages.forward_failed",
					zap.Int64("account_id", account.ID),
					zap.Bool("fallback_error_response_written", wroteFallback),
					zap.Error(err),
				)
				return
			}
		}
		if result != nil {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, account.GetMappedModel(currentRoutingModel), true, result.FirstTokenMs)
		} else {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, account.GetMappedModel(currentRoutingModel), true, nil)
		}

		userAgent := c.GetHeader("User-Agent")
		clientIP := ip.GetClientIP(c)
		requestPayloadHash := service.HashUsageRequestPayload(body)
		inboundEndpoint := GetInboundEndpoint(c)
		upstreamEndpoint := resolveOpenAIUpstreamEndpoint(c, account, result)
		quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)

		cyberBlocked := service.GetOpsCyberPolicy(c) != nil
		h.submitOpenAIUsageRecordTask(c.Request.Context(), result, func(ctx context.Context) {
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
				RequestPayloadHash: requestPayloadHash,
				APIKeyService:      h.apiKeyService,
				QuotaPlatform:      quotaPlatform,
				ChannelUsageFields: clientRequestedUsageFields(c, channelMappingMsg, reqModel, result.UpstreamModel),
				CyberBlocked:       cyberBlocked,
			}); err != nil {
				logger.L().With(
					zap.String("component", "handler.openai_gateway.messages"),
					zap.Int64("user_id", subject.UserID),
					zap.Int64("api_key_id", apiKey.ID),
					zap.Any("group_id", apiKey.GroupID),
					zap.String("model", reqModel),
					zap.Int64("account_id", account.ID),
				).Error("openai_messages.record_usage_failed", zap.Error(err))
			}
		})
		reqLog.Debug("openai_messages.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
		)
		return
	}
}

func resolveOpenAIMessagesMetadataSession(sessionHash, promptCacheKey, reqModel string, body []byte) (string, string) {
	// Anthropic metadata.user_id 鍙綔涓鸿处鍙风矘鎬т俊鍙枫€備笂娓?GPT/Codex 缂撳瓨閿?
	// 浜ょ粰 ForwardAsAnthropic 浠?cache_control 鎴栧畬鏁存秷鎭?digest 娲剧敓锛岄伩鍏?
	// 鍥哄畾 metadata key 鍘嬩綇鍚庣画 turn 鐨勭紦瀛樻粴鍔ㄣ€?
	if sessionHash != "" {
		return sessionHash, promptCacheKey
	}
	if userID := strings.TrimSpace(gjson.GetBytes(body, "metadata.user_id").String()); userID != "" {
		seed := reqModel + "-" + userID
		sessionHash = service.DeriveSessionHashFromSeed(seed)
	}
	return sessionHash, promptCacheKey
}

// anthropicErrorResponse writes an error in Anthropic Messages API format.
func (h *OpenAIGatewayHandler) anthropicErrorResponse(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

// anthropicStreamingAwareError handles errors that may occur during streaming,
// using Anthropic SSE error format.
func (h *OpenAIGatewayHandler) anthropicStreamingAwareError(c *gin.Context, status int, errType, message string, streamStarted bool) {
	if streamStarted {
		flusher, ok := c.Writer.(http.Flusher)
		if ok {
			errPayload, _ := json.Marshal(gin.H{
				"type": "error",
				"error": gin.H{
					"type":    errType,
					"message": message,
				},
			})
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", errPayload) //nolint:errcheck
			flusher.Flush()
		}
		return
	}
	h.anthropicErrorResponse(c, status, errType, message)
}

// handleAnthropicFailoverExhausted maps upstream failover errors to Anthropic format.
func (h *OpenAIGatewayHandler) handleAnthropicFailoverExhausted(c *gin.Context, failoverErr *service.UpstreamFailoverError, streamStarted bool) {
	if failoverErr != nil {
		copyFailoverRetryAfter(c, failoverErr.ResponseHeaders)
	}
	if failoverErr != nil && failoverErr.IsCredentialFailure() {
		status, message := credentialFailoverClientResponse(failoverErr)
		h.anthropicStreamingAwareError(c, status, "api_error", message, streamStarted)
		return
	}
	status, errType, errMsg := h.mapUpstreamError(failoverErr.StatusCode)
	h.anthropicStreamingAwareError(c, status, errType, errMsg, streamStarted)
}

// ensureAnthropicErrorResponse writes a fallback Anthropic error if no response was written.
func (h *OpenAIGatewayHandler) ensureAnthropicErrorResponse(c *gin.Context, streamStarted bool) bool {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return false
	}
	h.anthropicStreamingAwareError(c, http.StatusBadGateway, "api_error", "Upstream request failed", streamStarted)
	return true
}

func (h *OpenAIGatewayHandler) validateFunctionCallOutputRequest(c *gin.Context, body []byte, reqLog *zap.Logger) bool {
	if !gjson.GetBytes(body, `input.#(type=="function_call_output")`).Exists() {
		return true
	}

	validation := service.ValidateFunctionCallOutputContextBytes(body)
	if !validation.HasFunctionCallOutput {
		return true
	}

	previousResponseID := gjson.GetBytes(body, "previous_response_id").String()
	if strings.TrimSpace(previousResponseID) != "" || validation.HasToolCallContext {
		return true
	}

	if validation.HasFunctionCallOutputMissingCallID {
		reqLog.Warn("openai.request_validation_failed",
			zap.String("reason", "function_call_output_missing_call_id"),
		)
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "function_call_output requires call_id on HTTP requests; continuation via previous_response_id is only supported on Responses WebSocket v2")
		return false
	}
	if validation.HasItemReferenceForAllCallIDs {
		return true
	}

	reqLog.Warn("openai.request_validation_failed",
		zap.String("reason", "function_call_output_missing_item_reference"),
	)
	h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "function_call_output requires item_reference ids matching each call_id on HTTP requests; continuation via previous_response_id is only supported on Responses WebSocket v2")
	return false
}

func (h *OpenAIGatewayHandler) acquireResponsesUserSlot(
	c *gin.Context,
	userID int64,
	userConcurrency int,
	reqStream bool,
	streamStarted *bool,
	reqLog *zap.Logger,
) (func(), bool) {
	ctx := c.Request.Context()
	userReleaseFunc, err := h.concurrencyHelper.AcquireUserSlotWithWait(c, userID, userConcurrency, reqStream, streamStarted)
	if err != nil {
		reqLog.Warn("openai.user_slot_acquire_failed", zap.Error(err))
		h.handleConcurrencyError(c, err, "user", *streamStarted)
		return nil, false
	}
	return wrapReleaseOnDone(ctx, userReleaseFunc), true
}

func (h *OpenAIGatewayHandler) acquireResponsesAccountSlot(
	c *gin.Context,
	groupID *int64,
	sessionHash string,
	selection *service.AccountSelectionResult,
	reqStream bool,
	streamStarted *bool,
	reqLog *zap.Logger,
) (func(), bool) {
	if selection == nil || selection.Account == nil {
		markOpsRoutingCapacityLimited(c)
		h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available accounts", *streamStarted)
		return nil, false
	}

	ctx := c.Request.Context()
	account := selection.Account
	if selection.Acquired {
		return wrapReleaseOnDone(ctx, selection.ReleaseFunc), true
	}
	if selection.WaitPlan == nil {
		markOpsRoutingCapacityLimited(c)
		h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available accounts", *streamStarted)
		return nil, false
	}

	fastReleaseFunc, fastAcquired, err := h.concurrencyHelper.TryAcquireAccountSlot(
		ctx,
		account.ID,
		selection.WaitPlan.MaxConcurrency,
	)
	if err != nil {
		reqLog.Warn("openai.account_slot_quick_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		h.handleConcurrencyError(c, err, "account", *streamStarted)
		return nil, false
	}
	if fastAcquired {
		if err := h.gatewayService.BindStickySession(ctx, groupID, sessionHash, account.ID); err != nil {
			reqLog.Warn("openai.bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}
		return wrapReleaseOnDone(ctx, fastReleaseFunc), true
	}

	canWait, waitErr := h.concurrencyHelper.IncrementAccountWaitCount(ctx, account.ID, selection.WaitPlan.MaxWaiting)
	if waitErr != nil {
		reqLog.Warn("openai.account_wait_counter_increment_failed", zap.Int64("account_id", account.ID), zap.Error(waitErr))
	} else if !canWait {
		reqLog.Info("openai.account_wait_queue_full",
			zap.Int64("account_id", account.ID),
			zap.Int("max_waiting", selection.WaitPlan.MaxWaiting),
		)
		h.handleStreamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", "Too many pending requests, please retry later", *streamStarted)
		return nil, false
	}

	accountWaitCounted := waitErr == nil && canWait
	releaseWait := func() {
		if accountWaitCounted {
			h.concurrencyHelper.DecrementAccountWaitCount(ctx, account.ID)
			accountWaitCounted = false
		}
	}
	defer releaseWait()

	accountReleaseFunc, err := h.concurrencyHelper.AcquireAccountSlotWithWaitTimeout(
		c,
		account.ID,
		selection.WaitPlan.MaxConcurrency,
		selection.WaitPlan.Timeout,
		reqStream,
		streamStarted,
	)
	if err != nil {
		reqLog.Warn("openai.account_slot_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		h.handleConcurrencyError(c, err, "account", *streamStarted)
		return nil, false
	}

	// Slot acquired: no longer waiting in queue.
	releaseWait()
	if err := h.gatewayService.BindStickySession(ctx, groupID, sessionHash, account.ID); err != nil {
		reqLog.Warn("openai.bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(err))
	}
	return wrapReleaseOnDone(ctx, accountReleaseFunc), true
}

// ResponsesWebSocket handles OpenAI Responses API WebSocket ingress endpoint
// GET /openai/v1/responses (Upgrade: websocket)
func (h *OpenAIGatewayHandler) ResponsesWebSocket(c *gin.Context) {
	if !isOpenAIWSUpgradeRequest(c.Request) {
		h.errorResponse(c, http.StatusUpgradeRequired, "invalid_request_error", "WebSocket upgrade required (Upgrade: websocket)")
		return
	}
	setOpenAIClientTransportWS(c)

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}

	reqLog := requestLogger(
		c,
		"handler.openai_gateway.responses_ws",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
		zap.Bool("openai_ws_mode", true),
	)
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}
	reqLog.Info("openai.websocket_ingress_started")
	clientIP := ip.GetClientIP(c)
	userAgent := strings.TrimSpace(c.GetHeader("User-Agent"))
	ctx := c.Request.Context()
	maxIngressConnections := 0
	if h.cfg != nil {
		maxIngressConnections = h.cfg.Gateway.OpenAIWS.MaxIngressConnectionsPerAPIKey
	}
	ingressLease, ingressLeaseAcquired, ingressLeaseErr := h.concurrencyHelper.AcquireOpenAIWSIngressLease(ctx, apiKey.ID, maxIngressConnections)
	if ingressLeaseErr != nil {
		reqLog.Error("openai.websocket_ingress_lease_acquire_failed", zap.Error(ingressLeaseErr))
		h.errorResponse(c, http.StatusServiceUnavailable, "service_unavailable", "WebSocket ingress capacity is temporarily unavailable")
		return
	}
	if !ingressLeaseAcquired {
		reqLog.Info("openai.websocket_ingress_capacity_rejected", zap.Int("max_ingress_connections_per_api_key", maxIngressConnections))
		c.Header("Retry-After", "5")
		h.errorResponse(c, http.StatusTooManyRequests, "rate_limit_error", "Too many open WebSocket connections, please retry later")
		return
	}
	if ingressLease != nil {
		defer ingressLease.Release()
		ctx = ingressLease.Context()
		c.Request = c.Request.WithContext(ctx)
	}

	wsConn, err := coderws.Accept(c.Writer, c.Request, &coderws.AcceptOptions{
		CompressionMode: coderws.CompressionContextTakeover,
	})
	if err != nil {
		reqLog.Warn("openai.websocket_accept_failed",
			zap.Error(err),
			zap.String("client_ip", clientIP),
			zap.String("request_user_agent", userAgent),
			zap.String("upgrade_header", strings.TrimSpace(c.GetHeader("Upgrade"))),
			zap.String("connection_header", strings.TrimSpace(c.GetHeader("Connection"))),
			zap.String("sec_websocket_version", strings.TrimSpace(c.GetHeader("Sec-WebSocket-Version"))),
			zap.Bool("has_sec_websocket_key", strings.TrimSpace(c.GetHeader("Sec-WebSocket-Key")) != ""),
		)
		return
	}
	defer func() {
		_ = wsConn.CloseNow()
	}()
	wsConn.SetReadLimit(service.ResolveOpenAIWSClientReadLimitBytes(h.cfg))

	firstMessageTimeout := service.ResolveOpenAIWSClientFirstMessageTimeout(h.cfg)
	msgType, firstMessage, err := service.ReadOpenAIWSClientMessage(
		ctx,
		wsConn,
		firstMessageTimeout,
		coderws.StatusPolicyViolation,
		"missing first response.create message",
	)
	if err != nil {
		if errors.Is(context.Cause(ctx), service.ErrOpenAIWSIngressLeaseLost) {
			reqLog.Warn("openai.websocket_ingress_lease_lost_before_first_message", zap.Error(err))
			closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "websocket ingress capacity lease lost; please reconnect")
			return
		}
		closeStatus, closeReason := summarizeWSCloseErrorForLog(err)
		reqLog.Warn("openai.websocket_read_first_message_failed",
			zap.Error(err),
			zap.String("client_ip", clientIP),
			zap.String("close_status", closeStatus),
			zap.String("close_reason", closeReason),
			zap.Duration("read_timeout", firstMessageTimeout),
		)
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "missing first response.create message")
		return
	}
	if msgType != coderws.MessageText && msgType != coderws.MessageBinary {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "unsupported websocket message type")
		return
	}
	if !gjson.ValidBytes(firstMessage) {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "invalid JSON payload")
		return
	}
	reqModel := strings.TrimSpace(gjson.GetBytes(firstMessage, "model").String())
	if reqModel == "" {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "model is required in first response.create payload")
		return
	}
	ensureCompositeTargetPlatform(c, apiKey, reqModel)
	ctx = c.Request.Context()
	if apiKey.Group != nil && apiKey.Group.Platform == service.PlatformComposite {
		platform, ok := service.ResolvedTargetPlatformFromContext(ctx)
		if !ok || (platform != service.PlatformOpenAI && platform != service.PlatformGrok) {
			closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "Responses WebSocket API only supports OpenAI-compatible models for composite groups")
			return
		}
	}
	previousResponseID := strings.TrimSpace(gjson.GetBytes(firstMessage, "previous_response_id").String())
	previousResponseIDKind := service.ClassifyOpenAIPreviousResponseIDKind(previousResponseID)
	if previousResponseID != "" && previousResponseIDKind == service.OpenAIPreviousResponseIDKindMessageID {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "previous_response_id must be a response.id (resp_*), not a message id")
		return
	}
	firstMessageToolCoverage := service.AnalyzeToolCallOutputContextCoverageBytes(firstMessage)
	previousResponseCanMove := !firstMessageToolCoverage.HasFunctionCallOutput || firstMessageToolCoverage.ContextCoversAllCallIDs
	reqLog = reqLog.With(
		zap.Bool("ws_ingress", true),
		zap.String("model", reqModel),
		zap.Bool("has_previous_response_id", previousResponseID != ""),
		zap.String("previous_response_id_kind", previousResponseIDKind),
	)
	setOpsRequestContext(c, reqModel, true)
	setOpsEndpointContext(c, "", int16(service.RequestTypeWSV2))

	if decision := h.checkSecurityAuditStage(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, reqModel, firstMessage, "first_turn"); decision != nil && !decision.AllowNextStage {
		writeSecurityAuditWSError(ctx, wsConn, decision)
		closeOpenAIClientWS(wsConn, securityAuditWSCloseStatus(decision), securityAuditWSCloseReason(decision))
		return
	}

	imageIntent := service.IsExplicitImageGenerationIntent("/v1/responses", reqModel, firstMessage)
	if imageIntent && !service.GroupAllowsImageGeneration(apiKey.Group) {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, service.ImageGenerationPermissionMessage())
		return
	}

	// F5a: 鎻℃墜灞備細璇濆睆钄芥鏌ャ€俉S 鎻℃墜鏃?body锛屾樉寮忔爣璇嗕粎鏉ヨ嚜鎻℃墜 header
	// 锛坰ession_id / conversation_id锛夛紱鏃犳爣璇嗗垯鏀捐锛岃繛鎺ュ唴浠嶆湁鏈湴 flag 鍏滃簳銆?
	cyberBlockKey := service.CyberSessionBlockKey(apiKey.ID, c, nil)
	if cyberBlockKey != "" && h.gatewayService.IsCyberSessionBlocked(c.Request.Context(), cyberBlockKey) {
		writeCyberSessionBlockedWSError(c.Request.Context(), wsConn)
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "session blocked by cyber-security policy")
		h.enqueueCyberSessionBlockedOpsEntry(c, apiKey, reqModel, cyberBlockKey)
		return
	}
	cyberBlockedThisConn := false

	// 瑙ｆ瀽娓犻亾绾фā鍨嬫槧灏?
	channelMappingWS, _ := h.gatewayService.ResolveChannelMappingAndRestrict(ctx, apiKey.GroupID, reqModel)

	var currentUserRelease func()
	var currentAccountRelease func()
	releaseAccountSlot := func() {
		if currentAccountRelease != nil {
			currentAccountRelease()
			currentAccountRelease = nil
		}
	}
	releaseTurnSlots := func() {
		releaseAccountSlot()
		if currentUserRelease != nil {
			currentUserRelease()
			currentUserRelease = nil
		}
	}
	// 蹇呴』灏芥棭娉ㄥ唽锛岀‘淇濅换浣?early return 閮借兘閲婃斁宸茶幏鍙栫殑骞跺彂妲戒綅銆?
	defer releaseTurnSlots()

	userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlotForAPIKey(ctx, subject.UserID, subject.Concurrency, apiKey.ID)
	if err != nil {
		reqLog.Warn("openai.websocket_user_slot_acquire_failed", zap.Error(err))
		closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to acquire user concurrency slot")
		return
	}
	if !userAcquired {
		closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "too many concurrent requests, please retry later")
		return
	}
	currentUserRelease = wrapReleaseOnDone(ctx, userReleaseFunc)
	ensureUserSlotHeld := func() bool {
		if currentUserRelease != nil {
			return true
		}
		userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlotForAPIKey(ctx, subject.UserID, subject.Concurrency, apiKey.ID)
		if err != nil {
			reqLog.Warn("openai.websocket_user_slot_reacquire_failed", zap.Error(err))
			closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to acquire user concurrency slot")
			return false
		}
		if !userAcquired {
			closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "too many concurrent requests, please retry later")
			return false
		}
		currentUserRelease = wrapReleaseOnDone(ctx, userReleaseFunc)
		return true
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	requestPlatform := openAICompatibleRequestPlatform(ctx, apiKey)
	requiredTransport := service.OpenAIUpstreamTransportResponsesWebsocketV2Ingress
	if requestPlatform == service.PlatformGrok {
		requiredTransport = service.OpenAIUpstreamTransportHTTPSSE
	}
	if err := h.billingCacheService.CheckBillingEligibility(ctx, apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai.websocket_billing_eligibility_check_failed", zap.Error(err))
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "billing check failed")
		return
	}

	sessionHash := h.gatewayService.GenerateSessionHashWithFallback(
		c,
		firstMessage,
		openAIWSIngressFallbackSessionSeed(subject.UserID, apiKey.ID, apiKey.GroupID),
	)
	maxAccountSwitches := h.maxAccountSwitches
	switchCount := 0
	failedAccountIDs := make(map[int64]struct{})
	var lastFailoverErr *service.UpstreamFailoverError
	wsPrimaryTurnDecisions := make(map[int]wsPrimaryTurnDecision)
	var oauth429FailoverState service.OpenAIOAuth429FailoverState
	handleWSFailover := func(account *service.Account, failoverErr *service.UpstreamFailoverError) bool {
		if ctx.Err() != nil {
			return false
		}
		if failoverErr.ShouldReportAccountScheduleFailure() {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, account.GetMappedModel(reqModel), false, nil)
		}
		releaseAccountSlot()
		if !failoverErr.ShouldRetryNextAccount() {
			closeOpenAIWSFailoverExhausted(wsConn, failoverErr)
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		h.gatewayService.RecordOpenAIAccountSwitch()
		failedAccountIDs[account.ID] = struct{}{}
		lastFailoverErr = failoverErr
		if switchCount >= maxAccountSwitches {
			closeOpenAIWSFailoverExhausted(wsConn, failoverErr)
			return false
		}
		switchCount++
		if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount, &oauth429FailoverState) {
			closeOpenAIWSFailoverExhausted(wsConn, failoverErr)
			return false
		}
		reqLog.Warn("openai.websocket_upstream_failover_switching",
			zap.Int64("account_id", account.ID),
			zap.Int("upstream_status", failoverErr.StatusCode),
			zap.Int("switch_count", switchCount),
			zap.Int("max_switches", maxAccountSwitches),
		)
		if ctx.Err() != nil {
			return false
		}
		return ensureUserSlotHeld()
	}

	// 涓?HTTP Responses 璺緞淇濇寔涓€鑷达細鐢熷浘鎰忓浘璇锋眰瑕佹眰璐﹀彿鏀寔 Responses API锛?4417锛夈€?
	// WSv2 浼犺緭鏈韩宸查殣鍚?Responses 鏀寔锛屾澶勪负闃插尽鎬у榻愩€?
	// 浣跨敤 IsExplicitImageGenerationIntent 鎺掗櫎琚姩 namespace 澹版槑锛?4476锛夈€?
	requiredCapability := service.OpenAIEndpointCapabilityChatCompletions
	if service.IsExplicitImageGenerationIntent("/v1/responses", reqModel, firstMessage) && requestPlatform == service.PlatformOpenAI {
		requiredCapability = service.OpenAIEndpointCapabilityResponses
	}

	for {
		if ctx.Err() != nil {
			return
		}
		reqLog.Debug("openai.websocket_account_selecting", zap.Int("excluded_account_count", len(failedAccountIDs)))
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
			ctx,
			apiKey.GroupID,
			previousResponseID,
			sessionHash,
			reqModel,
			failedAccountIDs,
			requiredTransport,
			requiredCapability,
			false,
			previousResponseCanMove,
			!imageIntent,
			requestPlatform,
		)
		if err != nil {
			reqLog.Warn("openai.websocket_account_select_failed",
				zap.Error(openAICompatibleSelectionErrorForLog(err, requestPlatform)),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if lastFailoverErr != nil {
				closeOpenAIWSFailoverExhausted(wsConn, lastFailoverErr)
			} else {
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "no available account")
			}
			return
		}
		if selection == nil || selection.Account == nil {
			if lastFailoverErr != nil {
				closeOpenAIWSFailoverExhausted(wsConn, lastFailoverErr)
			} else {
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "no available account")
			}
			return
		}

		account := selection.Account
		accountMaxConcurrency := account.Concurrency
		if selection.WaitPlan != nil && selection.WaitPlan.MaxConcurrency > 0 {
			accountMaxConcurrency = selection.WaitPlan.MaxConcurrency
		}
		accountReleaseFunc := selection.ReleaseFunc
		if !selection.Acquired {
			if selection.WaitPlan == nil {
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "account is busy, please retry later")
				return
			}
			fastReleaseFunc, fastAcquired, err := h.concurrencyHelper.TryAcquireAccountSlot(
				ctx,
				account.ID,
				selection.WaitPlan.MaxConcurrency,
			)
			if err != nil {
				reqLog.Warn("openai.websocket_account_slot_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
				closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to acquire account concurrency slot")
				return
			}
			if !fastAcquired {
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "account is busy, please retry later")
				return
			}
			accountReleaseFunc = fastReleaseFunc
		}
		currentAccountRelease = wrapReleaseOnDone(ctx, accountReleaseFunc)
		if err := h.gatewayService.BindStickySession(ctx, apiKey.GroupID, sessionHash, account.ID); err != nil {
			reqLog.Warn("openai.websocket_bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}

		token, _, err := h.gatewayService.GetRequestCredential(ctx, c, account)
		if err != nil {
			reqLog.Warn("openai.websocket_get_access_token_failed", zap.Int64("account_id", account.ID), zap.Error(err))
			if ctx.Err() != nil {
				return
			}
			var failoverErr *service.UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				if handleWSFailover(account, failoverErr) {
					continue
				}
				return
			}
			closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to get access token")
			return
		}

		reqLog.Debug("openai.websocket_account_selected",
			zap.Int64("account_id", account.ID),
			zap.String("account_name", account.Name),
			zap.String("schedule_layer", scheduleDecision.Layer),
			zap.Int("candidate_count", scheduleDecision.CandidateCount),
		)

		maxReasoningEffort := ""
		var reasoningEffortMappings []service.ReasoningEffortMapping
		if apiKey.Group != nil && apiKey.Group.Platform == service.PlatformOpenAI {
			maxReasoningEffort = apiKey.Group.MaxReasoningEffort
			reasoningEffortMappings = apiKey.Group.ReasoningEffortMappings
		}
		var requestPayloadHash string
		hooks := &service.OpenAIWSIngressHooks{
			InitialRequestModel:     reqModel,
			MaxReasoningEffort:      maxReasoningEffort,
			ReasoningEffortMappings: reasoningEffortMappings,
			BeforeRequest: func(turn int, payload []byte, originalModel string) error {
				if turn == 1 {
					return nil
				}
				if !gjson.ValidBytes(payload) {
					return service.NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "invalid websocket request payload", errors.New("invalid json"))
				}
				model := strings.TrimSpace(originalModel)
				if model == "" {
					model = strings.TrimSpace(gjson.GetBytes(payload, "model").String())
				}
				if model == "" {
					model = reqModel
				}
				if decision := h.checkSecurityAuditStage(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, model, payload, "subsequent_turn"); decision != nil && !decision.AllowNextStage {
					writeSecurityAuditWSError(ctx, wsConn, decision)
					return service.NewOpenAIWSClientCloseError(securityAuditWSCloseStatus(decision), securityAuditWSCloseReason(decision), nil)
				}
				return nil
			},
			BeforeTurn: func(turn int) error {
				// turn==1 鐨勪細璇濆睆钄藉凡鐢辨彙鎵嬪眰妫€鏌ヨ鐩栵紱杩炴帴鍐?flag 鍙嫤鎴悗缁?turn銆?
				if cyberBlockedThisConn {
					return service.NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, cyberSessionBlockedClientMsg, nil)
				}
				if turn == 1 {
					return nil
				}
				// 闃插尽寮忔竻鐞嗭細閬垮厤寮傚父璺緞涓嬫棫妲戒綅瑕嗙洊瀵艰嚧娉勬紡銆?
				releaseTurnSlots()
				// 闈為杞?turn 闇€瑕侀噸鏂版姠鍗犲苟鍙戞Ы浣嶏紝閬垮厤闀胯繛鎺ョ┖闂插崰妲姐€?
				userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlotForAPIKey(ctx, subject.UserID, subject.Concurrency, apiKey.ID)
				if err != nil {
					return service.NewOpenAIWSClientCloseError(coderws.StatusInternalError, "failed to acquire user concurrency slot", err)
				}
				if !userAcquired {
					return service.NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "too many concurrent requests, please retry later", nil)
				}
				accountReleaseFunc, accountAcquired, err := h.concurrencyHelper.TryAcquireAccountSlot(ctx, account.ID, accountMaxConcurrency)
				if err != nil {
					if userReleaseFunc != nil {
						userReleaseFunc()
					}
					return service.NewOpenAIWSClientCloseError(coderws.StatusInternalError, "failed to acquire account concurrency slot", err)
				}
				if !accountAcquired {
					if userReleaseFunc != nil {
						userReleaseFunc()
					}
					return service.NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "account is busy, please retry later", nil)
				}
				currentUserRelease = wrapReleaseOnDone(ctx, userReleaseFunc)
				currentAccountRelease = wrapReleaseOnDone(ctx, accountReleaseFunc)
				return nil
			},
			AfterTurn: func(turn int, result *service.OpenAIForwardResult, turnErr error) {
				// F1: cyber 鏍囪鎸?turn 鐢熷懡鍛ㄦ湡娓呯悊鈥斺€攄efer 淇濊瘉浠绘剰鏃╄繑鍥炶矾寰勯兘鎵ц锛?
				// CyberBlocked 蹇呴』鍦?submit 鍓嶅悓姝ラ鎹曡幏锛坱ask 闂寘鐢?worker 姹犲紓姝ユ墽琛岋紝
				// 灞婃椂 defer 宸叉竻闄ゆ爣璁帮級銆?
				defer clearCyberPolicyTurnState(c)
				releaseTurnSlots()
				if result != nil && result.ImageChannel == "chatgpt2api_primary" {
					return
				}
				h.recordCyberPolicyIfMarked(c, apiKey, account, subscription, reqModel, turnErr != nil, cyberBlockKey, clientRequestedUsageFields(c, channelMappingWS, reqModel, ""), requestPayloadHash)
				if service.GetOpsCyberPolicy(c) != nil {
					cyberBlockedThisConn = true
				}
				if turnErr != nil {
					if result == nil || result.ImageCount <= 0 {
						return
					}
					// cyber 鍛戒腑鏃惰 turn 鐨勭敤閲忓凡鐢?recordCyberPolicyIfMarked(forwardErrored=true)
					// 鎸夌湡瀹?token 璁板綍锛岃繖閲屼笉鍐嶈蛋涓嬫柟 RecordUsage锛岄伩鍏嶅鍚屼竴 turn 鍙屽啓/鍙屾墸璐广€?
					if service.GetOpsCyberPolicy(c) != nil {
						return
					}
					reqLog.Warn("openai.websocket_partial_error_with_image_result",
						zap.Int64("account_id", account.ID),
						zap.Int("image_count", result.ImageCount),
						zap.Error(turnErr),
					)
				}
				if result == nil {
					return
				}
				// 鎺掗櫎 spark 褰卞瓙:鍏?codex_* 浠呯敱 QueryUsage(/wham/usage bengalfox)鏇存柊(澶栧绗?杞?P1)銆?
				if account.Type == service.AccountTypeOAuth && !account.IsShadow() {
					h.gatewayService.UpdateCodexUsageSnapshotFromHeaders(ctx, account.ID, result.ResponseHeaders)
				}
				h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, account.GetMappedModel(reqModel), openAIForwardSucceededForScheduling(result), result.FirstTokenMs)
				inboundEndpoint := GetInboundEndpoint(c)
				upstreamEndpoint := resolveOpenAIUpstreamEndpoint(c, account, result)
				quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)
				cyberBlocked := service.GetOpsCyberPolicy(c) != nil
				h.submitOpenAIUsageRecordTask(ctx, result, func(taskCtx context.Context) {
					if err := h.gatewayService.RecordUsage(taskCtx, &service.OpenAIRecordUsageInput{
						Result:             result,
						APIKey:             apiKey,
						User:               apiKey.User,
						Account:            account,
						Subscription:       subscription,
						InboundEndpoint:    inboundEndpoint,
						UpstreamEndpoint:   upstreamEndpoint,
						UserAgent:          userAgent,
						IPAddress:          clientIP,
						RequestPayloadHash: requestPayloadHash,
						APIKeyService:      h.apiKeyService,
						QuotaPlatform:      quotaPlatform,
						ImageChannel:       result.ImageChannel,
						PrimaryTaskID:      result.PrimaryTaskID,
						PrimaryDurationMS:  result.PrimaryDurationMS,
						FallbackReason:     result.FallbackReason,
						FallbackDurationMS: int(result.Duration.Milliseconds()),
						ChannelUsageFields: clientRequestedUsageFields(c, channelMappingWS, reqModel, result.UpstreamModel),
						CyberBlocked:       cyberBlocked,
					}); err != nil {
						reqLog.Error("openai.websocket_record_usage_failed",
							zap.Int64("account_id", account.ID),
							zap.String("request_id", result.RequestID),
							zap.Error(err),
						)
					}
				})
			},
		}
		if h.imagePrimaryEnabledForAPIKey(apiKey) {
			hooks.PrimaryTurn = func(turn int, payload []byte, originalModel string) (*service.OpenAIForwardResult, bool, error) {
				if cached, ok := wsPrimaryTurnDecisions[turn]; ok {
					return cached.result, cached.handled, cached.err
				}
				model := strings.TrimSpace(originalModel)
				if model == "" {
					model = reqModel
				}
				result, handled, routeErr := h.handleWSImagePrimaryTurn(ctx, wsImagePrimaryTurnInput{
					Payload: payload, Model: model, UserID: subject.UserID, APIKeyID: apiKey.ID,
					APIKey: apiKey, User: apiKey.User, Subscription: subscription, APIKeyService: h.apiKeyService,
					InboundEndpoint: GetInboundEndpoint(c), UserAgent: userAgent, IPAddress: clientIP,
				}, func(event []byte) error {
					writeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					defer cancel()
					return wsConn.Write(writeCtx, coderws.MessageText, event)
				})
				if handled || result != nil {
					wsPrimaryTurnDecisions[turn] = wsPrimaryTurnDecision{result: result, handled: handled, err: routeErr}
				}
				return result, handled, routeErr
			}
		}

		// 搴旂敤娓犻亾妯″瀷鏄犲皠鍒?WebSocket 棣栨潯娑堟伅
		wsFirstMessage := firstMessage
		if channelMappingWS.Mapped {
			wsFirstMessage = h.gatewayService.ReplaceModelInBody(firstMessage, channelMappingWS.MappedModel)
		}
		// 鍒囩粍/浼氳瘽澶遍厤闃叉姢锛歱revious_response_id 鏈湪褰撳墠鍒嗙粍鍛戒腑绮樿繛璐﹀彿锛圫tickyPreviousHit=false锛夛紝
		// 璇存槑璇ヤ細璇濋摼涓嶅睘浜庢湰娆¤皟搴﹀埌鐨勮处鍙凤紝鍘熸牱杞彂浼氳Е鍙戜笂娓镐細璇濋摼閴存潈澶辫触锛堚€滈壌鏉冨け璐ワ紝璇锋鏌?API Key鈥濓級銆?
		// 鏁呭墺绂婚鍖呴噷鐨?previous_response_id锛屾敼鐢ㄩ鍖呭唴 input 閲嶅缓涓婁笅鏂囷紱甯?function_call_output 鐨?
		// 宸ュ叿缁摼鏃犳硶閲嶅缓锛屼繚鎸佸師鏍枫€備粎浣滅敤浜庨杞鍖咃紝鍚庣画 turn 鐨勭画閾剧敱 WS 杞彂灞傛棦鏈夐€昏緫澶勭悊銆?
		if previousResponseID != "" && !scheduleDecision.StickyPreviousHit && previousResponseCanMove {
			wsFirstMessage = service.RemovePreviousResponseIDFromBody(wsFirstMessage)
			reqLog.Debug("openai.websocket_previous_response_id_stripped_cross_group",
				zap.Int64("account_id", account.ID),
				zap.String("schedule_layer", scheduleDecision.Layer),
			)
		}

		// WebSocket 棣栧寘鍙兘寰堝ぇ锛宧ash 蹇呴』鍦?hooks 澶栫畻鎴愬瓧绗︿覆锛岄伩鍏?AfterTurn 闂寘淇濇椿璇锋眰浣撱€?
		requestPayloadHash = service.HashUsageRequestPayload(wsFirstMessage)

		if err := h.gatewayService.ProxyResponsesWebSocketFromClient(ctx, c, wsConn, account, token, wsFirstMessage, hooks); err != nil {
			var failoverErr *service.UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				if handleWSFailover(account, failoverErr) {
					continue
				}
				return
			}

			if errors.Is(context.Cause(ctx), service.ErrOpenAIWSIngressLeaseLost) {
				reqLog.Warn("openai.websocket_ingress_lease_lost",
					zap.Int64("account_id", account.ID),
					zap.Error(err),
				)
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "websocket ingress capacity lease lost; please reconnect")
				return
			}

			var closeErr *service.OpenAIWSClientCloseError
			if errors.As(err, &closeErr) && closeErr.StatusCode() == coderws.StatusNormalClosure {
				reqLog.Info("openai.websocket_ingress_closed_normally",
					zap.Int64("account_id", account.ID),
					zap.String("reason", closeErr.Reason()),
				)
				closeOpenAIClientWS(wsConn, closeErr.StatusCode(), closeErr.Reason())
				return
			}

			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, account.GetMappedModel(reqModel), false, nil)
			closeStatus, closeReason := summarizeWSCloseErrorForLog(err)
			proxyFailedFields := []zap.Field{
				zap.Int64("account_id", account.ID),
				zap.Error(err),
				zap.String("close_status", closeStatus),
				zap.String("close_reason", closeReason),
			}
			if account.Proxy != nil {
				proxyFailedFields = append(proxyFailedFields,
					zap.Int64("proxy_id", account.Proxy.ID),
					zap.String("proxy_name", account.Proxy.Name),
					zap.String("proxy_host", account.Proxy.Host),
					zap.Int("proxy_port", account.Proxy.Port),
				)
			} else if account.ProxyID != nil {
				proxyFailedFields = append(proxyFailedFields, zap.Int64p("proxy_id", account.ProxyID))
			}
			reqLog.Warn("openai.websocket_proxy_failed", proxyFailedFields...)
			if errors.As(err, &closeErr) {
				closeOpenAIClientWS(wsConn, closeErr.StatusCode(), closeErr.Reason())
				return
			}
			closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "upstream websocket proxy failed")
			return
		}
		reqLog.Info("openai.websocket_ingress_closed", zap.Int64("account_id", account.ID))
		return
	}

}

func (h *OpenAIGatewayHandler) recoverResponsesPanic(c *gin.Context, streamStarted *bool) {
	recovered := recover()
	if recovered == nil {
		return
	}

	started := false
	if streamStarted != nil {
		started = *streamStarted
	}
	wroteFallback := h.ensureForwardErrorResponse(c, started)
	requestLogger(c, "handler.openai_gateway.responses").Error(
		"openai.responses_panic_recovered",
		zap.Bool("fallback_error_response_written", wroteFallback),
		zap.Any("panic", recovered),
		zap.ByteString("stack", debug.Stack()),
	)
}

// recoverAnthropicMessagesPanic recovers from panics in the Anthropic Messages
// handler and returns an Anthropic-formatted error response.
func (h *OpenAIGatewayHandler) recoverAnthropicMessagesPanic(c *gin.Context, streamStarted *bool) {
	recovered := recover()
	if recovered == nil {
		return
	}

	started := streamStarted != nil && *streamStarted
	requestLogger(c, "handler.openai_gateway.messages").Error(
		"openai.messages_panic_recovered",
		zap.Bool("stream_started", started),
		zap.Any("panic", recovered),
		zap.ByteString("stack", debug.Stack()),
	)
	if !started {
		h.anthropicErrorResponse(c, http.StatusInternalServerError, "api_error", "Internal server error")
	}
}

func (h *OpenAIGatewayHandler) ensureResponsesDependencies(c *gin.Context, reqLog *zap.Logger) bool {
	missing := h.missingResponsesDependencies()
	if len(missing) == 0 {
		return true
	}

	if reqLog == nil {
		reqLog = requestLogger(c, "handler.openai_gateway.responses")
	}
	reqLog.Error("openai.handler_dependencies_missing", zap.Strings("missing_dependencies", missing))

	if c != nil && c.Writer != nil && !c.Writer.Written() {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"type":    "api_error",
				"message": "Service temporarily unavailable",
			},
		})
	}
	return false
}

func (h *OpenAIGatewayHandler) missingResponsesDependencies() []string {
	missing := make([]string, 0, 5)
	if h == nil {
		return append(missing, "handler")
	}
	if h.gatewayService == nil {
		missing = append(missing, "gatewayService")
	}
	if h.billingCacheService == nil {
		missing = append(missing, "billingCacheService")
	}
	if h.apiKeyService == nil {
		missing = append(missing, "apiKeyService")
	}
	if h.concurrencyHelper == nil || h.concurrencyHelper.concurrencyService == nil {
		missing = append(missing, "concurrencyHelper")
	}
	return missing
}

func getContextInt64(c *gin.Context, key string) (int64, bool) {
	if c == nil || key == "" {
		return 0, false
	}
	v, ok := c.Get(key)
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	case float64:
		return int64(t), true
	default:
		return 0, false
	}
}

func (h *OpenAIGatewayHandler) submitUsageRecordTask(parent context.Context, task service.UsageRecordTask) {
	if task == nil {
		return
	}
	task = wrapUsageRecordTaskContext(parent, task)
	if h.usageRecordWorkerPool != nil {
		h.usageRecordWorkerPool.Submit(task)
		return
	}
	// 鍥為€€璺緞锛歸orker 姹犳湭娉ㄥ叆鏃跺悓姝ユ墽琛岋紝閬垮厤閫€鍥炲埌鏃犵晫 goroutine 妯″紡銆?
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.L().With(
				zap.String("component", "handler.openai_gateway.responses"),
				zap.Any("panic", recovered),
			).Error("openai.usage_record_task_panic_recovered")
		}
	}()
	task(ctx)
}

func (h *OpenAIGatewayHandler) submitOpenAIUsageRecordTask(parent context.Context, result *service.OpenAIForwardResult, task service.UsageRecordTask) {
	if result != nil && result.ImageCount > 0 {
		h.submitMandatoryUsageRecordTask(parent, task)
		return
	}
	h.submitUsageRecordTask(parent, task)
}

func (h *OpenAIGatewayHandler) submitMandatoryUsageRecordTask(parent context.Context, task service.UsageRecordTask) {
	if task == nil {
		return
	}
	task = wrapUsageRecordTaskContext(parent, task)
	if h.usageRecordWorkerPool != nil {
		if mode := h.usageRecordWorkerPool.Submit(task); mode != service.UsageRecordSubmitModeDropped {
			return
		}
		logger.L().With(
			zap.String("component", "handler.openai_gateway.usage"),
		).Warn("openai.usage_record_task_mandatory_sync_fallback")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.L().With(
				zap.String("component", "handler.openai_gateway.usage"),
				zap.Any("panic", recovered),
			).Error("openai.usage_record_task_panic_recovered")
		}
	}()
	task(ctx)
}

func (h *OpenAIGatewayHandler) acquireImageGenerationSlot(c *gin.Context, streamStarted bool) (func(), bool) {
	if h == nil || h.cfg == nil || h.imageLimiter == nil {
		return nil, true
	}
	imageConcurrency := h.cfg.Gateway.ImageConcurrency
	wait := strings.TrimSpace(imageConcurrency.OverflowMode) == config.ImageConcurrencyOverflowModeWait
	release, acquired := h.imageLimiter.Acquire(
		c.Request.Context(),
		imageConcurrency.Enabled,
		imageConcurrency.MaxConcurrentRequests,
		wait,
		time.Duration(imageConcurrency.WaitTimeoutSeconds)*time.Second,
		imageConcurrency.MaxWaitingRequests,
	)
	if acquired {
		return release, true
	}
	h.handleStreamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", "Image generation concurrency limit exceeded, please retry later", streamStarted)
	return nil, false
}

// handleConcurrencyError handles concurrency-related acquire errors.
func (h *OpenAIGatewayHandler) handleConcurrencyError(c *gin.Context, err error, slotType string, streamStarted bool) {
	status, errType, message := concurrencyErrorResponse(err, slotType)
	h.handleStreamingAwareError(c, status, errType, message, streamStarted)
}

func (h *OpenAIGatewayHandler) handleFailoverExhausted(c *gin.Context, failoverErr *service.UpstreamFailoverError, streamStarted bool) {
	if failoverErr == nil {
		h.handleFailoverExhaustedSimple(c, http.StatusBadGateway, streamStarted)
		return
	}
	if failoverErr.IsOpenAIRequestBodyTooLarge() {
		service.SetOpsUpstreamError(c, http.StatusRequestEntityTooLarge, service.OpenAIRequestBodyTooLargeClientMessage, "")
		h.handleStreamingAwareError(
			c,
			http.StatusRequestEntityTooLarge,
			"invalid_request_error",
			service.OpenAIRequestBodyTooLargeClientMessage,
			streamStarted,
		)
		return
	}
	copyFailoverRetryAfter(c, failoverErr.ResponseHeaders)
	if failoverErr.IsCredentialFailure() {
		status, message := credentialFailoverClientResponse(failoverErr)
		h.handleStreamingAwareError(c, status, "upstream_error", message, streamStarted)
		return
	}
	statusCode := failoverErr.StatusCode
	responseBody := failoverErr.ResponseBody
	if service.IsOpenAISilentRefusalErrorBody(responseBody) {
		service.SetOpsUpstreamError(c, statusCode, service.OpenAISilentRefusalClientMessage(), "")
		h.handleStreamingAwareError(c, http.StatusBadGateway, "upstream_error", service.OpenAISilentRefusalClientMessage(), streamStarted)
		return
	}

	// 鍏堟鏌ラ€忎紶瑙勫垯
	if h.errorPassthroughService != nil && len(responseBody) > 0 {
		if rule := h.errorPassthroughService.MatchRule("openai", statusCode, responseBody); rule != nil {
			// 纭畾鍝嶅簲鐘舵€佺爜
			respCode := statusCode
			if !rule.PassthroughCode && rule.ResponseCode != nil {
				respCode = *rule.ResponseCode
			}

			// 纭畾鍝嶅簲娑堟伅
			msg := service.ExtractUpstreamErrorMessage(responseBody)
			if !rule.PassthroughBody && rule.CustomMessage != nil {
				msg = *rule.CustomMessage
			}

			if rule.SkipMonitoring {
				c.Set(service.OpsSkipPassthroughKey, true)
			}

			h.handleStreamingAwareError(c, respCode, "upstream_error", msg, streamStarted)
			return
		}
	}

	// 璁板綍鍘熷涓婃父鐘舵€佺爜锛屼互渚?ops 閿欒鏃ュ織鎹曡幏鐪熷疄鐨勪笂娓搁敊璇?
	upstreamMsg := service.ExtractUpstreamErrorMessage(responseBody)
	service.SetOpsUpstreamError(c, statusCode, upstreamMsg, "")

	// 浣跨敤榛樿鐨勯敊璇槧灏?
	status, errType, errMsg := h.mapUpstreamError(statusCode)
	h.handleStreamingAwareError(c, status, errType, errMsg, streamStarted)
}

func credentialFailoverClientResponse(failoverErr *service.UpstreamFailoverError) (int, string) {
	_ = failoverErr
	return http.StatusServiceUnavailable, service.GrokCredentialUnavailableClientMessage
}

func copyFailoverRetryAfter(c *gin.Context, headers http.Header) {
	if c == nil || headers == nil {
		return
	}
	retryAfter := strings.TrimSpace(headers.Get("Retry-After"))
	if retryAfter == "" || len(retryAfter) > 128 || strings.ContainsAny(retryAfter, "\r\n") || !isSafeRetryAfter(retryAfter) {
		return
	}
	c.Header("Retry-After", retryAfter)
}

func isSafeRetryAfter(value string) bool {
	digitsOnly := true
	for _, char := range value {
		if char < '0' || char > '9' {
			digitsOnly = false
			break
		}
	}
	if digitsOnly {
		seconds, err := strconv.ParseUint(value, 10, 32)
		return err == nil && seconds <= uint64((7*24*time.Hour)/time.Second)
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return false
	}
	return !retryAt.After(time.Now().Add(7 * 24 * time.Hour))
}

// handleFailoverExhaustedSimple 绠€鍖栫増鏈紝鐢ㄤ簬娌℃湁鍝嶅簲浣撶殑鎯呭喌
func (h *OpenAIGatewayHandler) handleFailoverExhaustedSimple(c *gin.Context, statusCode int, streamStarted bool) {
	status, errType, errMsg := h.mapUpstreamError(statusCode)
	service.SetOpsUpstreamError(c, statusCode, errMsg, "")
	h.handleStreamingAwareError(c, status, errType, errMsg, streamStarted)
}

func (h *OpenAIGatewayHandler) mapUpstreamError(statusCode int) (int, string, string) {
	switch statusCode {
	case 401:
		return http.StatusBadGateway, "upstream_error", "Upstream authentication failed, please contact administrator"
	case 403:
		return http.StatusBadGateway, "upstream_error", "Upstream access forbidden, please contact administrator"
	case 429:
		return http.StatusTooManyRequests, "rate_limit_error", "Upstream rate limit exceeded, please retry later"
	case 529:
		return http.StatusServiceUnavailable, "upstream_error", "Upstream service overloaded, please retry later"
	case 500, 502, 503, 504:
		return http.StatusBadGateway, "upstream_error", "Upstream service temporarily unavailable"
	default:
		return http.StatusBadGateway, "upstream_error", "Upstream request failed"
	}
}

// handleStreamingAwareError handles errors that may occur after streaming has started
func (h *OpenAIGatewayHandler) handleStreamingAwareError(c *gin.Context, status int, errType, message string, streamStarted bool) {
	h.handleStreamingAwareErrorWithCode(c, status, errType, "", message, streamStarted, false)
}

func (h *OpenAIGatewayHandler) handleStreamingAwareErrorWithCode(
	c *gin.Context,
	status int,
	errType string,
	code string,
	message string,
	streamStarted bool,
	countTowardsSLA bool,
) {
	// body-signal compact 蹇冭烦鍙兘宸叉妸鍝嶅簲澶存彁浜や负 200锛氬厛鍋滃績璺筹紙寤虹珛
	// happens-before锛屾帴绠?ResponseWriter锛夛紝骞跺崌绾т负娴佸唴閿欒澶勭悊銆?
	if service.StopOpenAICompactSSEKeepaliveCommitted(c) {
		streamStarted = true
	}
	if streamStarted {
		if countTowardsSLA {
			service.MarkOpsStreamFailure(c, errType, code, message, status)
		} else {
			service.MarkOpsStreamError(c, errType, message, status)
		}
		// /v1/responses 鐨勪弗鏍?SDK锛圕odex CLI锛夎姹傜粓姝簨浠跺繀椤诲睘浜?
		// response.completed/failed/incomplete/cancelled 闆嗗悎銆?
		// 閫氱敤 `event: error` 甯т笉琚瘑鍒负缁堟浜嬩欢锛屼細瀵艰嚧
		// "stream closed before response.completed"銆?
		if inboundIsResponses(c) {
			if writeResponsesFailedSSE(c, errType, message) {
				return
			}
		}
		// Stream already started, send error as SSE event then close
		flusher, ok := c.Writer.(http.Flusher)
		if ok {
			errorObject := gin.H{"type": errType, "message": message}
			if code != "" {
				errorObject["code"] = code
			}
			payload, err := json.Marshal(gin.H{"error": errorObject})
			if err != nil {
				payload = []byte(`{"error":{"type":"upstream_error","message":"Upstream request failed"}}`)
			}
			errorEvent := "event: error\ndata: " + string(payload) + "\n\n"
			if _, err := fmt.Fprint(c.Writer, errorEvent); err != nil {
				_ = c.Error(err)
			}
			flusher.Flush()
		}
		return
	}

	// Normal case: return JSON response with proper status code
	if code == "" {
		h.errorResponse(c, status, errType, message)
		return
	}
	c.JSON(status, gin.H{"error": gin.H{
		"type": errType, "code": code, "message": message,
	}})
}

func (h *OpenAIGatewayHandler) ensureOpenAIStreamReadErrorResponse(c *gin.Context, err error, streamStarted bool) bool {
	code, message, ok := service.OpenAIUpstreamStreamReadErrorDetails(err)
	if !ok || c == nil || c.Writer == nil || service.IsResponseCommitted(c) {
		return false
	}
	if c.Writer.Written() {
		streamStarted = true
	}
	h.handleStreamingAwareErrorWithCode(
		c, http.StatusBadGateway, "upstream_error", code, message, streamStarted, true,
	)
	return true
}

// ensureForwardErrorResponse 鍦?Forward 杩斿洖閿欒浣嗗皻鏈啓鍝嶅簲鏃惰ˉ鍐欑粺涓€閿欒鍝嶅簲銆?
func (h *OpenAIGatewayHandler) ensureForwardErrorResponse(c *gin.Context, streamStarted bool) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	// 鍏堝仠 compact 蹇冭烦鍐嶈 Writer 鐘舵€侊紝閬垮厤涓庡績璺?goroutine 绔炰簤銆?
	compactKeepaliveCommitted := service.StopOpenAICompactSSEKeepaliveCommitted(c)
	if compactKeepaliveCommitted {
		streamStarted = true
	}
	imageKeepalivePresent := service.OpenAIImagesJSONKeepalivePresent(c)
	service.StopOpenAIImagesJSONKeepaliveCommitted(c)
	imageKeepalivePaddingOnly := false
	imageKeepaliveResponseWritten := false
	if imageKeepalivePresent {
		adjustedSize := service.OpenAIImagesJSONKeepaliveAdjustedWrittenSize(c)
		imageKeepalivePaddingOnly = adjustedSize < 0
		imageKeepaliveResponseWritten = adjustedSize >= 0
	}
	if service.IsResponseCommitted(c) || (!compactKeepaliveCommitted && imageKeepaliveResponseWritten) {
		return false
	}
	if c.Writer.Written() && !imageKeepalivePaddingOnly {
		streamStarted = true
	}
	h.handleStreamingAwareError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed", streamStarted)
	return true
}

func shouldLogOpenAIForwardFailureAsWarn(c *gin.Context, wroteFallback bool) bool {
	if wroteFallback {
		return false
	}
	if c == nil || c.Writer == nil {
		return false
	}
	return c.Writer.Written()
}

// openAIForwardErrorAlreadyCommunicated reports whether Forward returned an
// error after it had already written the upstream terminal error response to
// the client.
//
// This matters for Responses streams: upstream may return HTTP 200 with a
// non-retryable `response.failed` event (for example a policy/safety rejection).
// The service layer forwards that terminal event verbatim, then returns an
// error so the caller can log/account for the failed upstream response. The
// handler must not append its generic fallback `response.failed`, otherwise
// strict clients may see the useful upstream message replaced by "Upstream
// request failed" or receive duplicate terminal events.
func openAIForwardErrorAlreadyCommunicated(c *gin.Context, writerSizeBeforeForward int, err error) bool {
	if err == nil || c == nil || c.Writer == nil {
		return false
	}
	// 涓庡揩鐓у悓鍙ｅ緞锛氭帓闄?compact 蹇冭烦瀛楄妭锛岄伩鍏?浠呭績璺冲啓鍑?琚鍒や负
	// 鍝嶅簲宸插啓鍑猴紙#3887锛夈€?
	if service.OpenAICompactKeepaliveAdjustedWrittenSize(c) == writerSizeBeforeForward ||
		service.OpenAIImagesJSONKeepaliveAdjustedWrittenSize(c) == writerSizeBeforeForward {
		return false
	}

	// cyber_policy 鍛戒腑鏃朵笂娓稿師濮嬮敊璇綋宸查€忎紶缁欏鎴风锛堥潪娴佸紡 c.Data 鍐欏嚭 400 body锛?
	// 娴佸紡鍐欏嚭 response.failed 浜嬩欢锛夛紝涓嶈兘鍐嶈 ensureForwardErrorResponse 杩藉姞
	// fallback 鈥斺€?鍚﹀垯鍦ㄥ凡鍐欏嚭鐨勫畬鏁村搷搴斿熬閮ㄨ拷鍔?SSE锛坮esponses 绔偣灏鹃殢
	// response.failed銆乧hat 绔偣灏鹃殢 event:error锛夛紝姹℃煋鍝嶅簲浣撱€係ize 宸插彉鍖栬瘉鏄庡搷搴旂‘宸插啓鍑恒€?
	if service.GetOpsCyberPolicy(c) != nil {
		return true
	}

	msg := strings.TrimSpace(err.Error())
	for _, prefix := range []string{
		"upstream response failed:",
		"non-streaming openai protocol error:",
	} {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}

func openAIForwardMayFailover(c *gin.Context, writerSizeBeforeForward int, failoverErr *service.UpstreamFailoverError) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	if service.OpenAICompactKeepaliveAdjustedWrittenSize(c) == writerSizeBeforeForward {
		return true
	}
	return failoverErr != nil && failoverErr.SafeToFailoverAfterWrite
}

func openAIRequestAllowsFailoverReplay(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	return !failoverClientGone(c)
}

func openAIFirstOutputFailoverExhausted(failoverErr *service.UpstreamFailoverError, switchCount *int) bool {
	if failoverErr == nil || !failoverErr.SafeToFailoverAfterWrite || switchCount == nil {
		return false
	}
	if *switchCount >= maxOpenAIFirstOutputTimeoutSwitches {
		return true
	}
	*switchCount = *switchCount + 1
	return false
}

// errorResponse returns OpenAI API format error response
func (h *OpenAIGatewayHandler) errorResponse(c *gin.Context, status int, errType, message string) {
	// body-signal compact 蹇冭烦鍙兘宸叉妸鍝嶅簲澶存彁浜や负 200锛欽SON 閿欒浣撲細涓庡凡
	// 鎻愪氦鐨?SSE 娴佷氦閿欙紝蹇呴』闄嶇骇涓?response.failed 缁堟浜嬩欢锛?3887锛夈€?
	if service.StopOpenAICompactSSEKeepaliveCommitted(c) {
		service.MarkOpsStreamError(c, errType, message, status)
		if writeResponsesFailedSSE(c, errType, message) {
			return
		}
	}
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

// openAICompactKeepaliveInterval 澶嶇敤娴佸紡 keepalive 閰嶇疆浣滀负 compact 涓嬫父
// 蹇冭烦闂撮殧锛? 琛ㄧず绂佺敤锛堜笌娴佸紡璺緞璇箟涓€鑷达級銆?
func (h *OpenAIGatewayHandler) openAICompactKeepaliveInterval() time.Duration {
	if h.cfg == nil || h.cfg.Gateway.StreamKeepaliveInterval <= 0 {
		return 0
	}
	return time.Duration(h.cfg.Gateway.StreamKeepaliveInterval) * time.Second
}

func setOpenAIClientTransportHTTP(c *gin.Context) {
	service.SetOpenAIClientTransport(c, service.OpenAIClientTransportHTTP)
}

func setOpenAIClientTransportWS(c *gin.Context) {
	service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)
}

func ensureOpenAIPoolModeSessionHash(sessionHash string, account *service.Account) string {
	if sessionHash != "" || account == nil || !account.IsPoolMode() {
		return sessionHash
	}
	// 涓哄綋鍓嶈姹傜敓鎴愪竴娆℃€х矘鎬т細璇濋敭锛岀‘淇濆悓璐﹀彿閲嶈瘯涓嶄細閲嶆柊璐熻浇鍧囪　鍒板叾浠栬处鍙枫€?
	return "openai-pool-retry-" + uuid.NewString()
}

func openAIWSIngressFallbackSessionSeed(userID, apiKeyID int64, groupID *int64) string {
	gid := int64(0)
	if groupID != nil {
		gid = *groupID
	}
	return fmt.Sprintf("openai_ws_ingress:%d:%d:%d", gid, userID, apiKeyID)
}

func isOpenAIWSUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	return strings.Contains(strings.ToLower(strings.TrimSpace(r.Header.Get("Connection"))), "upgrade")
}

func closeOpenAIClientWS(conn *coderws.Conn, status coderws.StatusCode, reason string) {
	if conn == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 120 {
		reason = reason[:120]
	}
	_ = conn.Close(status, reason)
	_ = conn.CloseNow()
}

func closeOpenAIWSFailoverExhausted(conn *coderws.Conn, failoverErr *service.UpstreamFailoverError) {
	if failoverErr == nil {
		closeOpenAIClientWS(conn, coderws.StatusInternalError, "upstream websocket proxy failed")
		return
	}
	if failoverErr.Stage == service.GatewayFailureStageAccountAuth {
		closeOpenAIClientWS(conn, coderws.StatusTryAgainLater, service.GrokCredentialUnavailableClientMessage)
		return
	}
	switch failoverErr.StatusCode {
	case http.StatusTooManyRequests:
		closeOpenAIClientWS(conn, coderws.StatusTryAgainLater, "upstream rate limit exceeded, please retry later")
	case 529, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		closeOpenAIClientWS(conn, coderws.StatusTryAgainLater, "upstream service temporarily unavailable")
	case http.StatusUnauthorized, http.StatusForbidden:
		closeOpenAIClientWS(conn, coderws.StatusPolicyViolation, "upstream websocket authentication failed")
	default:
		closeOpenAIClientWS(conn, coderws.StatusInternalError, "upstream websocket proxy failed")
	}
}

func writeContentModerationWSError(ctx context.Context, conn *coderws.Conn, decision *service.ContentModerationDecision) {
	if conn == nil || decision == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	message := strings.TrimSpace(decision.Message)
	if message == "" {
		message = "content moderation blocked this request"
	}
	payload, err := json.Marshal(gin.H{
		"event_id": "evt_content_moderation_blocked",
		"type":     "error",
		"error": gin.H{
			"type":    "invalid_request_error",
			"code":    contentModerationErrorCode(decision),
			"message": message,
		},
	})
	if err != nil {
		payload = []byte(`{"event_id":"evt_content_moderation_blocked","type":"error","error":{"type":"invalid_request_error","code":"content_policy_violation","message":"content moderation blocked this request"}}`)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = conn.Write(writeCtx, coderws.MessageText, payload)
}

// writeCyberSessionBlockedWSError sends an error frame telling the client this
// session is blocked by the cyber session block (F5a) before closing.
func writeCyberSessionBlockedWSError(ctx context.Context, conn *coderws.Conn) {
	if conn == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := json.Marshal(gin.H{
		"event_id": "evt_cyber_session_blocked",
		"type":     "error",
		"error": gin.H{
			"type":    "permission_error",
			"code":    "session_blocked_by_cyber_policy",
			"message": cyberSessionBlockedClientMsg,
		},
	})
	if err != nil {
		payload = []byte(`{"event_id":"evt_cyber_session_blocked","type":"error","error":{"type":"permission_error","code":"session_blocked_by_cyber_policy","message":"This session is blocked by cyber-security policy, please start a new session"}}`)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = conn.Write(writeCtx, coderws.MessageText, payload)
}

// cyberPolicyRecordedKey guards against double-firing recordCyberPolicyIfMarked
// within one request (e.g. in a retry/failover loop).
const cyberPolicyRecordedKey = "ops_cyber_recorded"

// cyberPolicyOpsErrorMeta carries request-scoped fields captured outside the
// async goroutine for building the cyber ops_error_logs entry.
type cyberPolicyOpsErrorMeta struct {
	RequestID       string
	ClientRequestID string
	Platform        string
	Model           string
	RequestPath     string
	Stream          bool
	InboundEndpoint string
	UserAgent       string
	APIKeyPrefix    string
	UserID          int64
	APIKeyID        int64
	AccountID       int64
	GroupID         *int64
	ClientIP        string
	CreatedAt       time.Time
	SessionBlockKey string
}

// buildCyberPolicyOpsErrorEntry builds the ops_error_logs entry for an upstream
// cyber_policy hit. StatusCode mirrors what the codex client actually received
// (400 non-stream / 200 stream), per F6.
func buildCyberPolicyOpsErrorEntry(meta cyberPolicyOpsErrorMeta, mark *service.CyberPolicyMark) *service.OpsInsertErrorLogInput {
	rt := int16(service.RequestTypeCyberBlocked)
	entry := &service.OpsInsertErrorLogInput{
		RequestID:         meta.RequestID,
		ClientRequestID:   meta.ClientRequestID,
		Platform:          meta.Platform,
		Model:             meta.Model,
		RequestPath:       meta.RequestPath,
		Stream:            meta.Stream,
		InboundEndpoint:   meta.InboundEndpoint,
		RequestType:       &rt,
		UserAgent:         meta.UserAgent,
		APIKeyPrefix:      meta.APIKeyPrefix,
		ErrorPhase:        "request",
		ErrorType:         "cyber_policy",
		Severity:          "P3",
		StatusCode:        mark.UpstreamStatus,
		IsBusinessLimited: true,
		ErrorMessage:      "cyber_policy: " + mark.Message,
		// 鍘熷 body 鐩存帴鍏ラ槦锛沷ps service 钀藉簱鍓嶇粺涓€璧?sanitizeErrorBodyForStorage 鑴辨晱涓庢埅鏂€?
		ErrorBody:   mark.Body,
		ErrorSource: "upstream_http",
		ErrorOwner:  "provider",
		CreatedAt:   meta.CreatedAt,
	}
	if meta.UserID > 0 {
		entry.UserID = &meta.UserID
	}
	if meta.APIKeyID > 0 {
		entry.APIKeyID = &meta.APIKeyID
	}
	if meta.AccountID > 0 {
		entry.AccountID = &meta.AccountID
	}
	entry.GroupID = meta.GroupID
	if meta.ClientIP != "" {
		entry.ClientIP = &meta.ClientIP
	}
	return entry
}

// 鍙岃鍗曚覆锛氱綉鍏冲鎴风闈㈠悜涓嫳鐢ㄦ埛锛屼笖鏈敊璇棤 i18n 鍗忓晢閫氶亾銆?
const cyberSessionBlockedClientMsg = "璇ヤ細璇濆凡琚綉缁滃畨鍏ㄧ瓥鐣ュ睆钄斤紝璇峰紑鍚柊浼氳瘽 / This session is blocked by cyber-security policy, please start a new session"

// buildCyberSessionBlockedOpsEntry builds the ops_error_logs entry for a request
// rejected locally by the cyber session block (F5a). Distinct error_type from
// upstream `cyber_policy`; never feeds moderation logs / violation counting
// (the request never reached upstream 鈥?see spec).
func buildCyberSessionBlockedOpsEntry(meta cyberPolicyOpsErrorMeta) *service.OpsInsertErrorLogInput {
	rt := int16(service.RequestTypeCyberBlocked)
	entry := &service.OpsInsertErrorLogInput{
		RequestID:         meta.RequestID,
		ClientRequestID:   meta.ClientRequestID,
		Platform:          meta.Platform,
		Model:             meta.Model,
		RequestPath:       meta.RequestPath,
		Stream:            meta.Stream,
		InboundEndpoint:   meta.InboundEndpoint,
		RequestType:       &rt,
		UserAgent:         meta.UserAgent,
		APIKeyPrefix:      meta.APIKeyPrefix,
		ErrorPhase:        "request",
		ErrorType:         "cyber_policy_session_blocked",
		Severity:          "P3",
		StatusCode:        http.StatusForbidden,
		IsBusinessLimited: true,
		ErrorMessage:      "cyber_policy_session_blocked: request rejected locally by session block",
		ErrorSource:       "gateway_local",
		ErrorOwner:        "platform",
		CreatedAt:         meta.CreatedAt,
		// AccountID 鏈夋剰涓嶈锛氳姹傚湪璐﹀彿閫夋嫨鍓嶅嵆琚嫆缁濄€?
	}
	if meta.SessionBlockKey != "" {
		entry.ErrorBody = "session_block_key=" + meta.SessionBlockKey
	}
	if meta.UserID > 0 {
		entry.UserID = &meta.UserID
	}
	if meta.APIKeyID > 0 {
		entry.APIKeyID = &meta.APIKeyID
	}
	entry.GroupID = meta.GroupID
	if meta.ClientIP != "" {
		entry.ClientIP = &meta.ClientIP
	}
	return entry
}

// cyberSessionBlockFormat selects the per-endpoint error envelope for a locally
// blocked session (鐢ㄦ埛鍐崇瓥锛氬吋瀹硅矾寰勫悇鑷牸寮?.
type cyberSessionBlockFormat int

const (
	cyberBlockFormatResponses cyberSessionBlockFormat = iota
	cyberBlockFormatChat
	cyberBlockFormatAnthropic
)

// rejectIfCyberSessionBlocked checks the session-block table BEFORE account
// selection. Returns true when the request was rejected (response already
// written + ops entry enqueued). Fail-open: disabled switch / empty key /
// store error 鈫?false.
func (h *OpenAIGatewayHandler) rejectIfCyberSessionBlocked(c *gin.Context, apiKey *service.APIKey, body []byte, model string, format cyberSessionBlockFormat) bool {
	if h == nil || h.gatewayService == nil || apiKey == nil {
		return false
	}
	// 寮€鍏抽粯璁ゅ叧锛氬厛璧?~ns 绾х紦瀛樺紑鍏虫鏌ワ紝鍐嶄粯鍑?key 娲剧敓(gjson+sha256)鎴愭湰銆?
	if enabled, _ := h.gatewayService.CyberSessionBlockRuntime(c.Request.Context()); !enabled {
		return false
	}
	key := service.CyberSessionBlockKey(apiKey.ID, c, body)
	if key == "" {
		return false
	}
	if !h.gatewayService.IsCyberSessionBlocked(c.Request.Context(), key) {
		return false
	}
	// body-signal compact 蹇冭烦鍙兘宸叉妸鍝嶅簲澶存彁浜や负 200锛坈yber 妫€鏌ュ湪鐢ㄦ埛妲戒綅
	// 闀跨瓑寰呬箣鍚庢墽琛岋級锛氫互 response.failed 缁堟浜嬩欢鍥炰紶锛涙湭鎻愪氦鏃跺仠鎷嶅悗鐓у父
	// 鍐?JSON锛?3887锛夈€?
	if service.StopOpenAICompactSSEKeepaliveCommitted(c) {
		service.MarkOpsStreamError(c, "permission_error", cyberSessionBlockedClientMsg, http.StatusForbidden)
		if writeResponsesFailedSSE(c, "permission_error", cyberSessionBlockedClientMsg) {
			h.enqueueCyberSessionBlockedOpsEntry(c, apiKey, model, key)
			return true
		}
	}
	switch format {
	case cyberBlockFormatAnthropic:
		c.JSON(http.StatusForbidden, gin.H{"type": "error", "error": gin.H{
			"type":    "permission_error",
			"message": cyberSessionBlockedClientMsg,
		}})
	default: // cyberBlockFormatResponses 涓?cyberBlockFormatChat锛氬悓鏋勭殑 OpenAI error envelope
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{
			"type":    "permission_error",
			"code":    "session_blocked_by_cyber_policy",
			"message": cyberSessionBlockedClientMsg,
		}})
	}
	h.enqueueCyberSessionBlockedOpsEntry(c, apiKey, model, key)
	return true
}

// enqueueCyberSessionBlockedOpsEntry captures request meta and enqueues the
// ops_error_logs entry for a locally blocked request.
func (h *OpenAIGatewayHandler) enqueueCyberSessionBlockedOpsEntry(c *gin.Context, apiKey *service.APIKey, model string, sessionBlockKey string) {
	if h.opsService == nil {
		return
	}
	meta := cyberPolicyOpsErrorMeta{Model: model, InboundEndpoint: GetInboundEndpoint(c), CreatedAt: time.Now(), SessionBlockKey: sessionBlockKey}
	meta.RequestID = c.Writer.Header().Get("X-Request-Id")
	if c.Request != nil && c.Request.URL != nil {
		meta.RequestPath = c.Request.URL.Path
	}
	if v, ok := c.Get(opsStreamKey); ok {
		if b, ok := v.(bool); ok {
			meta.Stream = b
		}
	}
	requestCtx := context.Background()
	if c.Request != nil {
		requestCtx = c.Request.Context()
	}
	meta.Platform = resolveOpsPlatform(requestCtx, apiKey, guessPlatformFromPath(meta.RequestPath))
	if c.Request != nil {
		meta.ClientRequestID, _ = c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		meta.UserAgent = c.GetHeader("User-Agent")
		meta.ClientIP = strings.TrimSpace(ip.GetClientIP(c))
	}
	meta.APIKeyID = apiKey.ID
	meta.GroupID = apiKey.GroupID
	meta.APIKeyPrefix = keyPrefix(apiKey.Key, 8)
	if apiKey.User != nil {
		meta.UserID = apiKey.User.ID
	}
	enqueueOpsErrorLog(h.opsService, buildCyberSessionBlockedOpsEntry(meta))
}

// recordCyberPolicyIfMarked 鍦?gateway forward 杩斿洖鍚庢鏌?cyber 鏍囪锛屽紓姝ュ啓椋庢帶鏃ュ織/閭欢锛?
// 骞跺湪 forward 杩斿洖閿欒鏃跺啓涓€鏉?tokens=0 鐢ㄩ噺琛屻€傛爣璁扮敱 gateway 鏈嶅姟灞傚湪閫忎紶 cyber 鍚庤缃紱
// 褰撳墠璇锋眰宸插彂缁欑敤鎴凤紝鏈柟娉曞彧鍋氫簨鍚庤褰曪紝涓嶅奖鍝嶅搷搴斻€俧orwardErrored 涓?true 鏃舵墠鍐欑敤閲忚锛?
// 閬垮厤涓庢甯?RecordUsage(forward 鎴愬姛璺緞)閲嶅銆傛瘡璇锋眰鑷冲璁板綍涓€娆°€?
func (h *OpenAIGatewayHandler) recordCyberPolicyIfMarked(c *gin.Context, apiKey *service.APIKey, account *service.Account, subscription *service.UserSubscription, model string, forwardErrored bool, cyberBlockKey string, channelFields service.ChannelUsageFields, requestPayloadHash string) {
	mark := service.GetOpsCyberPolicy(c)
	if mark == nil {
		return
	}
	if c.GetBool(cyberPolicyRecordedKey) {
		return
	}
	c.Set(cyberPolicyRecordedKey, true)
	model = clientRequestedModel(c, model)

	requestID := c.Writer.Header().Get("X-Request-Id")
	var userID, apiKeyID int64
	var userEmail, apiKeyName, groupName string
	var groupID *int64
	if apiKey != nil {
		apiKeyID = apiKey.ID
		apiKeyName = apiKey.Name
		groupID = apiKey.GroupID
		if apiKey.User != nil {
			userID = apiKey.User.ID
			userEmail = apiKey.User.Email
		}
		if apiKey.Group != nil {
			groupName = apiKey.Group.Name
		}
	}
	inboundEndpoint := GetInboundEndpoint(c)
	upstreamEndpoint := ""
	var accountID int64
	if account != nil {
		accountID = account.ID
		upstreamEndpoint = resolveOpenAIUpstreamEndpoint(c, account, nil)
	}
	stream := false
	if v, ok := c.Get(opsStreamKey); ok {
		if b, ok := v.(bool); ok {
			stream = b
		}
	}
	cmSvc := h.contentModerationService
	gwSvc := h.gatewayService
	opsSvc := h.opsService
	apiKeySvc := h.apiKeyService
	requestPath := ""
	if c.Request != nil && c.Request.URL != nil {
		requestPath = c.Request.URL.Path
	}
	requestCtx := context.Background()
	if c.Request != nil {
		requestCtx = c.Request.Context()
	}
	platform := resolveOpsPlatform(requestCtx, apiKey, guessPlatformFromPath(requestPath))
	var clientRequestID, userAgent, clientIPStr string
	if c.Request != nil {
		clientRequestID, _ = c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		userAgent = c.GetHeader("User-Agent")
		clientIPStr = strings.TrimSpace(ip.GetClientIP(c))
	}
	apiKeyPrefix := ""
	if apiKey != nil {
		apiKeyPrefix = keyPrefix(apiKey.Key, 8)
	}
	opsMeta := cyberPolicyOpsErrorMeta{
		RequestID:       requestID,
		ClientRequestID: clientRequestID,
		Platform:        platform,
		Model:           model,
		RequestPath:     requestPath,
		Stream:          stream,
		InboundEndpoint: inboundEndpoint,
		UserAgent:       userAgent,
		APIKeyPrefix:    apiKeyPrefix,
		UserID:          userID,
		APIKeyID:        apiKeyID,
		AccountID:       accountID,
		GroupID:         groupID,
		ClientIP:        clientIPStr,
		CreatedAt:       time.Now(),
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cmSvc != nil {
			cmSvc.RecordCyberPolicyEvent(ctx, service.CyberPolicyRecordInput{
				RequestID:       requestID,
				UserID:          userID,
				UserEmail:       userEmail,
				APIKeyID:        apiKeyID,
				APIKeyName:      apiKeyName,
				GroupID:         groupID,
				GroupName:       groupName,
				Endpoint:        inboundEndpoint,
				Model:           model,
				UpstreamMessage: mark.Message,
				UpstreamBody:    mark.Body,
				UpstreamStatus:  mark.UpstreamStatus,
				UpstreamInTok:   mark.UpstreamInTok,
				UpstreamOutTok:  mark.UpstreamOutTok,
			})
		}
		if forwardErrored && gwSvc != nil {
			gwSvc.RecordCyberPolicyUsageLog(ctx, service.CyberPolicyUsageInput{
				APIKey:             apiKey,
				Account:            account,
				Subscription:       subscription,
				RequestID:          requestID,
				Model:              model,
				Stream:             stream,
				InputTokens:        mark.UpstreamInTok,
				OutputTokens:       mark.UpstreamOutTok,
				InboundEndpoint:    inboundEndpoint,
				UpstreamEndpoint:   upstreamEndpoint,
				UserAgent:          userAgent,
				IPAddress:          clientIPStr,
				RequestPayloadHash: requestPayloadHash,
				APIKeyService:      apiKeySvc,
				ChannelUsageFields: channelFields,
			})
		}
		if gwSvc != nil && cyberBlockKey != "" {
			gwSvc.MarkCyberSessionBlocked(ctx, cyberBlockKey)
		}
		if opsSvc != nil {
			enqueueOpsErrorLog(opsSvc, buildCyberPolicyOpsErrorEntry(opsMeta, mark))
		}
	}()
}

// clearCyberPolicyTurnState resets the cyber mark and the per-request recorded
// guard. WS-only: called at the END of AfterTurn, after recordCyberPolicyIfMarked
// and RecordUsage (which reads CyberBlocked) have both consumed the mark.
func clearCyberPolicyTurnState(c *gin.Context) {
	if c == nil {
		return
	}
	service.ClearOpsCyberPolicy(c)
	c.Set(cyberPolicyRecordedKey, false)
}

func summarizeWSCloseErrorForLog(err error) (string, string) {
	if err == nil {
		return "-", "-"
	}
	statusCode := coderws.CloseStatus(err)
	if statusCode == -1 {
		return "-", "-"
	}
	closeStatus := fmt.Sprintf("%d(%s)", int(statusCode), statusCode.String())
	closeReason := "-"
	var closeErr coderws.CloseError
	if errors.As(err, &closeErr) {
		reason := strings.TrimSpace(closeErr.Reason)
		if reason != "" {
			closeReason = reason
		}
	}
	return closeStatus, closeReason
}
