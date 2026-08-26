package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	defaultCodexFingerprintPoolSize = 16
	minCodexFingerprintPoolSize     = 8
	maxCodexFingerprintPoolSize     = 32
	codexFingerprintPoolLeaseTTL    = 30 * time.Minute
	codexFingerprintPoolWait        = 2 * time.Second
	codexFingerprintPoolRetry       = 25 * time.Millisecond
	codexFingerprintResponseRootTTL = 24 * time.Hour
	maxCodexFingerprintLocalLeases  = 4096
	maxCodexFingerprintLocalRoots   = 4096
)

var errCodexFingerprintPoolExhausted = errors.New("Codex fingerprint session pool is busy")

// CodexFingerprintStateStore is an optional GatewayCache capability. Production
// uses Redis so several gateway replicas share leases and response lineage; unit
// tests and cache-degraded operation fall back to the process-local store below.
type CodexFingerprintStateStore interface {
	ClaimCodexFingerprintPoolSlot(ctx context.Context, scope string, candidates []int, owner string, ttl time.Duration) (slot int, acquired bool, err error)
	ReleaseCodexFingerprintPoolSlot(ctx context.Context, scope string, slot int, owner string) error
	GetCodexFingerprintResponseRoot(ctx context.Context, scope, responseID string) (string, error)
	SetCodexFingerprintResponseRoot(ctx context.Context, scope, responseID, root string, ttl time.Duration) error
}

type codexFingerprintLocalLease struct {
	owner     string
	expiresAt time.Time
}

type codexFingerprintLocalResponseRoot struct {
	root      string
	expiresAt time.Time
}

type codexFingerprintLocalState struct {
	mu        sync.Mutex
	leases    map[string]codexFingerprintLocalLease
	responses map[string]codexFingerprintLocalResponseRoot
}

func (s *codexFingerprintLocalState) claim(scope string, candidates []int, owner string, ttl time.Duration) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases == nil {
		s.leases = make(map[string]codexFingerprintLocalLease)
	}
	now := time.Now()
	if len(s.leases) >= maxCodexFingerprintLocalLeases {
		for key, lease := range s.leases {
			if !now.Before(lease.expiresAt) {
				delete(s.leases, key)
			}
		}
	}
	for _, slot := range candidates {
		key := scope + ":" + strconv.Itoa(slot)
		if _, exists := s.leases[key]; !exists && len(s.leases) >= maxCodexFingerprintLocalLeases {
			continue
		}
		lease, exists := s.leases[key]
		if exists && now.Before(lease.expiresAt) && lease.owner != owner {
			continue
		}
		s.leases[key] = codexFingerprintLocalLease{owner: owner, expiresAt: now.Add(ttl)}
		return slot, true
	}
	return -1, false
}

func (s *codexFingerprintLocalState) release(scope string, slot int, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := scope + ":" + strconv.Itoa(slot)
	if lease, ok := s.leases[key]; ok && lease.owner == owner {
		delete(s.leases, key)
	}
}

func (s *codexFingerprintLocalState) getResponseRoot(scope, responseID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.responses == nil {
		return ""
	}
	key := codexFingerprintLocalResponseKey(scope, responseID)
	value, ok := s.responses[key]
	if !ok {
		return ""
	}
	if time.Now().After(value.expiresAt) {
		delete(s.responses, key)
		return ""
	}
	return value.root
}

func (s *codexFingerprintLocalState) setResponseRoot(scope, responseID, root string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.responses == nil {
		s.responses = make(map[string]codexFingerprintLocalResponseRoot)
	}
	now := time.Now()
	if len(s.responses) >= maxCodexFingerprintLocalRoots {
		oldestKey := ""
		var oldestExpiry time.Time
		for key, value := range s.responses {
			if !now.Before(value.expiresAt) {
				delete(s.responses, key)
				continue
			}
			if oldestKey == "" || value.expiresAt.Before(oldestExpiry) {
				oldestKey, oldestExpiry = key, value.expiresAt
			}
		}
		if len(s.responses) >= maxCodexFingerprintLocalRoots && oldestKey != "" {
			delete(s.responses, oldestKey)
		}
	}
	s.responses[codexFingerprintLocalResponseKey(scope, responseID)] = codexFingerprintLocalResponseRoot{root: root, expiresAt: now.Add(ttl)}
}

func codexFingerprintLocalResponseKey(scope, responseID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(responseID)))
	return scope + ":" + hex.EncodeToString(sum[:16])
}

