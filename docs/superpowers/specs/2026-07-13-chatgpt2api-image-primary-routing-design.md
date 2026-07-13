# ChatGPT2API 优先的图片生成双渠道设计

## 1. 目标

将 Sub2API 的全部 OpenAI/Codex 生图能力调整为双渠道路由：优先使用同机 `chatgpt2api` 图片生成能力，确认主渠道失败后再进入现有原生 `gpt-image-2` 账号调度。无论最终采用哪个上游，继续按 Sub2API 当前图片价格和计费规则收费。

本期覆盖：

- `/v1/images/generations`，包括 `n > 1`。
- `/v1/images/edits`。
- `/v1/responses` 中显式提供或由 Sub2API 自动注入的 `image_generation` 工具。
- Responses API 的同步、SSE 流式和 WebSocket 传输。
- `/v1/chat/completions` 中被现有逻辑识别为图片生成的 OpenAI/Codex 请求。

Gemini、Antigravity、Grok 图片接口保持各自原渠道。现有 `/v1/images/batches` 是 Gemini/Vertex 批量服务，不属于 OpenAI/Codex，本期不迁移；OpenAI 单次请求内的多图 `n > 1` 仍迁移到 ChatGPT2API。

## 2. 方案选择

采用统一 OpenAI 图片主渠道适配器，并在各协议处理器的上游转发边界调用。图片请求通过现有鉴权、内容审核、图片并发和计费资格检查后，先交给独立的 ChatGPT2API 图片客户端；主渠道未产出结果且满足兜底条件时，再进入已有账号选择、原生转发、账号切换和错误处理流程。

不把 ChatGPT2API 建模为普通账号，避免它参与账号测活、OAuth 刷新、优先级计算和账号错误状态管理。

## 3. 配置

新增服务端配置：

- `CHATGPT2API_IMAGE_PRIMARY_ENABLED`：是否启用主渠道，默认 `false`。
- `CHATGPT2API_IMAGE_BASE_URL`：服务器内部地址，优先使用同一 Docker 网络内的服务名，例如 `http://chatgpt2api:3000`；若使用宿主机端口，则显式配置 `host-gateway` 后再使用宿主机地址。
- `CHATGPT2API_IMAGE_API_KEY`：ChatGPT2API 调用密钥，只从环境变量读取，不写入日志或前端。
- `CHATGPT2API_IMAGE_TIMEOUT_SECONDS`：任务总等待时间，默认 `300`。
- `CHATGPT2API_IMAGE_POLL_INTERVAL_SECONDS`：任务轮询间隔，默认 `5`。

配置缺失或功能关闭时完全沿用现有原生链路。

## 4. 主渠道请求流程

### 4.1 Images API

1. 保留现有请求解析、模型校验、分组生图权限、内容审核、并发限制和计费资格检查。
2. 为请求生成稳定的 `primary_task_id`，优先使用合法的客户端请求 ID，没有时生成 UUID。Sub2API 在本地保存该 ID、ChatGPT2API 返回的内部任务 ID与当前调用记录的映射。
3. 向 ChatGPT2API `/v1/images/generations` 提交 OpenAI 兼容字段，并强制增加：
   - `background=true`
   - `client_task_id=<primary_task_id>`
4. 主渠道应透传 `model`、`prompt`、`n`、`size`、`quality` 和 `response_format`。未知扩展字段不透传，避免两端语义不一致。
5. 提交成功后，每 5 秒按任务映射调用 `/v1/images/tasks/{task_id}`：
   - `success`：提取最终 OpenAI Images 响应并返回。
   - `error`：记录原因并进入原生兜底。
   - `queued/running`：继续轮询。
6. 达到 300 秒时任务仍在运行，返回 `504` 和 `primary_task_id`，不触发原生兜底，以免重复生成。调用方可通过 Sub2API 的鉴权接口 `GET /v1/images/tasks/{primary_task_id}` 查询同一任务。

图片编辑调用 ChatGPT2API `/v1/images/edits`，保留 multipart 图片、mask、prompt、model、size、quality 和 response format。实施时先为 ChatGPT2API 补齐与文生图一致的稳定任务 ID 和查询能力，再启用 Sub2API 的统一主渠道开关；不允许出现“文生图已切换但编辑仍静默走原生”的部分启用状态。

