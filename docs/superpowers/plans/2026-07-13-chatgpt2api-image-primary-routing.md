# ChatGPT2API Image Primary Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route every OpenAI/Codex image-generation request through ChatGPT2API first, fall back to native `gpt-image-2` only after a conclusive failure, bill exactly once with existing image prices, and expose the actual channel and task ID in the admin usage log.

**Architecture:** Add a protocol-neutral primary-image adapter before native account selection, backed by a persistent `image_primary_tasks` state machine. The adapter owns idempotency, polling and safe fallback decisions; existing native account forwarding remains unchanged and is called only for `fallback_allowed`. Primary-channel usage is settled through a dedicated account-free billing path, while the same pricing resolver and atomic usage billing repository preserve current prices and exactly-once deductions.

**Tech Stack:** Go 1.x, Gin, Ent/PostgreSQL, Wire, Viper, Vue 3/TypeScript, Vitest, Docker Compose, Python/FastAPI-compatible ChatGPT2API contract.

## Global Constraints

- Primary timeout is exactly `300` seconds and poll interval is exactly `5` seconds by default.
- Primary task states are only `queued`, `running`, `success`, and `error`.
- Never fall back while a primary task exists in `queued/running` or creation is uncertain.
- Never log API keys, full prompts, image Base64, account credentials, or ChatGPT2API internal task IDs.
- Main-channel and fallback success use the existing Sub2API image pricing table and produce exactly one usage/billing record.
- Gemini, Antigravity, Grok and `/v1/images/batches` retain their current routing.
- `CHATGPT2API_IMAGE_PRIMARY_ENABLED` defaults to `false`; deployment may enable it only after all contract tests pass.
- The empty repository `1832094726/chatgpt2api` is not a usable source baseline. Do not edit a running container in place; import a reviewed source baseline into that fork before any ChatGPT2API-side change.

## File Map

- `backend/internal/config/config.go`: top-level `chatgpt2api_image` configuration, defaults and validation.
- `backend/internal/service/image_primary.go`: stable task/result types, fallback decision enum and repository/client ports.
- `backend/internal/service/chatgpt2api_image_client.go`: HTTP, SSE and task polling client; secret-safe errors.
- `backend/internal/service/image_primary_router.go`: state machine, idempotent submit/query and conclusive fallback policy.
- `backend/ent/schema/image_primary_task.go`: persistent task ownership and settlement state.
- `backend/internal/repository/image_primary_task_repo.go`: Ent-backed compare-and-set task repository.
- `backend/migrations/100_add_image_primary_routing.sql`: task table, usage-channel columns and nullable primary-channel account attribution.
- `backend/internal/handler/openai_image_primary.go`: shared handler orchestration and authenticated task query.
- `backend/internal/handler/openai_images.go`: Images/Edits primary attempt before native account selection.
- `backend/internal/handler/openai_gateway_handler.go`: OpenAI Responses HTTP/SSE and WebSocket primary routing.
- `backend/internal/handler/openai_chat_completions.go`: image-intent Chat Completions primary routing.
- `backend/internal/service/openai_gateway_usage.go`: account-free primary settlement using existing price resolution.
- `backend/internal/service/usage_log.go`, `backend/ent/schema/usage_log.go`, `backend/internal/repository/usage_log_repo_*.go`: channel/task log fields and nullable account relation.
- `backend/internal/handler/dto/types.go`, `backend/internal/handler/dto/mappers.go`: admin usage DTO fields.
- `backend/internal/server/routes/gateway.go`: authenticated `GET /v1/images/tasks/:task_id` aliases.
- `frontend/src/components/admin/usage/UsageTable.vue`: channel and task ID display.
- `frontend/src/i18n/locales/{zh,en}/dashboard.ts`: labels.
- `/Users/hechengjun.9/Documents/conference-latex-template/deployments/chatgpt2api-aliyun/docker-compose.yml` and `deploy/docker-compose.yml`: private service address and secrets.

---

### Task 1: Configuration and Primary Client Contract

**Files:**
- Modify: `backend/internal/config/config.go`
- Create: `backend/internal/service/image_primary.go`
- Create: `backend/internal/service/chatgpt2api_image_client.go`
- Test: `backend/internal/config/config_test.go`
- Test: `backend/internal/service/chatgpt2api_image_client_test.go`

