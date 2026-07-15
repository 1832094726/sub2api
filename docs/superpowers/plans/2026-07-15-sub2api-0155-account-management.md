# Sub2API 0.1.155 Account Management Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在保留现有图片主路由与账号健康检查能力的前提下同步官方 0.1.155，并交付图片模型动态设置、多分组导入、受控导入 JSON 查看和可靠的 OpenAI 隐私设置。

**Architecture:** 现有图片路由分支合并 `upstream/main`，应用版本保持官方 `0.1.155`。运行时图片模型通过 SettingService 解析；导入快照使用独立表和 AES-256-GCM；隐私请求复用 ChatGPT 账号解析及公共请求头，不改变自动刷新令牌的 best-effort 语义。

**Tech Stack:** Go、Gin、Ent/PostgreSQL、Redis、Vue 3、TypeScript、Vitest、GitHub Actions、Docker Compose。

## Global Constraints

- 应用内版本必须是 `0.1.155`，自定义标识只出现在镜像标签 `0.1.155-custom.1`。
- 图片模型仅允许 `codex-gpt-image-2` 和 `gpt-image-2`，默认 `codex-gpt-image-2`。
- 普通账号接口不得返回完整导入 JSON；完整查看必须二次确认并审计。
- 旧字段 `default_group_id` 必须继续兼容。
- 自动刷新令牌不得因隐私设置失败而失败。
- 测试、日志和提交内容不得包含真实 Token、Cookie、邮箱或管理员凭据。

---

### Task 1: Merge Upstream 0.1.155

**Files:**
- Modify: conflicts reported by `git merge upstream/main`
- Verify: `backend/cmd/server/VERSION`

**Interfaces:**
- Consumes: `upstream/main` at the fetched official head and current image-routing branch.
- Produces: one integrated branch whose application version is exactly `0.1.155`.

- [ ] **Step 1: Record baseline tests**

Run:

```bash
rtk sh -lc 'cd backend && go test ./internal/service ./internal/handler/admin'
rtk sh -lc 'cd frontend && npm run test:unit -- --run'
```

Expected: existing branch tests pass, or every pre-existing failure is recorded before merge.

- [ ] **Step 2: Merge official main without hiding conflicts**

```bash
rtk git merge --no-ff upstream/main
```

Expected: a clean merge or an explicit conflict list. Resolve by preserving official behavior plus the image-routing additions; never select an entire side for shared gateway files without reading both versions.

- [ ] **Step 3: Verify version semantics**

```bash
rtk sh -lc 'test "$(cat backend/cmd/server/VERSION)" = "0.1.155"'
rtk sh -lc 'cd backend && go test ./internal/service -run Test.*Version -count=1'
```

Expected: both commands pass and no custom suffix appears in `VERSION`.

- [ ] **Step 4: Run merged smoke tests and commit**

```bash
rtk sh -lc 'cd backend && go test ./internal/service ./internal/handler/admin'
rtk sh -lc 'cd frontend && npm run type-check'
rtk git commit
```

Expected: merged baseline passes and merge commit is created.

### Task 2: Dynamic Image Routing Model

**Files:**
- Modify: `backend/internal/service/setting_features.go`
- Modify: `backend/internal/service/settings_view.go`
- Modify: `backend/internal/handler/admin/setting_handler.go`
- Modify: `backend/internal/handler/admin/setting_handler_update.go`
- Modify: `backend/internal/handler/admin/setting_handler_audit.go`
- Modify: `backend/internal/service/chatgpt2api_image_client.go`
- Test: `backend/internal/service/setting_service_image_model_test.go`
- Modify: `frontend/src/views/admin/SettingsView.vue`
- Modify: `frontend/src/api/admin/settings.ts`
- Modify: `frontend/src/i18n/locales/zh/admin/settings.ts`
- Modify: `frontend/src/i18n/locales/en/admin/settings.ts`
- Test: `frontend/src/views/admin/__tests__/SettingsView.spec.ts`

