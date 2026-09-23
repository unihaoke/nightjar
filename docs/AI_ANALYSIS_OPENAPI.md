# 日志告警接入「AI 代码分析开放接口 v1」验收清单

> 适用：把平台的日志告警 AI 分析接到《AI 代码分析接口文档 v1》（`AI代码分析接口文档.md`）。
> 协议开关：`AI 设置 → AI 代码分析 → 对接协议 = 开放接口 v1（repoLocator / stacktrace）`。
> 另一档 `通用协议` 是平台自研形状（`question` / `task_id` / `Authorization: Bearer`），本文不适用。

验收思路是**先把外部服务验通，再验平台**：一半的"平台侧故障"其实是 AI 服务侧的仓库没注册或密钥没权限，
直接跳过第一步会让排查变成在平台里瞎猜。

---

## 0. 前置清单

| # | 项 | 谁负责 | 完成标志 |
|---|---|---|---|
| 1 | AI 服务可访问（`https://<AI 域名>`） | AI 服务方 | 平台容器能 `curl` 通 |
| 2 | 创建 API Key，权限含 `task:write` | AI 服务方 | 拿到密钥明文 |
| 3 | 在 AI 服务控制台注册目标仓库，填 `hostPatterns` / `keywords` | AI 服务方 | 仅凭服务名能定位到仓库 |
| 4 | 配置 AI 服务的 `PUBLIC_URL` | AI 服务方 | 报告链接可点击 |
| 5 | 平台对外地址能被 AI 服务访问到（回调要用） | 平台运维 | AI 服务方能 `curl` 通平台域名 |
| 6 | 出网白名单放行该服务 | 平台运维 | `MWOPS_SECURITY_OUTBOUND_WHITELIST` 含服务名（默认空 = 全禁） |

---

## 1. 先绕开平台，直接验 AI 服务（5 分钟）

用文档 §5 的同步接口打一发。**这一步不通过，后面全都不用看。**

```bash
curl -i -X POST 'https://<AI 域名>/api/v1/openapi/analyze' \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: <your_api_key>' \
  -d '{
    "repoLocator": { "host": "order-service" },
    "stacktrace": "java.lang.NullPointerException\n\tat com.acme.order.OrderService.create(OrderService.java:42)",
    "environment": "prod",
    "timeout": 120
  }'
```

| 返回 | 含义 | 处理 |
|---|---|---|
| `200` + `runId` / `rootCause` | 通了 | 记下 `repo.key`，继续第 2 步 |
| `401` | 密钥无效或未携带 | 换 key / 确认权限 `task:write` |
| `404` / `422` | 仓库定位失败 | 回控制台注册仓库并配 `hostPatterns`（服务名要与日志里的 `service` **完全一致**，大小写不敏感） |
| `400` | 缺 `stacktrace` 或栈超 `maxStacktraceBytes`（默认 64KB） | 检查请求体 / 截断堆栈 |
| `429` | 配额或队列超限 | 降频后重试 |

---

## 2. 平台侧配置

### 2.1 AI 设置（系统设置 → AI 设置 → AI 代码分析）

| 字段 | 填什么 |
|---|---|
| 启用 | 开 |
| 服务地址 Base URL | `https://<AI 域名>` |
| API Key | 第 1 步验过的那把 |
| **对接协议** | 开放接口 v1（repoLocator / stacktrace） |
| **鉴权头** | `X-API-Key` |
| 提交路径 | `/api/v1/openapi/tasks`（切协议后自动带出） |
| 同步提交路径 | `/api/v1/openapi/analyze`（仅 `call_mode=sync` 用） |
| 查询路径 | `/api/v1/runs/{run_id}` |
| 回调地址 | **异步模式必填**：`https://<平台域名>`；要自带校验参数时写 `https://<平台域名>/{path}?token=xxx`。详见 §5.2 |
| 回调令牌 | 与上面 URL 里的 `token` 一致（不一致会被 401 拒收，改由轮询兜底） |
| 仓库定位方式 | 服务名（当 host，默认）／服务名→git 地址映射 ／ 服务名→repoId 映射 |
| 环境标识 | 如 `prod`（可选） |
| 调用方式 | async（推荐） |
| 任务超时 / 轮询间隔 / 每轮条数 | `30m` / `60s` / `20`（按需） |

> 协议从「通用」切到「开放接口 v1」时，**没被手动改过的**提交/查询路径会自动改写；
> 你自己填过的路径一律保留——所以切换后请扫一眼这两个框。

填完先点卡片底部的**「测试连接」**（不需保存，用当前表单值 + 已存密钥的合并结果探测）。判定口径：

