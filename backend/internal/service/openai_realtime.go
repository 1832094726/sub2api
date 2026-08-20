package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

const DefaultOpenAIRealtimeModel = "gpt-realtime-2.1-mini"

// ProxyOpenAIRealtime relays the public OpenAI Realtime WebSocket protocol
// byte-for-byte. Both audio buffers and control events are JSON frames, so the
// gateway only replaces credentials and does not translate event schemas.
func (s *OpenAIGatewayService) ProxyOpenAIRealtime(
	ctx context.Context,
	c *gin.Context,
	client *coderws.Conn,
	account *Account,
	token string,
	model string,
) (bool, error) {
	if s == nil || client == nil || account == nil {
		return false, errors.New("realtime service, client, and account are required")
	}
	if !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityRealtime) {
		return false, fmt.Errorf("account %d does not support OpenAI Realtime", account.ID)
	}
	target, err := s.buildOpenAIRealtimeURL(account, model)
	if err != nil {
		return false, err
	}
	headers, err := s.buildOpenAIAuthenticationHeaders(ctx, account, token)
	if err != nil {
		return false, err
	}
	if c != nil {
		if userAgent := strings.TrimSpace(c.GetHeader("User-Agent")); userAgent != "" {
			headers.Set("User-Agent", userAgent)
		}
	}
	account.ApplyHeaderOverrides(headers)

	upstream, _, _, err := s.getOpenAIWSPassthroughDialer().Dial(
		ctx,
		target,
		headers,
		resolveAccountProxyURL(account),
	)
	if err != nil {
		return false, err
	}
	defer func() { _ = upstream.Close() }()
	return relayRealtimeJSONEvents(ctx, client, upstream)
}

func (s *OpenAIGatewayService) buildOpenAIRealtimeURL(account *Account, model string) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityRealtime) {
		return "", errors.New("OpenAI API key account is required for Realtime")
	}
	base := strings.TrimSpace(account.GetOpenAIBaseURL())
	validatedBase, err := s.validateUpstreamBaseURL(base)
	if err != nil {
		return "", err
	}
	target := buildOpenAIEndpointURL(validatedBase, "/v1/realtime")
	parsed, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("invalid OpenAI Realtime URL: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	case "wss", "ws":
	default:
		return "", fmt.Errorf("unsupported Realtime URL scheme: %s", parsed.Scheme)
	}
	query := parsed.Query()
	query.Set("model", firstNonEmpty(strings.TrimSpace(model), DefaultOpenAIRealtimeModel))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