**Interfaces:**
- Produces: `NormalizeChatGPT2APIImageModel(string) (string, error)` and `(*SettingService).GetChatGPT2APIImageModel(context.Context) string`.
- Consumes: setting key `chatgpt2api_image_model` and environment fallback `CHATGPT2API_IMAGE_MODEL`.

- [ ] **Step 1: Write failing resolver tests**

Add table tests asserting empty values resolve to `codex-gpt-image-2`, both whitelist values are accepted, illegal values return an error, and a persisted setting overrides the environment.

```go
func TestNormalizeChatGPT2APIImageModel(t *testing.T) {
    require.Equal(t, "codex-gpt-image-2", mustNormalize(t, ""))
    require.Equal(t, "gpt-image-2", mustNormalize(t, "gpt-image-2"))
    _, err := NormalizeChatGPT2APIImageModel("auto")
    require.Error(t, err)
}
```

- [ ] **Step 2: Verify resolver tests fail**

```bash
rtk sh -lc 'cd backend && go test ./internal/service -run ChatGPT2APIImageModel -count=1'
```

Expected: FAIL because the resolver and setting accessor do not exist.

- [ ] **Step 3: Implement resolver and runtime lookup**

Implement the whitelist, database/environment/default priority, update validation and cache invalidation. Change the image client to resolve the model for each task through SettingService rather than retaining only startup configuration.

- [ ] **Step 4: Add and run frontend failing test**

Assert the Gateway tab renders exactly two Chinese-labeled options and submits `chatgpt2api_image_model`.

```bash
rtk sh -lc 'cd frontend && npm run test:unit -- --run src/views/admin/__tests__/SettingsView.spec.ts'
```

Expected before UI implementation: FAIL because the select is absent.

- [ ] **Step 5: Implement UI, run tests and commit**

```bash
rtk sh -lc 'cd backend && go test ./internal/service ./internal/handler/admin -run ImageModel -count=1'
rtk sh -lc 'cd frontend && npm run test:unit -- --run src/views/admin/__tests__/SettingsView.spec.ts'
rtk git add backend frontend
rtk git commit -m "feat: configure image routing model at runtime"
```

### Task 3: Multi-Group Account Import

**Files:**
- Modify: `backend/internal/handler/admin/account_data.go`
- Test: `backend/internal/handler/admin/account_data_import_defaults_test.go`
- Modify: `frontend/src/components/admin/account/ImportDataModal.vue`
- Modify: `frontend/src/api/admin/accounts.ts`
- Modify: `frontend/src/types/index.ts`
- Modify: `frontend/src/i18n/locales/zh/admin/accounts.ts`
- Modify: `frontend/src/i18n/locales/en/admin/accounts.ts`
- Test: `frontend/src/__tests__/integration/data-import.spec.ts`

**Interfaces:**
- Produces request field `default_group_ids: number[]` while retaining `default_group_id?: number`.
- Produces backend helper `resolveImportGroupIDs(newIDs []int64, legacyID *int64) []int64`.

- [ ] **Step 1: Write failing compatibility tests**

Cover array precedence, legacy fallback, duplicate removal, all selected group bindings, and atomic rejection when any group is missing, inactive, or platform-incompatible.

```go
func TestResolveImportGroupIDs_NewArrayWinsAndDeduplicates(t *testing.T) {
    legacy := int64(9)
    require.Equal(t, []int64{2, 3}, resolveImportGroupIDs([]int64{2, 3, 2}, &legacy))
}
```

- [ ] **Step 2: Run failing backend test**

```bash
rtk sh -lc 'cd backend && go test ./internal/handler/admin -run Import.*Group -count=1'
```

Expected: FAIL because `default_group_ids` and the resolver are absent.

- [ ] **Step 3: Implement backend validation and binding**

Extend `DataImportRequest`, normalize the IDs once before account processing, validate all groups, then bind every successfully imported account to every normalized group.

- [ ] **Step 4: Write failing frontend payload test**

Mount `ImportDataModal`, select two groups and assert the request contains `default_group_ids: [2, 8]` and does not collapse to one ID.

- [ ] **Step 5: Implement searchable multi-select, test and commit**