### 4.2 Responses API

1. 继续使用现有 `IsImageGenerationIntent` 判断请求是否可能生图。普通文本 Responses 请求不进入图片主渠道。
2. 对包含 `image_generation` 工具的请求，将完整 Responses 请求发送到 ChatGPT2API `/v1/responses`，保留输入、工具、tool choice、模型、流式选项和会话关联字段。
3. ChatGPT2API 返回的 `image_generation_call`、增量图片事件、文本事件、usage 和终态事件按原协议转发，Sub2API 继续使用现有图片计数器统计最终图片数和尺寸。
4. 同步与 SSE 请求使用稳定 `primary_task_id`。ChatGPT2API 需要把该 ID 关联到内部图片任务，并允许通过图片任务接口查询终态；Sub2API 本地保存任务映射，不依赖提示词或请求体哈希反查任务。
5. WebSocket 请求按每个 response turn 生成任务 ID；同一 turn 内只允许创建一个主渠道任务。连接断开后任务仍存在时不得触发原生兜底。
6. Responses 主渠道返回文本但未产生图片时，视为主渠道成功，不再调用原生生图；计费按实际返回的 Token 结果处理。

### 4.3 Chat Completions

仅当现有请求分类明确识别为图片生成时切换 ChatGPT2API。返回值继续转换为当前 Chat Completions 兼容结构，图片数量和尺寸进入统一计费结果。普通多模态理解、图片输入但不要求生成图片的请求不切换。

## 5. 兜底判定

允许进入原生 `gpt-image-2` 链路的情况：

- 连接 ChatGPT2API 失败，并且按任务 ID 查询后确认任务不存在。
- 提交返回 `429` 或 `5xx`，并且按任务 ID查询后确认任务不存在。
- 任务明确进入 `error`。
- 任务查询明确返回 `404`。
- ChatGPT2API 返回无法解析且不包含图片结果的终态响应。

禁止兜底的情况：

- 已收到一张或多张图片，或 Responses 请求已收到合法终态结果。
- 任务仍为 `queued/running`。
- 客户端主动取消或连接断开，但主渠道任务已经创建。
- Sub2API 无法确认主渠道是否已创建任务。

原生兜底复用现有账号调度与账号间 failover，不重新执行用户级计费资格检查，不重复占用用户并发槽。

任务创建与兜底判定必须使用同一个本地任务状态机。`primary_task_id` 在数据库中唯一；重复提交同一 ID 只读取或续查已有任务，不创建新的 ChatGPT2API 或原生任务。

## 6. 响应兼容

Images API 主渠道成功时，转换为现有 OpenAI Images 响应结构，保留 `created`、`data[].b64_json/url`、`revised_prompt` 和可用的 `usage`。内部字段 `task_id` 不注入标准成功响应，通过响应头 `X-Image-Task-Id` 暴露。

Responses 和 Chat Completions 保持各自标准响应结构，任务 ID 通过响应头或 WebSocket 终态事件的扩展 metadata 暴露，不改变标准图片事件字段。

主渠道超时时返回标准 OpenAI 错误结构，错误码为 `image_primary_pending_timeout`，响应头携带 `X-Image-Task-Id`，便于后台排查和后续任务查询。

`GET /v1/images/tasks/{primary_task_id}` 复用调用方 API Key 鉴权，并校验任务归属。运行中返回 `queued/running`；成功时返回原协议的最终图片结果；失败时返回标准错误。该接口不得暴露 ChatGPT2API 内部任务 ID、密钥或其他用户的任务。

## 7. 计费

- ChatGPT2API 主渠道成功：沿用当前请求模型、尺寸和图片张数计算价格。
- 原生兜底成功：沿用同一计费规则。
- 主渠道失败后兜底成功：只生成一条最终用量和扣费记录。
- 主渠道任务失败且原生兜底也失败：不扣图片成功费用，保留失败调用日志。
- 主渠道超过 300 秒仍运行：超时响应时不记成功图片费用；任务查询发现最终成功时，基于 `primary_task_id` 首次完成一次性结算。重复查询只返回结果，不重复扣费；后续不得自动创建第二笔原生任务。
- Responses 主渠道未生成图片但正常返回文本：按现有 Token 规则计费，不显示“按次（图片）”。
- Responses 主渠道生成图片：沿用当前逻辑优先显示“按次（图片）”，`image_count` 和尺寸决定图片费用。