**Interfaces:**
- Produces: `ImagePrimaryConfig`, `ImagePrimaryClient`, `ImagePrimarySubmit`, `ImagePrimarySnapshot`, `ImagePrimaryRouteResult`.
- Consumes: standard `http.Client`, Viper environment mapping and existing OpenAI response structures.

- [ ] **Step 1: Write failing config and client tests**

```go
func TestImagePrimaryDefaults(t *testing.T) {
    cfg := loadConfigForTest(t, nil)
    require.False(t, cfg.ChatGPT2APIImage.PrimaryEnabled)
    require.Equal(t, 300, cfg.ChatGPT2APIImage.TimeoutSeconds)
    require.Equal(t, 5, cfg.ChatGPT2APIImage.PollIntervalSeconds)
}

func TestChatGPT2APIClientRedactsAuthorization(t *testing.T) {
    client := NewChatGPT2APIImageClient(ChatGPT2APIImageClientConfig{
        BaseURL: server.URL, APIKey: "secret-value", HTTPClient: server.Client(),
    })
    _, err := client.GetTask(context.Background(), "task-1")
    require.Error(t, err)
    require.NotContains(t, err.Error(), "secret-value")
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `cd backend && go test ./internal/config ./internal/service -run 'TestImagePrimaryDefaults|TestChatGPT2APIClient' -count=1`

Expected: FAIL because `Gateway.ImagePrimary` and `NewChatGPT2APIImageClient` do not exist.

- [ ] **Step 3: Add exact configuration and ports**

```go
type ImagePrimaryConfig struct {
    PrimaryEnabled      bool   `mapstructure:"primary_enabled"`
    BaseURL             string `mapstructure:"base_url"`
    APIKey              string `mapstructure:"api_key"`
    TimeoutSeconds      int    `mapstructure:"timeout_seconds"`
    PollIntervalSeconds int    `mapstructure:"poll_interval_seconds"`
}