type codexFingerprintIngressIdentity struct {
	client                codexClientIdentity
	conversationID        string
	promptCacheKey        string
	previousResponseID    string
	clientMetadataPresent bool
	turnMetadataPresent   bool
	metadataDigest        string
}

func firstCodexIdentityValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func extractCodexFingerprintIngressIdentity(headers http.Header, body []byte) codexFingerprintIngressIdentity {
	identity := codexFingerprintIngressIdentity{}
	if headers != nil {
		identity.client = extractCodexTurnMetadataIdentity(headers)
		identity.client.sessionID = firstCodexIdentityValue(extractClientSessionID(headers), identity.client.sessionID)
		identity.client.threadID = firstCodexIdentityValue(extractClientThreadID(headers), identity.client.threadID)
		identity.client.windowID = firstCodexIdentityValue(headers.Get("x-codex-window-id"), identity.client.windowID)
		identity.client.parentThreadID = firstCodexIdentityValue(headers.Get("x-codex-parent-thread-id"), identity.client.parentThreadID)
		identity.conversationID = firstCodexIdentityValue(headers.Get("conversation-id"), headers.Get("conversation_id"))
		identity.turnMetadataPresent = strings.TrimSpace(headers.Get("x-codex-turn-metadata")) != ""
	}

	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return identity
	}
	identity.client.sessionID = firstCodexIdentityValue(identity.client.sessionID, root.Get("session_id").String(), root.Get("session-id").String())
	identity.client.threadID = firstCodexIdentityValue(identity.client.threadID, root.Get("thread_id").String(), root.Get("thread-id").String())
	identity.client.turnID = firstCodexIdentityValue(identity.client.turnID, root.Get("turn_id").String(), root.Get("turn-id").String())
	identity.client.parentThreadID = firstCodexIdentityValue(identity.client.parentThreadID, root.Get("parent_thread_id").String(), root.Get("x-codex-parent-thread-id").String())
	if embedded := strings.TrimSpace(root.Get("x-codex-turn-metadata").String()); embedded != "" {
		identity.turnMetadataPresent = true
		parsed := gjson.Parse(embedded)
		if parsed.IsObject() {
			identity.client.sessionID = firstCodexIdentityValue(identity.client.sessionID, parsed.Get("session_id").String())
			identity.client.threadID = firstCodexIdentityValue(identity.client.threadID, parsed.Get("thread_id").String())
			identity.client.turnID = firstCodexIdentityValue(identity.client.turnID, parsed.Get("turn_id").String())
			identity.client.windowID = firstCodexIdentityValue(identity.client.windowID, parsed.Get("window_id").String())
			identity.client.parentThreadID = firstCodexIdentityValue(identity.client.parentThreadID, parsed.Get("parent_thread_id").String())
		}
	}
	identity.promptCacheKey = strings.TrimSpace(root.Get("prompt_cache_key").String())
	identity.previousResponseID = strings.TrimSpace(root.Get("previous_response_id").String())
	identity.conversationID = firstCodexIdentityValue(
		identity.conversationID,
		root.Get("conversation_id").String(),
		root.Get("conversation.id").String(),
		func() string {
			value := root.Get("conversation")
			if value.Type == gjson.String {
				return value.String()
			}
			return ""
		}(),
	)

	metadata := root.Get("client_metadata")
	identity.clientMetadataPresent = metadata.Exists()
	if !metadata.IsObject() {
		return identity
	}
	identity.metadataDigest = stableCodexMetadataDigest(metadata)
	identity.client.sessionID = firstCodexIdentityValue(identity.client.sessionID, metadata.Get("session_id").String(), metadata.Get("session-id").String())
	identity.client.threadID = firstCodexIdentityValue(identity.client.threadID, metadata.Get("thread_id").String(), metadata.Get("thread-id").String())
	identity.client.turnID = firstCodexIdentityValue(identity.client.turnID, metadata.Get("turn_id").String(), metadata.Get("turn-id").String())
	identity.client.windowID = firstCodexIdentityValue(identity.client.windowID, metadata.Get("x-codex-window-id").String(), metadata.Get("window_id").String())
	identity.client.parentThreadID = firstCodexIdentityValue(identity.client.parentThreadID, metadata.Get("x-codex-parent-thread-id").String(), metadata.Get("parent_thread_id").String())
	identity.conversationID = firstCodexIdentityValue(identity.conversationID, metadata.Get("conversation_id").String())

	embedded := strings.TrimSpace(metadata.Get("x-codex-turn-metadata").String())
	if embedded != "" {
		identity.turnMetadataPresent = true
		parsed := gjson.Parse(embedded)
		if parsed.IsObject() {
			identity.client.sessionID = firstCodexIdentityValue(identity.client.sessionID, parsed.Get("session_id").String())
			identity.client.threadID = firstCodexIdentityValue(identity.client.threadID, parsed.Get("thread_id").String())
			identity.client.turnID = firstCodexIdentityValue(identity.client.turnID, parsed.Get("turn_id").String())
			identity.client.windowID = firstCodexIdentityValue(identity.client.windowID, parsed.Get("window_id").String())
			identity.client.parentThreadID = firstCodexIdentityValue(identity.client.parentThreadID, parsed.Get("parent_thread_id").String())
		}
	}
	return identity
}