```bash
rtk sh -lc 'cd backend && go test ./internal/handler/admin -run Import -count=1'
rtk sh -lc 'cd frontend && npm run test:unit -- --run src/__tests__/integration/data-import.spec.ts'
rtk git add backend frontend
rtk git commit -m "feat: bind imported accounts to multiple groups"
```

### Task 4: Encrypted Import Snapshot and Controlled View

**Files:**
- Create: `backend/ent/schema/account_import_snapshot.go`
- Generate: `backend/ent/accountimportsnapshot*` and related Ent client files
- Create: next available `backend/migrations/<NNN>_account_import_snapshots.sql`
- Create: `backend/internal/service/account_import_snapshot.go`
- Create: `backend/internal/repository/account_import_snapshot_repo.go`
- Test: `backend/internal/service/account_import_snapshot_test.go`
- Test: `backend/internal/repository/account_import_snapshot_repo_test.go`
- Modify: `backend/internal/handler/admin/account_data.go`
- Create: `backend/internal/handler/admin/account_import_snapshot.go`
- Modify: `backend/internal/server/routes/admin.go`
- Modify: `backend/internal/handler/wire.go`
- Modify: `backend/internal/repository/wire.go`
- Modify: `backend/internal/service/wire.go`
- Modify: `backend/cmd/server/wire_gen.go`
- Modify: `frontend/src/components/admin/account/AccountStatsModal.vue`
- Modify: `frontend/src/api/admin/accounts.ts`
- Modify: `frontend/src/types/index.ts`
- Test: `frontend/src/components/admin/account/__tests__/AccountStatsModal.spec.ts`

**Interfaces:**
- Produces: `Save(ctx, accountID, batchID, encryptedJSON)` and `GetByAccountID(ctx, accountID)` repository methods.
- Produces: `MaskImportJSON(any) any`, masked GET endpoint, and explicit reveal POST endpoint.
- Consumes: existing `service.SecretEncryptor` AES-256-GCM implementation.

- [ ] **Step 1: Write failing masking and encryption tests**

Test recursive maps and arrays, case-insensitive sensitive keys, non-sensitive preservation, encrypt/decrypt round trip, latest snapshot overwrite, and absence for manually created accounts.

```go
func TestMaskImportJSON_RecursivelyMasksSecrets(t *testing.T) {
    in := map[string]any{"email": "visible@example.test", "credentials": map[string]any{"Refresh_Token": "abcdefghijk"}}
    got := MaskImportJSON(in).(map[string]any)
    require.Equal(t, "visible@example.test", got["email"])
    require.NotEqual(t, "abcdefghijk", got["credentials"].(map[string]any)["Refresh_Token"])
}
```

- [ ] **Step 2: Verify tests fail and implement storage**

```bash
rtk sh -lc 'cd backend && go test ./internal/service ./internal/repository -run ImportSnapshot -count=1'
```

Expected before implementation: FAIL. Then add migration, Ent schema/repository and service; regenerate Ent with the repository's existing generation command.

- [ ] **Step 3: Add handler tests before routes**

Tests must assert masked GET never contains plaintext secrets, reveal requires an authenticated admin, reveal sets `Cache-Control: no-store`, and every successful reveal writes an audit event without JSON content.

- [ ] **Step 4: Implement endpoints and import hook**

Persist the source object only after each account import succeeds. Return metadata and masked JSON from the stats path; decrypt only inside the reveal endpoint and clear local references after serialization.

- [ ] **Step 5: Add frontend failing test and implement UI**

Test default masked rendering, confirmation before reveal, hide action, and cleanup on modal close. Never place revealed content in a store or browser storage.

- [ ] **Step 6: Run focused tests and commit**

```bash
rtk sh -lc 'cd backend && go test ./internal/service ./internal/repository ./internal/handler/admin -run ImportSnapshot -count=1'
rtk sh -lc 'cd frontend && npm run test:unit -- --run src/components/admin/account/__tests__/AccountStatsModal.spec.ts'
rtk git add backend frontend
rtk git commit -m "feat: retain encrypted account import snapshots"
```

### Task 5: Reliable OpenAI Privacy Setting