type ImagePrimaryClient interface {
    SubmitImages(context.Context, *ImagePrimarySubmit) (*ImagePrimarySnapshot, error)
    SubmitResponses(context.Context, *ImagePrimarySubmit) (*ImagePrimarySnapshot, error)
    GetTask(context.Context, string) (*ImagePrimarySnapshot, error)
}
```

Add `ChatGPT2APIImage ImagePrimaryConfig` to the root `Config` with `mapstructure:"chatgpt2api_image"`. Set defaults under `chatgpt2api_image.*`, producing the exact environment names `CHATGPT2API_IMAGE_PRIMARY_ENABLED`, `CHATGPT2API_IMAGE_BASE_URL`, `CHATGPT2API_IMAGE_API_KEY`, `CHATGPT2API_IMAGE_TIMEOUT_SECONDS`, and `CHATGPT2API_IMAGE_POLL_INTERVAL_SECONDS`; validate positive timeout/poll values and require an absolute HTTP(S) `base_url` plus non-empty `api_key` only when enabled. Implement request headers with `Authorization: Bearer <key>`, bounded response reads and sanitized errors.

- [ ] **Step 4: Run focused tests**

Run: `cd backend && go test ./internal/config ./internal/service -run 'ImagePrimary|ChatGPT2API' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/config/config.go backend/internal/config/config_test.go backend/internal/service/image_primary.go backend/internal/service/chatgpt2api_image_client.go backend/internal/service/chatgpt2api_image_client_test.go
git commit -m "feat: add chatgpt2api image client contract"
```

### Task 2: Persistent Idempotent Task State

**Files:**
- Create: `backend/ent/schema/image_primary_task.go`
- Create: `backend/internal/repository/image_primary_task_repo.go`
- Modify: `backend/internal/repository/wire.go`
- Modify generated files under: `backend/ent/`
- Create: `backend/migrations/100_add_image_primary_routing.sql`
- Test: `backend/internal/repository/image_primary_task_repo_integration_test.go`
- Test: `backend/internal/repository/migrations_schema_integration_test.go`

**Interfaces:**
- Consumes: `ImagePrimaryTaskRepository` from Task 1.
- Produces: atomic `CreateOrGet`, `BindUpstreamTask`, `Transition`, `ClaimSettlement`, and owner-scoped `GetByPublicID`.

- [ ] **Step 1: Write repository tests for ownership and compare-and-set**

```go
func (s *ImagePrimaryTaskRepoSuite) TestCreateOrGetAndClaimSettlement() {
    first, created, err := s.repo.CreateOrGet(s.ctx, service.ImagePrimaryTaskCreate{
        PublicID: "imgp_1", UserID: s.user.ID, APIKeyID: s.key.ID,
        Protocol: "images", Model: "gpt-image-2", RequestHash: "hash-1",
    })
    s.Require().NoError(err)
    s.True(created)
    second, created, err := s.repo.CreateOrGet(s.ctx, service.ImagePrimaryTaskCreate{
        PublicID: "imgp_1", UserID: s.user.ID, APIKeyID: s.key.ID,
        Protocol: "images", Model: "gpt-image-2", RequestHash: "hash-1",
    })
    s.Require().NoError(err)
    s.False(created)
    s.Equal(first.ID, second.ID)
    claimed, err := s.repo.ClaimSettlement(s.ctx, first.ID)
    s.Require().NoError(err)
    s.True(claimed)
    claimed, err = s.repo.ClaimSettlement(s.ctx, first.ID)
    s.Require().NoError(err)
    s.False(claimed)
}
```

- [ ] **Step 2: Run the repository test and verify failure**

Run: `cd backend && go test ./internal/repository -run ImagePrimaryTask -count=1`

Expected: FAIL because the schema and repository are absent.

- [ ] **Step 3: Add schema and migration**

Create columns: `public_id` unique, `user_id`, `api_key_id`, `usage_log_id` nullable, `protocol`, `model`, `request_hash`, `upstream_task_id`, `status`, `fallback_reason`, `result_locator`, `image_count`, `image_size`, `primary_duration_ms`, `fallback_duration_ms`, `settlement_state`, `created_at`, `updated_at`, `expires_at`. Use `settlement_state` values `pending/claimed/settled` and never store Base64.

Migration invariants:

```sql
CREATE UNIQUE INDEX image_primary_tasks_public_id_key ON image_primary_tasks(public_id);
CREATE INDEX image_primary_tasks_owner_idx ON image_primary_tasks(api_key_id, public_id);
ALTER TABLE image_primary_tasks ADD CONSTRAINT image_primary_tasks_status_check
CHECK (status IN ('queued','running','success','error'));
```

- [ ] **Step 4: Generate Ent and implement compare-and-set updates**

Run: `cd backend && go generate ./ent`

Implement `ClaimSettlement` as one SQL/Ent update from `pending` to `claimed`; implement `Transition` with allowed state transitions only: `queued -> running|success|error`, `running -> success|error`, and terminal states unchanged.

- [ ] **Step 5: Run schema and repository tests**

Run: `cd backend && go test ./internal/repository -run 'ImagePrimaryTask|Migration' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/ent backend/internal/repository backend/migrations/100_add_image_primary_routing.sql
git commit -m "feat: persist primary image tasks"
```

### Task 3: Safe Router and Fallback State Machine

**Files:**
- Create: `backend/internal/service/image_primary_router.go`
- Test: `backend/internal/service/image_primary_router_test.go`
- Modify: `backend/internal/service/wire.go`
- Modify generated Wire output: `backend/cmd/server/wire_gen.go`

**Interfaces:**
- Consumes: `ImagePrimaryClient`, `ImagePrimaryTaskRepository`, `config.ChatGPT2APIImage`.
- Produces: `Route(ctx, request) ImagePrimaryRouteResult` and `QueryOwnedTask(ctx, apiKeyID, publicID)`.

- [ ] **Step 1: Write table-driven fallback tests**

```go
func TestImagePrimaryRouterFallbackPolicy(t *testing.T) {
    tests := []struct{ name string; snapshot *ImagePrimarySnapshot; submitErr error; taskLookup ImagePrimaryLookup; want ImagePrimaryDecision }{
        {"success", &ImagePrimarySnapshot{Status: "success", ImageCount: 1}, nil, ImagePrimaryLookup{}, ImagePrimarySuccess},
        {"explicit error", &ImagePrimarySnapshot{Status: "error"}, nil, ImagePrimaryLookup{}, ImagePrimaryFallbackAllowed},
        {"running timeout", &ImagePrimarySnapshot{Status: "running"}, context.DeadlineExceeded, ImagePrimaryLookup{Found: true}, ImagePrimaryPending},
        {"connection uncertain", nil, io.ErrUnexpectedEOF, ImagePrimaryLookup{Uncertain: true}, ImagePrimaryPending},
        {"confirmed missing", nil, io.ErrUnexpectedEOF, ImagePrimaryLookup{Found: false}, ImagePrimaryFallbackAllowed},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            client := &fakeImagePrimaryClient{snapshot: tt.snapshot, submitErr: tt.submitErr, lookup: tt.taskLookup}
            router := newRouterForTest(t, client)
            got := router.Route(context.Background(), imagePrimaryRequestForTest("imgp_1"))
            require.Equal(t, tt.want, got.Decision)
            require.LessOrEqual(t, client.submitCalls, 1)
        })
    }
}
```

- [ ] **Step 2: Run test and verify failure**

Run: `cd backend && go test ./internal/service -run ImagePrimaryRouter -count=1`

Expected: FAIL because the router does not exist.

- [ ] **Step 3: Implement the state machine**

```go
type ImagePrimaryDecision string
const (
    ImagePrimarySuccess         ImagePrimaryDecision = "success"
    ImagePrimaryFallbackAllowed ImagePrimaryDecision = "fallback_allowed"
    ImagePrimaryPending         ImagePrimaryDecision = "pending"
    ImagePrimaryNotApplicable   ImagePrimaryDecision = "not_applicable"
)
```

Use `public_id + request_hash` for idempotency. Reject reuse with a different request hash. Poll until terminal state or configured timeout. On transport/429/5xx errors, query by bound task ID; return fallback only for explicit `error` or confirmed `404`. Never perform fallback inside the router.

- [ ] **Step 4: Generate Wire and run tests**

Run: `cd backend && go generate ./cmd/server && go test ./internal/service -run ImagePrimaryRouter -count=1`

Expected: PASS with no duplicate submit in any case.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/service/image_primary_router.go backend/internal/service/image_primary_router_test.go backend/internal/service/wire.go backend/cmd/server/wire_gen.go
git commit -m "feat: add safe image primary routing state machine"
```