func stableCodexMetadataDigest(metadata gjson.Result) string {
	if !metadata.IsObject() {
		return ""
	}
	for _, field := range []string{"installation_id", "x-codex-installation-id", "device_id", "user_id"} {
		if value := strings.TrimSpace(metadata.Get(field).String()); value != "" {
			return hashCodexConversationValue("client-metadata-"+field, value)
		}
	}
	values := map[string]any{}
	if err := json.Unmarshal([]byte(metadata.Raw), &values); err != nil {
		return hashCodexConversationValue("client-metadata", "present")
	}
	for _, field := range []string{
		"turn_id", "turn-id", "parent_turn_id", "root_turn_id",
		"turn_started_at_unix_ms", "window_id", "x-codex-window-id",
		"context_window_id", "request_id", "trace_id", "x-codex-turn-metadata",
	} {
		delete(values, field)
	}
	normalized, err := json.Marshal(values)
	if err != nil {
		return hashCodexConversationValue("client-metadata", "present")
	}
	return hashCodexConversationValue("client-metadata", string(normalized))
}

func hashCodexConversationValue(kind, raw string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + strings.TrimSpace(raw)))
	return "state:" + hex.EncodeToString(sum[:16])
}

func validCodexConversationRoot(root string, poolSize int) bool {
	if strings.HasPrefix(root, "state:") {
		value := strings.TrimPrefix(root, "state:")
		_, err := hex.DecodeString(value)
		return len(value) == 32 && err == nil
	}
	if strings.HasPrefix(root, "pool:") {
		slot, err := strconv.Atoi(strings.TrimPrefix(root, "pool:"))
		return err == nil && slot >= 0 && slot < poolSize
	}
	return false
}

func codexFingerprintPoolSize(account *Account) int {
	if account == nil || account.Extra == nil {
		return defaultCodexFingerprintPoolSize
	}
	value, configured := account.Extra[codexFingerprintPoolSizeExtraKey]
	if !configured {
		return defaultCodexFingerprintPoolSize
	}
	size := 0
	switch typed := value.(type) {
	case int:
		size = typed
	case int64:
		size = int(typed)
	case float64:
		size = int(typed)
	case json.Number:
		size, _ = strconv.Atoi(typed.String())
	case string:
		size, _ = strconv.Atoi(strings.TrimSpace(typed))
	}
	if size < minCodexFingerprintPoolSize {
		return minCodexFingerprintPoolSize
	}
	if size > maxCodexFingerprintPoolSize {
		return maxCodexFingerprintPoolSize
	}
	return size
}

func codexFingerprintConversationScope(account *Account, apiKeyID int64) string {
	namespace := codexAccountIdentityNamespace(account)
	if namespace == "" && account != nil {
		namespace = fmt.Sprintf("account:%d", account.ID)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("sub2api:codex-pool:v1:key:%d:account:%s", apiKeyID, namespace)))
	return hex.EncodeToString(sum[:12])
}

