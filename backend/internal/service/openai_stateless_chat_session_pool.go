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
	// Implicit Chat Completions clients do not provide a conversation signal.
	// Keeping a small deterministic pool prevents both single-turn and full-
	// history multi-turn prompts from creating unbounded upstream roots while
	// retaining enough lanes for concurrency. The legacy prefix is intentionally
	// retained so existing pooled roots continue to be reused after upgrades.
	openAIStatelessChatSessionPoolSize = uint64(8)
	openAIStatelessChatSessionPrefix   = "compat_stateless_cc_"
)

// ResolveChatCompletionsSession resolves both the scheduler hash and the
// upstream prompt/session key for /v1/chat/completions.
//
// Explicit conversation signals keep the existing behavior. Any genuine Chat
// Completions request without such a signal enters the bounded pool, including
// multi-turn clients that resend their full message history on every request.
func (s *OpenAIGatewayService) ResolveChatCompletionsSession(c *gin.Context, body []byte, apiKeyID int64) (sessionHash, promptCacheKey string) {
	explicitRequestID := explicitOpenAIRequestSessionID(c, body)
	if explicitRequestID != "" {
		currentHash, legacyHash := deriveOpenAISessionHashes(explicitRequestID)
		attachOpenAILegacySessionHashToGin(c, legacyHash)
		// x-grok-conv-id participates in scheduling only. Keep the pre-existing
		// behavior of not forwarding it as an OpenAI prompt_cache_key.
		return currentHash, explicitOpenAISessionID(c, body)
	}

	if apiKeyID > 0 && isPoolableOpenAIChatCompletionsRequest(body) {
		promptCacheKey = deriveOpenAIImplicitChatSessionSeed(c, body, apiKeyID)
		if promptCacheKey != "" {
			currentHash, legacyHash := deriveOpenAISessionHashes(promptCacheKey)
			attachOpenAILegacySessionHashToGin(c, legacyHash)
			return currentHash, promptCacheKey
		}
	}

	return s.GenerateSessionHash(c, body), ""
}

func isPoolableOpenAIChatCompletionsRequest(body []byte) bool {
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

	return firstOpenAIChatUserLaneAnchor(body) != ""
}

func firstOpenAIChatUserLaneAnchor(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return ""
	}

	anchor := ""
	messages.ForEach(func(_, message gjson.Result) bool {
		if !strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "user") {
			return true
		}
		content := message.Get("content")
		if hasMeaningfulOpenAIContent(content) {
			// The first user message is resent unchanged by full-history Chat
			// Completions clients. Hash only this raw content so later assistant,
			// follow-up and system-prompt changes stay on the same bounded lane.
			anchor = content.Raw
		}
		return false
	})
	return anchor
}

func deriveOpenAIImplicitChatSessionSeed(c *gin.Context, body []byte, apiKeyID int64) string {
	if apiKeyID <= 0 || openAIStatelessChatSessionPoolSize == 0 {
		return ""
	}
	laneAnchor := firstOpenAIChatUserLaneAnchor(body)
	if laneAnchor == "" {
		return ""
	}

	model := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "model").String()))
	clientScope := openAIStatelessChatClientScope(c)
	identityHash := sha256.Sum256([]byte(fmt.Sprintf("api_key=%d|client=%s|model=%s", apiKeyID, clientScope, model)))
	anchorHash := sha256.Sum256([]byte(laneAnchor))
	slot := binary.BigEndian.Uint64(anchorHash[:8]) % openAIStatelessChatSessionPoolSize

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