### Task 4: Images and Edits Routing plus Task Query

**Files:**
- Create: `backend/internal/handler/openai_image_primary.go`
- Modify: `backend/internal/handler/openai_images.go`
- Modify: `backend/internal/server/routes/gateway.go`
- Test: `backend/internal/handler/openai_images_primary_test.go`
- Test: `backend/internal/server/routes/gateway_test.go`

**Interfaces:**
- Consumes: `ImagePrimaryRouter.Route`, existing `ForwardImages`, middleware API-key ownership.
- Produces: primary handling for generations/edits and `GET /v1/images/tasks/:task_id`.

- [ ] **Step 1: Write failing handler tests**

```go
func TestImagesPrimarySuccessSkipsNativeSelection(t *testing.T) {
    router := &fakeImagePrimaryRouter{result: service.ImagePrimaryRouteResult{
        Decision: service.ImagePrimarySuccess, PublicTaskID: "imgp_1",
        ResponseBody: []byte(`{"created":1,"data":[{"b64_json":"result"}]}`), ImageCount: 1,
    }}
    handler.Images(ctx)
    require.Equal(t, http.StatusOK, recorder.Code)
    require.Equal(t, "imgp_1", recorder.Header().Get("X-Image-Task-Id"))
    require.Zero(t, nativeSelectorCalls.Load())
}

func TestImagesPrimaryPendingReturnsTaskWithoutFallback(t *testing.T) {
    recorder, nativeCalls := runImagesPrimaryCase(t, service.ImagePrimaryRouteResult{
        Decision: service.ImagePrimaryPending, PublicTaskID: "imgp_pending",
    }, apiKeyFixture(1))
    require.Equal(t, http.StatusGatewayTimeout, recorder.Code)
    require.Contains(t, recorder.Body.String(), "image_primary_pending_timeout")
    require.Equal(t, "imgp_pending", recorder.Header().Get("X-Image-Task-Id"))
    require.Zero(t, nativeCalls)
}

func TestImageTaskQueryRejectsDifferentAPIKey(t *testing.T) {
    recorder := runImageTaskQuery(t, "imgp_owned_by_1", apiKeyFixture(2))
    require.Equal(t, http.StatusNotFound, recorder.Code)
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `cd backend && go test ./internal/handler ./internal/server/routes -run 'ImagesPrimary|ImageTaskQuery' -count=1`

Expected: FAIL because the primary branch and query route are absent.

- [ ] **Step 3: Insert primary orchestration before account selection**

Call the router only after authentication, image permission, moderation, image/user concurrency acquisition and billing eligibility. Map decisions exactly:

```go
switch primary.Decision {
case service.ImagePrimarySuccess:
    writePrimaryResponse(c, primary)
    settlePrimaryUsage(...)
    return
case service.ImagePrimaryPending:
    c.Header("X-Image-Task-Id", primary.PublicTaskID)
    h.errorResponse(c, http.StatusGatewayTimeout, "image_primary_pending_timeout", "Image task is still running")
    return
case service.ImagePrimaryFallbackAllowed:
    bindImageChannel(c, "openai_native_fallback", primary.PublicTaskID, primary.FallbackReason)
    // Continue into the existing native loop without reacquiring user/image slots.
case service.ImagePrimaryNotApplicable:
    bindImageChannel(c, "openai_native", "", "")
}
```

- [ ] **Step 4: Add owner-scoped query aliases**

Register both `/v1/images/tasks/:task_id` and `/images/tasks/:task_id` with the same body-limit, request-ID, ops logger, endpoint normalization and API-key middleware chain as image POST routes.

- [ ] **Step 5: Run focused handler tests**

Run: `cd backend && go test ./internal/handler ./internal/server/routes -run 'ImagesPrimary|ImageTaskQuery|ImagesRoute' -count=1`

Expected: PASS; existing native failover tests remain green.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/handler/openai_image_primary.go backend/internal/handler/openai_images.go backend/internal/handler/openai_images_primary_test.go backend/internal/server/routes/gateway.go backend/internal/server/routes/gateway_test.go
git commit -m "feat: route OpenAI images through chatgpt2api"
```