| 结果 | 含义 |
|---|---|
| 连接成功（2xx） | 地址、鉴权、报文形状都对 |
| 连接成功（404 / 422） | 服务可达且鉴权通过；探针服务 `connectivity-probe` 未注册属预期，真实告警会带实际服务名 |
| 连接失败 + `API Key 无效或未携带…` | 鉴权头没填 `X-API-Key`，或密钥错 |
| 连接失败 + `密钥缺少 task:write 权限` | 换一把有权限的 key |
| 连接失败 + `连接失败：…` | 地址不通 / DNS / 出网被拦 |

测试会向服务端提交一次**极小的分析请求**（同步端点、10 秒上限），不写回调、不落报告。

### 2.2 通知渠道（系统设置 → 通知渠道）

至少一个渠道启用并**发送测试**成功。卡片上才会出现「查看详情 / 确认」与「查看完整报告」两个按钮。

### 2.3 出网白名单

```bash
# .env
MWOPS_SECURITY_OUTBOUND_WHITELIST=order-service,pay-service
```

默认空 = **禁止任何外发**。没放行时事件状态是 `disabled`（不是 `failed`），原因写明"不在出网白名单内"。

### 2.4 日志告警规则（日志告警 → 日志告警规则）

平台**没有默认规则**：没命中任何已启用规则 = 不入库、不通知、不分析。

- `服务名`：与日志的 `service` 一致（留空 = 任意服务）
- `消息包含` / `/正则/`：匹配消息原文
- `级别≥`：`ERROR`
- **AI 分析：开**
- `通知渠道`：勾选第 2.2 步验过的渠道

同时确认**屏蔽规则**里没有把这条错误挡掉（屏蔽项优先于所有规则，命中即完全丢弃）。

---

## 3. 打一条测试日志

最快的方式是 Hook 直推（不用等 Filebeat）：

```bash
curl -sS -X POST http://<平台地址>/api/hooks/logs \
  -H "X-Hook-Token: $MWOPS_HOOK_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "service": "order-service",
    "level": "ERROR",
    "message": "Order 10086 处理失败",
    "stacktrace": "java.lang.NullPointerException\n\tat com.acme.order.OrderService.create(OrderService.java:42)"
  }'
```

`service` / `level` / `message` 必填；`stacktrace` 建议带上（开放接口下它是主输入，缺失时会用 `message` 顶上）。

---

## 4. 逐环节验收

后处理扫描间隔默认 15 秒（`LOG_ALERT_WORKER_INTERVAL`），所以第 ② 步之后要等十几秒。

| # | 环节 | 在哪看 | 通过标准 |
|---|---|---|---|
| ① | 入库 | 日志告警 → 事件列表 | 出现该事件，且「命中规则」是第 2.4 步建的那条 |
| ② | 提交 | 事件「AI 分析」列 | 由 `待分析` 变 `AI 分析中`（`awaiting`）；后端日志 `已提交 AI 分析任务` / `日志告警已提交 AI 分析` |
| ③ | 外部服务受理 | AI 服务侧 | 出现对应任务；平台库里 `ai_analysis_tasks` 有该行，且 `run_id` **非空** |
| ④ | 结论回来 | 事件「AI 分析」列 | 变 `已分析`（`done`） |
| ⑤ | 通知送达 | 飞书/企微群 | 卡片带根因、修复建议、代码位置、置信度，且有两个按钮 |
| ⑥ | 报告可跳 | 事件详情 → 代码分析报告 | 有「完整报告 → 在 AI 服务查看」链接；卡片按钮能打开报告页 |
| ⑦ | 幂等 | 重复上报同一条错误 | 不应再产生一次新的外部分析（同一 `idempotencyKey` 被服务端复用） |

> ③ 的 `run_id` 是关键：它为空说明响应解析没走到开放接口分支，轮询会查不到任务，
> 事件会一直停在 `AI 分析中` 直到 `task_timeout`（默认 30 分钟）判超时。

---

## 5. 故障速查

### 5.1 事件停在 `AI 分析中`，最后变 `分析失败`

看事件的**分析状态说明**（`analysis_error`），平台把外部服务的返回写在这里：

| 说明里的文字 | 根因 | 处理 |
|---|---|---|
| `仓库定位失败：请先在 AI 服务控制台注册该仓库并配置 hostPatterns/keywords` | HTTP 404/422 | 回第 1 步，确认服务名与仓库的 `hostPatterns` 对得上；或在平台配「服务名→git 地址」映射 / 「服务名→repoId」映射 |
| `API Key 无效或未携带：请确认「鉴权头」填的是 X-API-Key 且密钥正确` | HTTP 401 | 鉴权头没配成 `X-API-Key`，或密钥错了 |
| `密钥缺少 task:write 权限` | HTTP 403 | 换一把有权限的 key |
| `请求参数不合法：常见原因是缺 stacktrace、异步模式缺 callbackUrl，或栈超过 maxStacktraceBytes` | HTTP 400 | 检查堆栈是否超 64KB；异步模式必须能生成回调地址 |
| `配额或队列超限` | HTTP 429 | 降频（调大规则的去重窗口与冷却期） |
| `AI 服务返回成功但没有结论内容` | 200 但没有 `rootCause` / `patches` / `markdown` | 让 AI 服务方确认终态响应体 |
| `同步模式下 AI 服务未在等待时间内返回结论` | `call_mode=sync` 且服务端回了 202 | 改用 async，或调大「同步等待上限」（≤300s） |
| `AI 分析超时（超过 30m0s 仍未返回结论）` | 回调没到、轮询也没查到 | 见 5.2 |

