package service

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	// Stateless Chat Completions clients do not provide a conversation signal.
	// Keeping a small deterministic pool prevents every dynamic prompt from
	// creating a new upstream root while retaining enough lanes for concurrency.
	openAIStatelessChatSessionPoolSize = uint64(8)
	openAIStatelessChatSessionPrefix   = "compat_stateless_cc_"
)

// ResolveChatCompletionsSession resolves both the scheduler hash and the
// upstream prompt/session key for /v1/chat/completions.
//
// Explicit conversation signals and multi-turn requests keep the existing
// behavior. Only single-turn, history-free requests without an explicit
// signal enter the bounded pool.
func (s *OpenAIGatewayService) ResolveChatCompletionsSession(c *gin.Context, body []byte, apiKeyID int64) (sessionHash, promptCacheKey string) {
	explicitRequestID := explicitOpenAIRequestSessionID(c, body)
	if explicitRequestID != "" {
		currentHash, legacyHash := deriveOpenAISessionHashes(explicitRequestID)
		attachOpenAILegacySessionHashToGin(c, legacyHash)
		// x-grok-conv-id participates in scheduling only. Keep the pre-existing
		// behavior of not forwarding it as an OpenAI prompt_cache_key.
		return currentHash, explicitOpenAISessionID(c, body)
	}

	if apiKeyID > 0 && isStatelessOpenAIChatCompletionsRequest(body) {
		promptCacheKey = deriveOpenAIStatelessChatSessionSeed(c, body, apiKeyID)
		if promptCacheKey != "" {
			currentHash, legacyHash := deriveOpenAISessionHashes(promptCacheKey)
			attachOpenAILegacySessionHashToGin(c, legacyHash)
			return currentHash, promptCacheKey
		}
	}

	return s.GenerateSessionHash(c, body), ""
}

func isStatelessOpenAIChatCompletionsRequest(body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" {
		return false
	}
	if conversation := gjson.GetBytes(body, "conversation"); conversation.Exists() && conversation.Raw != "null" && strings.TrimSpace(conversation.String()) != "" {
		return false
	}
	// A Responses-shaped body sent to the Chat Completions URL retains its own
	// Responses session semantics and must not be classified as stateless Chat.
	if gjson.GetBytes(body, "input").Exists() {
		return false
	}

	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() || len(messages.Array()) == 0 {
		return false
	}

	userCount := 0
	hasHistory := false
	messages.ForEach(func(_, message gjson.Result) bool {
		switch strings.ToLower(strings.TrimSpace(message.Get("role").String())) {
		case "system", "developer":
			// Stable instructions do not make a request stateful.
		case "user":
			userCount++
		default:
			// assistant/tool/function and unknown roles imply conversation history.
			hasHistory = true
		}
		return !hasHistory
	})

	return !hasHistory && userCount == 1
}

func deriveOpenAIStatelessChatSessionSeed(c *gin.Context, body []byte, apiKeyID int64) string {
	if apiKeyID <= 0 || openAIStatelessChatSessionPoolSize == 0 {
		return ""
	}

	model := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "model").String()))
	clientScope := openAIStatelessChatClientScope(c)
	identityHash := sha256.Sum256([]byte(fmt.Sprintf("api_key=%d|client=%s|model=%s", apiKeyID, clientScope, model)))
	requestHash := sha256.Sum256(body)
	slot := binary.BigEndian.Uint64(requestHash[:8]) % openAIStatelessChatSessionPoolSize

	return fmt.Sprintf("%s%x_%02d", openAIStatelessChatSessionPrefix, identityHash[:12], slot)
}

func openAIStatelessChatClientScope(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return "unknown"
	}

	if rawOrigin := strings.TrimSpace(c.GetHeader("Origin")); rawOrigin != "" {
		if parsed, err := url.Parse(rawOrigin); err == nil {
			scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
			if scheme == "chrome-extension" || scheme == "moz-extension" || scheme == "ms-browser-extension" {
				return scheme + ":" + strings.ToLower(strings.TrimSpace(parsed.Host))
			}
		}
	}

	ua := strings.ToLower(c.GetHeader("User-Agent"))
	switch {
	case strings.Contains(ua, "edg/"):
		return "edge"
	case strings.Contains(ua, "opr/") || strings.Contains(ua, "opera"):
		return "opera"
	case strings.Contains(ua, "chrome/") || strings.Contains(ua, "chromium/"):
		return "chrome"
	case strings.Contains(ua, "firefox/"):
		return "firefox"
	case strings.Contains(ua, "safari/"):
		return "safari"
	case strings.Contains(ua, "curl/"):
		return "curl"
	case strings.Contains(ua, "python"):
		return "python"
	case strings.Contains(ua, "node"):
		return "node"
	default:
		return "other"
	}
}