### Task 5: Account-Free Exactly-Once Billing and Usage Metadata

**Files:**
- Modify: `backend/migrations/100_add_image_primary_routing.sql`
- Modify: `backend/ent/schema/usage_log.go`
- Modify: `backend/internal/service/usage_log.go`
- Modify: `backend/internal/service/openai_gateway_usage.go`
- Modify: `backend/internal/service/usage_billing.go`
- Modify: `backend/internal/repository/usage_log_repo_insert.go`
- Modify: `backend/internal/repository/usage_log_repo_query.go`
- Modify: `backend/internal/handler/dto/types.go`
- Modify: `backend/internal/handler/dto/mappers.go`
- Test: `backend/internal/service/openai_primary_usage_test.go`
- Test: `backend/internal/repository/usage_log_repo_request_type_test.go`
- Test: `backend/internal/handler/dto/mappers_usage_test.go`

**Interfaces:**
- Consumes: final `ImagePrimaryTask`, existing `CalculateImageCost`, `UsageBillingRepository.Apply`.
- Produces: `RecordPrimaryImageUsage(ctx, *OpenAIPrimaryUsageInput)` and admin fields `image_channel`, `primary_task_id`, `primary_duration_ms`, `fallback_reason`, `fallback_duration_ms`.

- [ ] **Step 1: Write failing idempotent billing tests**

```go
func TestRecordPrimaryImageUsageBillsOnceWithoutAccount(t *testing.T) {
    input := &OpenAIPrimaryUsageInput{PublicTaskID: "imgp_1", ImageCount: 1, ImageSize: "2K", Model: "gpt-image-2"}
    require.NoError(t, svc.RecordPrimaryImageUsage(ctx, input))
    require.NoError(t, svc.RecordPrimaryImageUsage(ctx, input))
    require.Equal(t, 1, billingRepo.ApplyCalls())
    log := usageRepo.Only(t)
    require.Nil(t, log.AccountID)
    require.Equal(t, "chatgpt2api", deref(log.ImageChannel))
    require.Equal(t, "image", deref(log.BillingMode))
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler/dto -run 'PrimaryImageUsage|ImageChannel' -count=1`

Expected: FAIL because account-free usage and metadata columns do not exist.

- [ ] **Step 3: Make usage account attribution nullable and add metadata**

Change `usage_logs.account_id` to nullable without removing referential integrity for non-null values. Change service/DTO `AccountID` to `*int64`, update repository scans/inserts and guard account-map enrichment with `nil` checks. Add nullable columns with bounded lengths: `image_channel varchar(32)`, `primary_task_id varchar(64)`, `primary_duration_ms integer`, `fallback_reason varchar(64)`, `fallback_duration_ms integer`.

- [ ] **Step 4: Implement primary settlement**

`RecordPrimaryImageUsage` must call the existing image pricing resolver, build a `UsageBillingCommand` with `AccountID=0`, `AccountType="image_primary"`, `AccountQuotaCost=0`, and request ID equal to `primary_task_id`. It must insert the usage row and mark the task `settled` in an idempotent sequence; retries rely on the existing `(request_id, api_key_id)` usage uniqueness plus `UsageBillingRepository.Apply` fingerprint checks.