func codexFingerprintFunctionalFingerprint(body []byte) uint64 {
	root := gjson.ParseBytes(body)
	h := sha256.New()
	for _, field := range []string{"model", "instructions", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "text", "service_tier"} {
		value := root.Get(field)
		if !value.Exists() {
			continue
		}
		_, _ = h.Write([]byte(field))
		_, _ = h.Write([]byte{0})
		if normalized, err := json.Marshal(value.Value()); err == nil {
			_, _ = h.Write(normalized)
		} else {
			_, _ = h.Write([]byte(value.Raw))
		}
		_, _ = h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return binary.BigEndian.Uint64(sum[:8])
}

func codexFingerprintPoolCandidates(preferred, size int) []int {
	result := make([]int, 0, size)
	for offset := 0; offset < size; offset++ {
		result = append(result, (preferred+offset)%size)
	}
	return result
}

func (s *OpenAIGatewayService) claimCodexFingerprintSlot(ctx context.Context, scope string, candidates []int, owner string) (slot int, acquired bool, local bool) {
	if s != nil && s.cache != nil {
		if store, ok := s.cache.(CodexFingerprintStateStore); ok {
			if slot, acquired, err := store.ClaimCodexFingerprintPoolSlot(ctx, scope, candidates, owner, codexFingerprintPoolLeaseTTL); err == nil {
				return slot, acquired, false
			}
		}
	}
	if s == nil {
		return -1, false, false
	}
	slot, acquired = s.codexFingerprintLocalState.claim(scope, candidates, owner, codexFingerprintPoolLeaseTTL)
	return slot, acquired, true
}

func (s *OpenAIGatewayService) acquireCodexFingerprintSlot(ctx context.Context, scope string, candidates []int, owner string) (slot int, local bool, err error) {
	deadline := time.Now().Add(codexFingerprintPoolWait)
	for {
		if slot, acquired, local := s.claimCodexFingerprintSlot(ctx, scope, candidates, owner); acquired {
			return slot, local, nil
		}
		if time.Now().After(deadline) {
			return -1, false, errCodexFingerprintPoolExhausted
		}
		timer := time.NewTimer(codexFingerprintPoolRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return -1, false, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *OpenAIGatewayService) getCodexFingerprintResponseRoot(ctx context.Context, scope, responseID string) string {
	if s != nil && s.cache != nil {
		if store, ok := s.cache.(CodexFingerprintStateStore); ok {
			if root, err := store.GetCodexFingerprintResponseRoot(ctx, scope, responseID); err == nil && root != "" {
				return root
			}
		}
	}
	if s == nil {
		return ""
	}
	return s.codexFingerprintLocalState.getResponseRoot(scope, responseID)
}

func scopeCodexFingerprintClientIdentity(account *Account, apiKeyID int64, client codexClientIdentity) codexClientIdentity {
	rootThread := client.sessionID != "" && client.threadID == client.sessionID
	client.sessionID = scopeCodexAccountIdentityField(account, apiKeyID, "session", client.sessionID)
	if rootThread {
		client.threadID = client.sessionID
	} else {
		client.threadID = scopeCodexAccountIdentityField(account, apiKeyID, "thread", client.threadID)
	}
	client.turnID = scopeCodexAccountIdentityField(account, apiKeyID, "turn", client.turnID)
	client.windowID = scopeCodexAccountIdentityField(account, apiKeyID, "window", client.windowID)
	client.parentThreadID = scopeCodexAccountIdentityField(account, apiKeyID, "thread", client.parentThreadID)
	return client
}

func resolveCodexFingerprintConversationRootIDs(account *Account, apiKeyID int64, client codexClientIdentity, rootKey string) *codexFingerprintIDs {
	scope := codexFingerprintConversationScope(account, apiKeyID)
	client = scopeCodexFingerprintClientIdentity(account, apiKeyID, client)
	return resolveCodexFingerprintIDsWithConversationRoot(account, client, codexFingerprintSession, scope+":"+rootKey)
}

// resolveCodexFingerprintIDsForRequest implements the production session-mode
// policy: explicit state gets a dedicated stable root, while identity-free API
// calls share a bounded leased pool. previous_response_id is only a lookup key;
// it is never fabricated or rewritten.
func (s *OpenAIGatewayService) resolveCodexFingerprintIDsForRequest(ctx context.Context, c *gin.Context, account *Account, headers http.Header, ingressBody []byte) (*codexFingerprintIDs, error) {
	if account == nil {
		return nil, nil
	}
	mode := account.GetCodexFingerprintMode()
	if mode != codexFingerprintSession {
		return resolveCodexFingerprintIDsFromRequest(account, headers), nil
	}
	if _, ok := codexFingerprintSeed(account.Extra); !ok {
		return nil, nil
	}
	identity := extractCodexFingerprintIngressIdentity(headers, ingressBody)
	apiKeyID := getAPIKeyIDFromContext(c)
	scope := codexFingerprintConversationScope(account, apiKeyID)
	poolSize := codexFingerprintPoolSize(account)

	rootKey := ""
	rootSource := ""
	mappedPreviousRoot := ""
	if identity.previousResponseID != "" {
		mappedPreviousRoot = s.getCodexFingerprintResponseRoot(ctx, scope, identity.previousResponseID)
		if !validCodexConversationRoot(mappedPreviousRoot, poolSize) {
			mappedPreviousRoot = ""
		}
	}
	switch {
	case mappedPreviousRoot != "":
		rootKey, rootSource = mappedPreviousRoot, "previous-response"
	case identity.client.sessionID != "":
		rootKey, rootSource = hashCodexConversationValue("session", identity.client.sessionID), "session"
	case identity.conversationID != "":
		rootKey, rootSource = hashCodexConversationValue("conversation", identity.conversationID), "conversation"
	case identity.previousResponseID != "":
		rootKey, rootSource = hashCodexConversationValue("previous-response", identity.previousResponseID), "previous-response"
	case identity.client.parentThreadID != "":
		rootKey, rootSource = hashCodexConversationValue("parent-thread", identity.client.parentThreadID), "parent-thread"
	case identity.promptCacheKey != "":
		rootKey, rootSource = hashCodexConversationValue("prompt-cache", identity.promptCacheKey), "prompt-cache"
	case identity.client.threadID != "":
		rootKey, rootSource = hashCodexConversationValue("thread", identity.client.threadID), "thread"
	case identity.clientMetadataPresent || identity.turnMetadataPresent:
		digest := identity.metadataDigest
		if digest == "" {
			digest = hashCodexConversationValue("metadata", "present")
		}
		rootKey, rootSource = digest, "metadata"
	}

	owner := uuid.NewString()
	poolSlot := -1
	poolLeaseLocal := false
	if rootKey == "" {
		preferred := int(codexFingerprintFunctionalFingerprint(ingressBody) % uint64(poolSize))
		slot, local, err := s.acquireCodexFingerprintSlot(ctx, scope, codexFingerprintPoolCandidates(preferred, poolSize), owner)
		if err != nil {
			return nil, err
		}
		poolSlot = slot
		poolLeaseLocal = local
		rootKey = "pool:" + strconv.Itoa(slot)
		rootSource = "pool"
	} else if strings.HasPrefix(rootKey, "pool:") {
		slot, _ := strconv.Atoi(strings.TrimPrefix(rootKey, "pool:"))
		acquired, local, err := s.acquireCodexFingerprintSlot(ctx, scope, []int{slot}, owner)
		if err != nil {
			return nil, err
		}
		poolSlot = acquired
		poolLeaseLocal = local
	}

	ids := resolveCodexFingerprintConversationRootIDs(account, apiKeyID, identity.client, rootKey)
	if ids == nil {
		if poolSlot >= 0 {
			s.releaseCodexFingerprintLease(&codexFingerprintIDs{poolScope: scope, poolSlot: poolSlot, poolLeaseToken: owner, poolLeaseLocal: poolLeaseLocal})
		}
		return nil, nil
	}
	ids.poolScope = scope
	ids.poolSlot = poolSlot
	if poolSlot >= 0 {
		ids.poolLeaseToken = owner
		ids.poolLeaseLocal = poolLeaseLocal
	}
	if rootSource == "prompt-cache" {
		ids.rootPromptCacheKey = scopeCodexPromptCacheKey(account, apiKeyID, identity.promptCacheKey, identity.client.sessionID, identity.client.parentThreadID)
	}
	return ids, nil
}

func (s *OpenAIGatewayService) releaseCodexFingerprintLease(ids *codexFingerprintIDs) {
	if s == nil || ids == nil || ids.poolSlot < 0 || ids.poolScope == "" || ids.poolLeaseToken == "" {
		return
	}
	if ids.poolLeaseLocal {
		s.codexFingerprintLocalState.release(ids.poolScope, ids.poolSlot, ids.poolLeaseToken)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if s.cache != nil {
		if store, ok := s.cache.(CodexFingerprintStateStore); ok {
			_ = store.ReleaseCodexFingerprintPoolSlot(ctx, ids.poolScope, ids.poolSlot, ids.poolLeaseToken)
		}
	}
}

func (s *OpenAIGatewayService) bindCodexFingerprintResponseRoot(_ context.Context, c *gin.Context, account *Account, responseID string) {
	responseID = strings.TrimSpace(responseID)
	ids := stagedCodexFingerprintIDs(c, account)
	if s == nil || ids == nil || responseID == "" || ids.poolScope == "" || ids.conversationRootKey == "" {
		return
	}
	root := strings.TrimPrefix(ids.conversationRootKey, ids.poolScope+":")
	if !validCodexConversationRoot(root, codexFingerprintPoolSize(account)) {
		return
	}
	stored := false
	storeCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if s.cache != nil {
		if store, ok := s.cache.(CodexFingerprintStateStore); ok {
			stored = store.SetCodexFingerprintResponseRoot(storeCtx, ids.poolScope, responseID, root, codexFingerprintResponseRootTTL) == nil
		}
	}
	if !stored {
		s.codexFingerprintLocalState.setResponseRoot(ids.poolScope, responseID, root, codexFingerprintResponseRootTTL)
	}
}