## 8. 日志与运营字段

每次调用写入结构化字段：

- `image_channel=chatgpt2api`：主渠道成功。
- `image_channel=openai_native_fallback`：主渠道失败后由原生兜底成功。
- `image_channel=openai_native`：主渠道关闭、配置缺失或请求不适用主渠道时沿用原生处理。
- `primary_task_id`：Sub2API 对外任务号。
- `primary_task_status`：`queued`、`running`、`success` 或 `error`。
- `primary_duration_ms`：主渠道提交和等待耗时。
- `fallback_reason`：`connection_error`、`http_429`、`http_5xx`、`task_failed`、`task_not_found` 或 `invalid_terminal_response`。
- `fallback_duration_ms`：原生兜底耗时。
- `image_count`：最终返回图片数。
- `billing_mode=image`：生成图片时沿用现有图片计费展示；Responses 无图终态仍使用现有 Token 计费模式。

调用日志详情新增“生图渠道”字段，中文展示为“ChatGPT2API”、“原生 gpt-image-2（兜底）”或“原生 gpt-image-2”。渠道切换过程写入 Ops 上游错误事件，但不额外生成用量记录。

日志禁止输出 ChatGPT2API API Key、完整提示词、图片 Base64 和账号凭据。

## 9. 组件边界

- `ChatGPT2APIImageClient`：只负责提交任务、查询任务、解析主渠道响应。
- `ChatGPT2APIResponsesClient`：负责 Responses HTTP、SSE 和 WebSocket 协议转发及任务 ID 关联。
- `ImagePrimaryRouteResult`：表达成功、可兜底失败、仍在运行和不确定状态。
- `ImagePrimaryTaskStore`：持久化任务归属、主渠道任务映射、状态、最终结果定位和结算状态，并以 `primary_task_id` 保证幂等。
- `OpenAIGatewayHandler.Images`、Responses 和 Chat Completions 处理器：保留入口编排，统一调用主渠道适配器并决定是否进入原生流程。
- 现有 `ForwardImages`、账号调度和计费服务不改变内部职责。
- ChatGPT2API 需补充 Responses/编辑请求的稳定任务 ID 关联；该能力与 Sub2API 路由在同一发布单元验收通过后才开启主渠道开关。Sub2API 不通过猜测或提示词哈希判断任务是否存在。

## 10. 测试与验收

单元测试覆盖：

- 主渠道成功且不调用原生账号。
- 图片编辑成功且保留 multipart 图片和 mask。
- Responses 同步、SSE 和 WebSocket 生图均由主渠道处理并正确统计图片数。
- Responses 正常返回文本但没有图片时按 Token 计费且不兜底。
- 普通文本、多模态理解和 Gemini/Grok 图片请求不进入 ChatGPT2API 图片主渠道。
- 主渠道任务失败后调用原生一次。
- 提交失败但任务已存在时继续轮询，不兜底。
- 300 秒仍运行时返回任务号且不兜底。
- 超时任务随后成功时可按任务号查询，并且只结算一次；其他 API Key 无法读取该任务。
- 重复使用同一 `primary_task_id` 提交时不创建第二个上游任务。
- 主渠道已返回图片后发生尾部错误时不兜底。
- 主渠道和兜底成功均只产生一次计费记录。
- 日志包含正确渠道、任务 ID 和兜底原因，不包含密钥和图片内容。
- 功能关闭或配置缺失时与当前行为一致。

部署验收：

- 服务器内部可访问 ChatGPT2API 健康检查与图片任务接口。
- 实际生成一张图片时，调用详情显示 `ChatGPT2API`。
- Responses HTTP、SSE、WebSocket 和 Images API 的调用详情均显示实际生图渠道。
- 模拟主渠道终态失败后，调用详情显示“原生 gpt-image-2（兜底）”。
- 两条路径的用户价格与当前价格一致。
- 主渠道长任务不会触发第二张重复图片。