- [ ] **Step 5: Run billing and repository tests**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler/dto -run 'PrimaryImageUsage|UsageLog|ImageChannel' -count=1`

Expected: PASS, including repeated query-after-timeout settlement.

- [ ] **Step 6: Commit**

```bash
git add backend/migrations/100_add_image_primary_routing.sql backend/ent backend/internal/service backend/internal/repository backend/internal/handler/dto
git commit -m "feat: bill primary image tasks exactly once"
```

### Task 6: Responses HTTP/SSE and Image-Intent Chat Completions

**Files:**
- Modify: `backend/internal/handler/openai_gateway_handler.go`
- Modify: `backend/internal/handler/openai_chat_completions.go`
- Create: `backend/internal/handler/openai_responses_primary_test.go`
- Create: `backend/internal/handler/openai_chat_primary_test.go`
- Test: `backend/internal/service/image_output_accounting_test.go`

**Interfaces:**
- Consumes: `IsImageGenerationIntent`, primary router, existing `OpenAIImageOutputCounter`, normal `RecordUsage` for no-image token responses.
- Produces: protocol-preserving primary routing for Responses sync/SSE and image-intent Chat Completions.

- [ ] **Step 1: Write failing protocol tests**

```go
func TestResponsesImageIntentUsesPrimaryAndCountsFinalImages(t *testing.T) {
    body := []byte(`{"model":"gpt-5.4","stream":true,"tools":[{"type":"image_generation"}],"input":"draw"}`)
    events := "data: {\"type\":\"response.image_generation_call.partial_image\",\"partial_image_b64\":\"partial\"}\n\n" +
        "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ig_1\",\"type\":\"image_generation_call\",\"result\":\"final\"}}\n\n" +
        "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"id\":\"ig_1\",\"type\":\"image_generation_call\",\"result\":\"final\"}]}}\n\n"
    got := runPrimaryResponses(t, body, events)
    require.Equal(t, events, got.Body)
    require.Equal(t, 1, got.Usage.ImageCount)
    require.Equal(t, "image", got.Usage.BillingMode)
    require.Zero(t, got.NativeSelectionCalls)
}

func TestResponsesTextOnlyPrimaryTerminalUsesTokenBilling(t *testing.T) {
    got := runPrimaryResponses(t, []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation"}],"input":"draw"}`),
        `{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"cannot draw"}]}]}`)
    require.Equal(t, "token", got.Usage.BillingMode)
    require.Zero(t, got.NativeSelectionCalls)
}

func TestChatCompletionsImageIntentUsesPrimaryOnly(t *testing.T) {
    image := runPrimaryChat(t, []byte(`{"model":"gpt-image-2","messages":[{"role":"user","content":"draw"}]}`))
    require.Equal(t, 1, image.PrimaryCalls)
    ordinary := runPrimaryChat(t, []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}},{"type":"text","text":"describe"}]}]}`))
    require.Zero(t, ordinary.PrimaryCalls)
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `cd backend && go test ./internal/handler -run 'ResponsesImageIntent|ResponsesTextOnlyPrimary|ChatCompletionsImageIntent' -count=1`

Expected: FAIL because these paths still select native accounts first.

- [ ] **Step 3: Add the shared primary branch**

For `/v1/responses`, enter only when `IsImageGenerationIntent` is true after current request normalization/tool injection. Forward the complete normalized body. Preserve all SSE event bytes and use the existing counter for final image count/size. A valid terminal response with zero images is success and uses token billing; it never triggers native image fallback.

For Chat Completions, enter only when existing classification marks image generation, then reuse the current Responses-to-Chat compatibility converter. Do not route image-input understanding requests.

- [ ] **Step 4: Run focused and regression tests**

Run: `cd backend && go test ./internal/handler ./internal/service -run 'Responses|ChatCompletions|ImageGenerationIntent|ImageOutput' -count=1`

Expected: PASS with existing Codex bridge and Spark tool-stripping tests unchanged.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/handler/openai_gateway_handler.go backend/internal/handler/openai_chat_completions.go backend/internal/handler/openai_responses_primary_test.go backend/internal/handler/openai_chat_primary_test.go backend/internal/service/image_output_accounting_test.go
git commit -m "feat: route responses image generation through chatgpt2api"
```

### Task 7: Responses WebSocket Turn Routing

