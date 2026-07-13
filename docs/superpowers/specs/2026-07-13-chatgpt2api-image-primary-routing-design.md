# ChatGPT2API 优先的图片生成双渠道设计

## 1. 目标

将 Sub2API 的 `POST /v1/images/generations` 调整为双渠道路由：优先使用同机 `chatgpt2api` 图片生成能力，确认主渠道失败后再进入现有原生 `gpt-image-2` 账号调度。无论最终采用哪个上游，继续按 Sub2API 当前图片价格和计费规则收费。

本期只覆盖文生图 `/v1/images/generations`。`/v1/images/edits`、Responses API 中的 `image_generation` 工具和批量图片接口保持原逻辑。

## 2. 方案选择

采用处理器级优先路由。图片请求通过现有鉴权、内容审核、图片并发和计费资格检查后，先交给独立的 ChatGPT2API 图片客户端；主渠道未产出结果且满足兜底条件时，再进入已有账号选择、原生转发、账号切换和错误处理流程。

不把 ChatGPT2API 建模为普通账号，避免它参与账号测活、OAuth 刷新、优先级计算和账号错误状态管理。

## 3. 配置

新增服务端配置：

- `CHATGPT2API_IMAGE_PRIMARY_ENABLED`：是否启用主渠道，默认 `false`。
- `CHATGPT2API_IMAGE_BASE_URL`：服务器内部地址，部署值为 `http://host.docker.internal:3000`。
- `CHATGPT2API_IMAGE_API_KEY`：ChatGPT2API 调用密钥，只从环境变量读取，不写入日志或前端。
- `CHATGPT2API_IMAGE_TIMEOUT_SECONDS`：任务总等待时间，默认 `300`。
- `CHATGPT2API_IMAGE_POLL_INTERVAL_SECONDS`：任务轮询间隔，默认 `5`。

配置缺失或功能关闭时完全沿用现有原生链路。

## 4. 主渠道请求流程

1. 保留现有请求解析、模型校验、分组生图权限、内容审核、并发限制和计费资格检查。
2. 为请求生成稳定的 `primary_task_id`，优先使用客户端请求 ID，没有时生成 UUID。
3. 向 ChatGPT2API `/v1/images/generations` 提交 OpenAI 兼容字段，并强制增加：
   - `background=true`
   - `client_task_id=<primary_task_id>`
4. 主渠道应透传 `model`、`prompt`、`n`、`size`、`quality` 和 `response_format`。未知扩展字段不透传，避免两端语义不一致。
5. 提交成功后，每 5 秒调用 `/v1/images/tasks/{primary_task_id}`：
   - `success`：提取最终 OpenAI Images 响应并返回。
   - `error`：记录原因并进入原生兜底。
   - `queued/running`：继续轮询。
6. 达到 300 秒时任务仍在运行，返回 `504` 和 `primary_task_id`，不触发原生兜底，以免重复生成。

## 5. 兜底判定

允许进入原生 `gpt-image-2` 链路的情况：

- 连接 ChatGPT2API 失败，并且按任务 ID 查询后确认任务不存在。
- 提交返回 `429` 或 `5xx`，并且按任务 ID查询后确认任务不存在。
- 任务明确进入 `error`。
- 任务查询明确返回 `404`。
- ChatGPT2API 返回无法解析且不包含图片结果的终态响应。

禁止兜底的情况：

- 已收到一张或多张图片。
- 任务仍为 `queued/running`。
- 客户端主动取消或连接断开，但主渠道任务已经创建。
- Sub2API 无法确认主渠道是否已创建任务。

原生兜底复用现有账号调度与账号间 failover，不重新执行用户级计费资格检查，不重复占用用户并发槽。

## 6. 响应兼容

主渠道成功时，转换为现有 OpenAI Images 响应结构，保留 `created`、`data[].b64_json/url`、`revised_prompt` 和可用的 `usage`。内部字段 `task_id` 不注入标准成功响应，通过响应头 `X-Image-Task-Id` 暴露。

主渠道超时时返回标准 OpenAI 错误结构，错误码为 `image_primary_pending_timeout`，响应头携带 `X-Image-Task-Id`，便于后台排查和后续任务查询。

## 7. 计费

- ChatGPT2API 主渠道成功：沿用当前请求模型、尺寸和图片张数计算价格。
- 原生兜底成功：沿用同一计费规则。
- 主渠道失败后兜底成功：只生成一条最终用量和扣费记录。
- 主渠道任务失败且原生兜底也失败：不扣图片成功费用，保留失败调用日志。
- 主渠道超过 300 秒仍运行：本次不记成功图片费用；后续不得自动创建第二笔原生任务。

## 8. 日志与运营字段

每次调用写入结构化字段：

- `image_channel=chatgpt2api`：主渠道成功。
- `image_channel=openai_native_fallback`：原生兜底成功或最终由原生处理。
- `primary_task_id`：ChatGPT2API 任务号。
- `primary_duration_ms`：主渠道提交和等待耗时。
- `fallback_reason`：`connection_error`、`http_429`、`http_5xx`、`task_failed`、`task_not_found` 或 `invalid_terminal_response`。
- `fallback_duration_ms`：原生兜底耗时。
- `image_count`：最终返回图片数。
- `billing_mode=existing_price`。

调用日志详情新增“生图渠道”字段，中文展示为“ChatGPT2API”或“原生 gpt-image-2（兜底）”。渠道切换过程写入 Ops 上游错误事件，但不额外生成用量记录。

日志禁止输出 ChatGPT2API API Key、完整提示词、图片 Base64 和账号凭据。

## 9. 组件边界

- `ChatGPT2APIImageClient`：只负责提交任务、查询任务、解析主渠道响应。
- `ImagePrimaryRouteResult`：表达成功、可兜底失败、仍在运行和不确定状态。
- `OpenAIGatewayHandler.Images`：保留入口编排，决定是否调用现有原生流程。
- 现有 `ForwardImages`、账号调度和计费服务不改变内部职责。

## 10. 测试与验收

单元测试覆盖：

- 主渠道成功且不调用原生账号。
- 主渠道任务失败后调用原生一次。
- 提交失败但任务已存在时继续轮询，不兜底。
- 300 秒仍运行时返回任务号且不兜底。
- 主渠道已返回图片后发生尾部错误时不兜底。
- 主渠道和兜底成功均只产生一次计费记录。
- 日志包含正确渠道、任务 ID 和兜底原因，不包含密钥和图片内容。
- 功能关闭或配置缺失时与当前行为一致。

部署验收：

- 服务器内部可访问 ChatGPT2API 健康检查与图片任务接口。
- 实际生成一张图片时，调用详情显示 `ChatGPT2API`。
- 模拟主渠道终态失败后，调用详情显示“原生 gpt-image-2（兜底）”。
- 两条路径的用户价格与当前价格一致。
- 主渠道长任务不会触发第二张重复图片。