**Files:**
- Modify: `backend/internal/service/openai_privacy_service.go`
- Modify: `backend/internal/service/token_refresh_service.go`
- Modify: `backend/internal/handler/admin/account_handler.go`
- Test: `backend/internal/service/openai_privacy_retry_test.go`
- Create: `backend/internal/service/openai_privacy_headers_test.go`
- Modify: `frontend/src/views/admin/AccountsView.vue`
- Modify: `frontend/src/i18n/locales/zh/admin/accounts.ts`
- Modify: `frontend/src/i18n/locales/en/admin/accounts.ts`
- Test: relevant account view test under `frontend/src/views/admin/__tests__`

**Interfaces:**
- Produces: structured `OpenAIPrivacyResult{Mode, Code, Status, Message}`.
- Consumes: resolved credential account, TokenProvider, proxy URL, `chatgpt_account_id` with `organization_id` fallback, and the existing ChatGPT common-header pattern.

- [ ] **Step 1: Write failing request-header tests**

Use an HTTP test server/client factory and assert `Authorization`, `chatgpt-account-id`, `openai-beta`, `originator`, `oai-language`, Origin and Referer are present. Add separate cases for missing account ID, 401 JSON, Cloudflare HTML, generic 403 and timeout.

- [ ] **Step 2: Verify tests fail**

```bash
rtk sh -lc 'cd backend && go test ./internal/service -run OpenAIPrivacy -count=1'
```

Expected: FAIL because the current helper accepts only token/proxy and cannot provide account context.

- [ ] **Step 3: Implement account-aware privacy call**

Refactor the manual handler to load and resolve the account, refresh/access the token through TokenProvider, build complete ChatGPT headers, classify errors and return non-200 on manual failure. Keep token refresh calling a best-effort wrapper that persists failure mode but returns no refresh error.

- [ ] **Step 4: Implement precise frontend errors**

Map missing account ID to reauthorization guidance, 401 to token invalid, Cloudflare challenge to network/IP guidance, and generic upstream rejection without falsely labeling it Cloudflare.

- [ ] **Step 5: Run tests and commit**

```bash
rtk sh -lc 'cd backend && go test ./internal/service ./internal/handler/admin -run Privacy -count=1'
rtk sh -lc 'cd frontend && npm run test:unit -- --run src/views/admin/__tests__'
rtk git add backend frontend
rtk git commit -m "fix: send account context for OpenAI privacy settings"
```

### Task 6: Full Verification, Publish and Azure Rollout

**Files:**
- Modify if needed: `.github/workflows/*` image build workflow
- Modify: Azure `/opt/sub2api/docker-compose.yml` image tag after artifact is available

**Interfaces:**
- Consumes: all prior commits and GitHub Actions artifact `ghcr.io/1832094726/sub2api:0.1.155-custom.1`.
- Produces: healthy Azure deployment and verified `0.1.155` UI version.

- [ ] **Step 1: Run complete local verification**

```bash
rtk sh -lc 'cd backend && go test ./...'
rtk sh -lc 'cd frontend && npm run test:unit -- --run && npm run type-check && npm run build'
rtk git diff --check
rtk git status --short
```

Expected: all tests/builds pass, no whitespace errors, only intentional files are changed.

- [ ] **Step 2: Push fork and build image**

```bash
rtk git push origin feat/chatgpt2api-image-routing
```

Wait for GitHub Actions success and verify the immutable image digest before deployment.

- [ ] **Step 3: Back up production and deploy**

On Azure, back up PostgreSQL and Compose configuration, update only the Sub2API image tag, pull, migrate and recreate the application container. Do not restart PostgreSQL or Redis unnecessarily.

- [ ] **Step 4: Verify production behavior**

Check health, application version `0.1.155`, image setting read/write, a two-group test import, masked snapshot view, audited reveal, privacy-setting response classification and a normal OpenAI streaming request.

- [ ] **Step 5: Record release and rollback evidence**

Record image digest, migration version, health result and previous image `0.1.153-image-routing.5`. If any critical check fails, restore the previous image and database snapshot before further debugging.