**Files:**
- Modify: `backend/internal/handler/openai_gateway_handler.go`
- Modify: `backend/internal/service/openai_ws_forwarder_ingress.go`
- Modify: `backend/internal/service/openai_ws_forwarder_v2.go`
- Create: `backend/internal/service/openai_ws_image_primary.go`
- Test: `backend/internal/service/openai_ws_image_primary_test.go`
- Test: `backend/internal/handler/openai_gateway_handler_test.go`

**Interfaces:**
- Consumes: one `primary_task_id` per `response.create` turn and existing WS event accounting.
- Produces: turn-scoped primary routing with no fallback after task creation or client disconnect.

- [ ] **Step 1: Write failing WebSocket tests**

```go
func TestWSImageTurnCreatesOnePrimaryTask(t *testing.T) {
    result := runWSPrimaryTurns(t, []string{"turn-1", "turn-1"}, false)
    require.Equal(t, 1, result.SubmitCalls)
    require.Equal(t, "imgp_turn_1", result.TerminalMetadata["primary_task_id"])
}

func TestWSDisconnectAfterPrimaryCreationDoesNotFallback(t *testing.T) {
    result := runWSPrimaryTurns(t, []string{"turn-disconnect"}, true)
    require.Equal(t, 1, result.SubmitCalls)
    require.Zero(t, result.NativeDialCalls)
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `cd backend && go test ./internal/service ./internal/handler -run 'WSImageTurn|WSDisconnectAfterPrimary' -count=1`

Expected: FAIL because WS turns have no primary adapter.

- [ ] **Step 3: Implement turn-scoped adapter**

Derive the task ID from authenticated API key ID plus the client turn/response ID, falling back to a generated UUID once per accepted turn. Forward primary events unchanged; append only `metadata.primary_task_id` to the terminal event. Persist the task before dialing upstream. Once persisted, cancellation changes no fallback decision.

- [ ] **Step 4: Run WS regression suite**

Run: `cd backend && go test ./internal/service ./internal/handler -run 'OpenAIWS|WebSocket|WSImage' -count=1`

Expected: PASS, including ingress pool, passthrough and HTTP bridge modes.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/handler/openai_gateway_handler.go backend/internal/service/openai_ws_forwarder_ingress.go backend/internal/service/openai_ws_forwarder_v2.go backend/internal/service/openai_ws_image_primary.go backend/internal/service/openai_ws_image_primary_test.go backend/internal/handler/openai_gateway_handler_test.go
git commit -m "feat: route websocket image turns through chatgpt2api"
```

### Task 8: Admin Usage Channel Display

**Files:**
- Modify: `frontend/src/types/index.ts:1392`
- Modify: `frontend/src/components/admin/usage/UsageTable.vue`
- Modify: `frontend/src/i18n/locales/zh/dashboard.ts`
- Modify: `frontend/src/i18n/locales/en/dashboard.ts`
- Test: `frontend/src/components/admin/usage/__tests__/UsageTable.spec.ts`

**Interfaces:**
- Consumes: admin DTO metadata from Task 5.
- Produces: visible Chinese channel label and copyable task ID in call details/table tooltip.

- [ ] **Step 1: Write failing component test**

```ts
it('shows the actual image channel and task id', () => {
  const wrapper = mountUsageTable({
    image_channel: 'openai_native_fallback',
    primary_task_id: 'imgp_123',
    image_count: 1,
    billing_mode: 'image',
  })
  expect(wrapper.text()).toContain('原生 gpt-image-2（兜底）')
  expect(wrapper.text()).toContain('imgp_123')
})
```

- [ ] **Step 2: Run test and verify failure**

Run: `cd frontend && npm run test -- UsageTable.spec.ts`

Expected: FAIL because the fields are not rendered.

- [ ] **Step 3: Add channel rendering and translations**

Map values exactly: `chatgpt2api -> ChatGPT2API`, `openai_native_fallback -> 原生 gpt-image-2（兜底）`, `openai_native -> 原生 gpt-image-2`. Show `primary_task_id` only when present; use existing icon/button and tooltip components for copying rather than a text-only rounded button.

- [ ] **Step 4: Run tests and build**

Run: `cd frontend && npm run test -- UsageTable.spec.ts && npm run build`