### 5.2 回调与轮询

平台侧的回调接收接口是**已实现**的：`POST /api/ai/analysis/callback`（注册在公开路由，不需要登录态）。

| 项 | 行为 |
|---|---|
| 鉴权 | 依次尝试：请求头 `X-Callback-Token` → `Authorization: Bearer` → 查询参数 `?callback_token=` / `?token=`，与「回调令牌」字段常量时间比对 |
| 令牌未配置 | **拒收所有回调**——宁可走轮询兜底，也不让任何人往平台里写结论 |
| 幂等 | 按「当前状态必须是 submitted」做条件更新；重复回调（AI 服务重试 3 次）不会写出两份报告 |
| 成功响应 | `200 {"code":0,...}`，AI 服务收到 2xx 即停止重试 |

**回调地址怎么填**：填平台对外的**基础地址**，平台会自动补上 `/api/ai/analysis/callback`。

```
https://platform.example.com                       → https://platform.example.com/api/ai/analysis/callback
https://platform.example.com:8000                  → https://platform.example.com:8000/api/ai/analysis/callback
https://platform.example.com/{path}?token=abc123   → https://platform.example.com/api/ai/analysis/callback?token=abc123
```

三种情况都不会重复拼接：地址已以该路径结尾时原样使用；含 `{path}` 占位时按占位替换；否则追加。

> **异步模式下这项是必填的**：开放接口把 `callbackUrl` 定为异步提交的必填字段（文档 §6.1），
> 留空会被服务端以 400 拒绝——这与通用协议"留空就只靠轮询"的行为不同。

> **查询参数里的令牌会落进 Nginx 访问日志**。能用请求头就用请求头；
> `?token=` 只在 AI 服务不支持自定义头时使用（文档 §6.4 明确建议的兜底方式）。

另外两点：

- 地址必须是 **AI 服务能访问到的**地址（不是 `localhost`，也不是只能内网访问的管理地址）；
- 走 Nginx 时 `/api/` 有 `limit_req 10r/s burst=20 nodelay`，回调量远低于此，但**告警风暴 + 回调重试**叠加时理论上可能被限流——若发现回调偶发失败，优先看 Nginx 的限流日志。

- 轮询兜底：`GET https://<AI 域名>/api/v1/runs/{run_id}`，间隔 `poll_interval`（默认 60s）。
- 两者都不通时的现象就是"卡在 `AI 分析中` 直到超时"。先确认 AI 服务能访问到平台的回调地址。

### 5.3 事件状态是 `未启用`（disabled）而不是 `失败`

这是**配置结果**，不是故障。两种原因，说明里写得很明确：

- `该规则未启用 AI 分析…` → 把规则的「AI 分析」打开，或对该条点「重新分析」；
- `服务 X 不在出网白名单内，已跳过 AI 分析` → 加到 `MWOPS_SECURITY_OUTBOUND_WHITELIST`；
- `平台未装配 AI 分析能力…` → AI 设置里没启用或地址为空。

### 5.4 完全没产生事件

按这个顺序查：① 屏蔽项命中；② 没命中任何已启用规则（平台没有默认规则）；③ 处于冷却期（事件「抑制」列有值，
`cooldown_until` 未过）；④ Hook 401（令牌不一致）。

---

## 6. 回归验证

改动这套协议后，至少跑一遍：

```bash
cd middleware-ops && gofmt -l . && go vet ./... && go test ./...
cd ../middleware-ops-web && npm run build
```

重点用例（`internal/service`）：

| 用例 | 钉住什么 |
|---|---|
| `TestOpenAPISubmitAsync` | 异步端点、`X-API-Key` 头、`stacktrace` / `callbackUrl` / `idempotencyKey` / `repoLocator` 字段 |
| `TestOpenAPISubmitSyncUsesAnalyzeEndpoint` | 同步模式走 `/api/v1/openapi/analyze` |
| `TestOpenAPIParseResult` | 终态映射（含 `needs_review` / `degraded` / `cancelled`）与结论合成 |
| `TestOpenAPICallback` | 回调报文（`state` / `summary` / `rootCause`）解析 |
| `TestOpenAPIQueryUsesRunID` | 轮询按 `{run_id}` 拼路径 |
| `TestProtocolSwitchRewritesPaths` | 切协议时改路径，但保留自定义路径 |
| `TestNotifyLogEventCarriesReportButton` | 飞书卡片有「查看完整报告」按钮 |