Expected: PASS and production build completes.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/types frontend/src/components/admin/usage/UsageTable.vue frontend/src/components/admin/usage/__tests__/UsageTable.spec.ts frontend/src/i18n/locales/zh/dashboard.ts frontend/src/i18n/locales/en/dashboard.ts
git commit -m "feat: show image generation channel in usage logs"
```

### Task 9: ChatGPT2API Contract Gate and Deployment

**Files:**
- Create: `backend/internal/service/chatgpt2api_contract_integration_test.go`
- Modify: `/Users/hechengjun.9/Documents/conference-latex-template/deployments/chatgpt2api-aliyun/docker-compose.yml`
- Modify: `deploy/docker-compose.yml`
- Modify: `/Users/hechengjun.9/Documents/conference-latex-template/chatgpt2api` only after it contains a reviewed source commit.

**Interfaces:**
- Consumes: deployed ChatGPT2API endpoints `/v1/images/generations`, `/v1/images/edits`, `/v1/responses`, `/v1/images/tasks/{task_id}`.
- Produces: release evidence that all protocol families support stable task IDs before enabling the flag.

- [ ] **Step 1: Add an opt-in live contract test**

```go
func TestChatGPT2APIImagePrimaryLiveContract(t *testing.T) {
    baseURL := os.Getenv("CHATGPT2API_IMAGE_BASE_URL")
    apiKey := os.Getenv("CHATGPT2API_IMAGE_API_KEY")
    if baseURL == "" || apiKey == "" { t.Skip("live contract env is not configured") }
    client := NewChatGPT2APIImageClient(ChatGPT2APIImageClientConfig{BaseURL: baseURL, APIKey: apiKey})
    for _, protocol := range []string{"generations", "edits", "responses"} {
        publicID := "contract-" + protocol + "-" + uuid.NewString()
        first := submitLiveContractTask(t, client, protocol, publicID)
        second := submitLiveContractTask(t, client, protocol, publicID)
        require.Equal(t, first.UpstreamTaskID, second.UpstreamTaskID)
        terminal := pollLiveContractTask(t, client, first.UpstreamTaskID, 300*time.Second)
        require.Contains(t, []string{"success", "error"}, terminal.Status)
    }
}
```

- [ ] **Step 2: Run backend, frontend and race tests locally**

Run: `cd backend && go test ./...`

Run: `cd backend && go test -race ./internal/service ./internal/handler`

Run: `cd frontend && npm run test && npm run build`

Expected: all PASS.

- [ ] **Step 3: Populate the empty ChatGPT2API fork before server-side changes**

Import the exact source corresponding to the deployed image into `1832094726/chatgpt2api`, preserving its license and upstream history. Verify with:

```bash
cd /Users/hechengjun.9/Documents/conference-latex-template/chatgpt2api
git log -1 --oneline
git ls-files api/ai.py services/image_task_service.py services/protocol/openai_v1_response.py
```

Expected: one or more commits exist and all three source files are tracked. If this check fails, do not modify the container and do not enable the Sub2API primary flag.

- [ ] **Step 4: Run the live contract test with the flag still disabled**

Run: `cd backend && go test ./internal/service -run TestChatGPT2APIImagePrimaryLiveContract -count=1 -v`

Expected: PASS for generations, edits and Responses stable-task behavior. Any failure blocks deployment; implement the missing behavior in the populated ChatGPT2API fork with Python tests before retrying.

- [ ] **Step 5: Configure private networking and enable the flag**

Place both services on one private Docker network or explicitly configure host-gateway. Set only server-side environment values:

```yaml
environment:
  CHATGPT2API_IMAGE_PRIMARY_ENABLED: "true"
  CHATGPT2API_IMAGE_BASE_URL: "http://chatgpt2api:3000"
  CHATGPT2API_IMAGE_API_KEY: "${CHATGPT2API_AUTH_KEY}"
  CHATGPT2API_IMAGE_TIMEOUT_SECONDS: "300"
  CHATGPT2API_IMAGE_POLL_INTERVAL_SECONDS: "5"
```

- [ ] **Step 6: Deploy and perform smoke tests**

Verify one request for Images, Edits, Responses sync, Responses SSE and Responses WebSocket. Confirm primary success displays `ChatGPT2API`; simulate explicit primary `error` and confirm one native request plus `原生 gpt-image-2（兜底）`; leave a task running past timeout and confirm task query returns the same ID with no native call and only one eventual charge.

- [ ] **Step 7: Commit deployment configuration without secrets**

```bash
git add deploy/docker-compose.yml backend/internal/service/chatgpt2api_contract_integration_test.go
git commit -m "deploy: enable chatgpt2api image primary routing"
```
