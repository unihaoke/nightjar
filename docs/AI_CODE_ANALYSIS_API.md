# AI 代码分析接口文档

> 版本：v1（2026-09-23）
> 文档定位：本文档是**外部 AI 代码分析服务**（下称 AI 服务，部分报文/控制台中称 CodeAgent）对外提供的
> OpenAPI v1 契约。nightjar 平台本身不实现这些接口，而是作为调用方（客户端）按本契约接入；
> 平台侧的配置与验收步骤见[附录 B](#附录-b平台侧接入验收清单)。
> 适用对象：希望将「异常栈」自动转为「根因 + 补丁 + 报告」并推送告警（如飞书）的调用方服务。
> 能力概述：调用方只需提供 **git 地址** 或 **主机标识** 即可定位仓库，无需感知 AI 服务内部 `repoId`；
> 支持 **同步等待结果** 与 **异步回调** 两种接入方式。

---

## 1. 接入概览

| 模式 | 接口 | 行为 | 适用场景 |
|------|------|------|----------|
| 同步 | `POST /api/v1/openapi/analyze` | 提交后阻塞等待分析完成，直接返回结构化结果 | 调用方需要立即拿到根因/补丁（如实时告警富化） |
| 异步 | `POST /api/v1/openapi/tasks` | 提交即返回 `runId`，分析完成时回调 `callbackUrl` | 分析耗时较长、调用方不想阻塞（如监控告警风暴） |

两种模式共享相同的**仓库定位**与**分析结果结构**。

---

## 2. 鉴权

所有接口使用 **API Key** 鉴权（适合调用方服务长期持有）。API Key 以 `ca_` 开头（生产环境形如 `ca_live_xxx`），在 AI 服务控制台「租户与权限 → 接入密钥」中创建。

推荐放在请求头：

```http
X-API-Key: <your_api_key>
```

支持的认证方式（任选其一）：

| 方式 | 用法 | 适用场景 |
|---|---|---|
| 请求头 | `X-API-Key: ca_live_xxx` | REST 调用（推荐） |
| 请求头 | `Authorization: ApiKey ca_live_xxx` | 与 Bearer 风格统一时 |
| 请求头 | `Authorization: Bearer <jwt>` | 控制台管理员登录（JWT） |
| 查询参数 | `?apiKey=ca_live_xxx` 或 `?token=<jwt>` | WebSocket / SSE 等无法自定义头时 |

- 密钥绑定到具体租户与主体，租户由密钥隐式确定。
- 所需权限：`task:write`（`admin:all` 可通配放行）。缺少权限返回 `403`。
- 未携带或无效密钥返回 `401`。
- 生产环境应关闭 AI 服务的匿名模式（`allowAnonymous`），由上游网关统一鉴权。

---

## 3. 公共约定

- **Base URL**：`https://<你的服务域名>`（由部署决定，下文以 `<BASE>` 代指）。
- **请求头**：`Content-Type: application/json`。
- **租户隔离**：API Key 隐式绑定租户，第三方无需在 body 中传租户 ID。
- **幂等**：建议携带 `idempotencyKey`；相同 key 在有效期内重复提交会复用同一分析任务，避免告警风暴触发多次分析。
- **错误码**：

| HTTP | 含义 | 处理建议 |
|------|------|----------|
| 400 | 请求参数不合法（如缺 `stacktrace`、异步缺 `callbackUrl`、栈过大） | 检查请求体 |
| 401 | 未认证（缺/错 API Key） | 补充 `X-API-Key` |
| 403 | 无权限 | 申请 `task:write` 权限 |
| 404/422 | 仓库定位失败（无匹配仓库） | 先在控制台注册仓库并配置 `matchRules` |
| 429 | 配额/队列超限 | 退避后重试 |
| 202 | 同步超时（结果未就绪）或异步受理 | 见各接口说明 |
| 200 | 成功 | — |

- **栈大小限制**：`stacktrace` 单字段上限 `maxStacktraceBytes`（默认 64KB），超出返回 `400`。

---

## 4. 仓库定位（repoLocator）

调用方通过三种方式之一指定目标仓库（互斥，**优先级 `repoId` > `groupId` > `repoLocator`**）：

| 字段 | 类型 | 说明 |
|------|------|------|
| `repoLocator.gitUrl` | string | 仓库 git 地址，如 `https://git.x/order.git` |
| `repoLocator.host` | string | 主机/服务标识，如 `order-svc`、`order.internal` |
| `repoId` | string | 系统内部仓库 ID 或 `repoKey`（已知时直接传，最精确） |
| `groupId` | string | 分组 ID，走「分组多仓」分析模式 |

若只给 `repoLocator`，服务端在租户内仓库列表按以下规则**打分取最高分**：

| 命中规则 | 分值 |
|---|---|
| `gitUrl` 精确匹配 `repo.url` | 100 |
| 命中 `matchRules.hostPatterns`（如 `order-svc`） | 80 |
| `gitUrl` 片段命中 `repo.url` | 70 |
| 命中 `matchRules.endpointPatterns` | 60 |
| 命中 `matchRules.keywords` | 50 |
| `host`/`gitUrl` 片段命中 `repo.key`/`repo.name` | 40 |

- 无匹配 → 返回 `422`，提示「请先在控制台注册仓库并配置 `matchRules`」。
- 多个仓库同分 → 取首个，并在响应 `warnings` 中提示实际选用了哪个 `key`。

> 推荐：控制台注册仓库时填写 `hostPatterns`/`keywords`，可让第三方仅凭 `host` 稳定命中。

---

## 5. 接口一：同步分析 `POST /api/v1/openapi/analyze`

提交异常栈并**阻塞等待**分析完成，直接返回结构化结果。

### 5.1 请求

**Header**
```http
POST /api/v1/openapi/analyze
Content-Type: application/json
X-API-Key: <your_api_key>
```

**Body 字段**

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `repoLocator` | object | 三选一 | `{ "gitUrl"?, "host"? }` |
| `repoId` | string | 三选一 | 内部仓库 ID / repoKey |
| `groupId` | string | 三选一 | 分组 ID（分组多仓模式） |
| `stacktrace` | string | 是 | 异常栈文本（≤ maxStacktraceBytes） |
| `logs` | string | 否 | 关联日志，辅助定位 |
| `entryFiles` | string[] | 否 | 嫌疑入口文件，缩小范围 |
| `environment` | string | 否 | 环境标识，如 `prod` |
| `autoVerify` | bool | 否 | 是否自动跑沙箱验证（默认按系统配置） |
| `priority` | int | 否 | 任务优先级 |
| `idempotencyKey` | string | 否 | 幂等键，防重复分析 |
| `timeout` | int | 否 | 同步等待上限（秒），默认 120，上限 300 |

### 5.2 请求示例

```bash
curl -X POST '<BASE>/api/v1/openapi/analyze' \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: <your_api_key>' \
  -d '{
    "repoLocator": { "gitUrl": "https://git.x/order.git" },
    "stacktrace": "java.lang.NullPointerException\n\tat com.acme.order.OrderService.create(OrderService.java:42)",
    "logs": "trace_id=abc123 timeout=2000ms",
    "environment": "prod",
    "idempotencyKey": "alert-2026-001",
    "timeout": 120
  }'
```

### 5.3 响应：分析完成（HTTP 200）

| 字段 | 类型 | 说明 |
|------|------|------|
| `runId` | string | 运行 ID |
| `taskId` | string | 任务 ID |
| `status` | string | 终态：`succeeded` / `needs_review` / `failed` / `degraded` / `cancelled` |
| `severity` | string | 严重程度 `critical`/`high`/`medium`/`low`/`unknown` |
| `repo` | object | `{ id, key, name, url }` 命中的仓库 |
| `rootCause` | object | 根因分析（见 §5.5） |
| `patches` | object[] | 候选补丁（见 §5.5） |
| `verification` | object | `{ passed, checks[], errors[] }` 沙箱验证结果 |
| `reportId` | string | 报告 ID（可后续拉取） |
| `reportUrl` | string | 报告跳转链接（由 `PUBLIC_URL` 生成） |
| `markdown` | string | 完整报告 Markdown，可直接贴飞书/工单 |
| `elapsedMs` | int | 耗时（毫秒） |
| `usage` | object | 模型 token 用量 |
| `warnings` | string[] | 定位/分析过程提示 |
| `degraded` | bool | 是否降级（部分能力不可用） |
| `error` | string | 失败原因（仅 `failed`/`degraded` 时有值） |

**200 响应示例**
```json
{
  "runId": "run_01HX...",
  "taskId": "task_01HX...",
  "status": "succeeded",
  "severity": "critical",
  "repo": { "id": "r1", "key": "order-service", "name": "订单服务", "url": "https://git.x/order.git" },
  "rootCause": {
    "summary": "OrderService.create 在未判空的情况下解引用了 order 对象",
    "category": "null_pointer",
    "confidence": 0.92,
    "detail": "入参 order 在并发场景下可能为 null …",
    "evidence": [],
    "blastRadius": []
  },
  "patches": [
    {
      "repositoryId": "r1",
      "repoKey": "order-service",
      "filePath": "service/order.go",
      "action": "modify",
      "unifiedDiff": "--- a/service/order.go\n+++ b/service/order.go\n@@ …",
      "rationale": "增加空值保护，避免 NPE"
    }
  ],
  "verification": { "passed": true, "checks": [], "errors": [] },
  "reportId": "rep_01HX...",
  "reportUrl": "https://console.x/r/rep_01HX...",
  "markdown": "# 分析报告 …",
  "elapsedMs": 12345,
  "usage": {},
  "warnings": [],
  "degraded": false,
  "error": ""
}
```

### 5.4 响应：同步超时（HTTP 202）

当分析在 `timeout` 内未完成，返回 `202` 并提示轮询或使用回调：

```json
{
  "runId": "run_01HX...",
  "taskId": "task_01HX...",
  "status": "running",
  "pollUrl": "/api/v1/runs/run_01HX...",
  "message": "分析未在超时内完成，请轮询 pollUrl 或提供 callbackUrl 等待回调"
}
```

> 若调用方在请求中携带了 `callbackUrl`，即使同步超时，分析完成后仍会异步回调该地址（见 §6）。

---

## 6. 接口二：异步提交 `POST /api/v1/openapi/tasks`

提交即返回受理结果，分析完成（终态）时**回调** `callbackUrl`。

### 6.1 请求

Body 与同步接口基本一致，但 **`callbackUrl` 为必填**（否则返回 `400`）。

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `callbackUrl` | string | 是 | 终态回调地址，服务端将以 `POST` 推送结构化结果 |
| 其余字段 | — | — | 同 §5.1（不含 `timeout`） |

### 6.2 请求示例

```bash
curl -X POST '<BASE>/api/v1/openapi/tasks' \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: <your_api_key>' \
  -d '{
    "repoLocator": { "host": "order-svc" },
    "stacktrace": "java.lang.NullPointerException …",
    "callbackUrl": "https://hook.your-svc.com/codeagent/callback"
  }'
```

### 6.3 受理响应（HTTP 202）

```json
{
  "runId": "run_01HX...",
  "taskId": "task_01HX...",
  "status": "queued",
  "acceptTime": "2026-09-23T10:00:00Z"
}
```

### 6.4 终态回调

分析进入终态后，服务端以 **`POST {callbackUrl}`** 推送结果：

- **Content-Type**：`application/json`
- **重试**：回调失败采用指数退避重试**最多 3 次**（间隔 1s / 2s）；全部失败会记录审计（`action=callback.failed`），调用方可通过 `GET /api/v1/runs/{runId}` 主动拉取。
- **签名（可选，按密钥开启）**：默认不签名；当发起任务的 API Key 在控制台开启了「回调签名鉴权」后，回调会附带
  `X-Callback-Signature` / `X-Callback-Key-Id` / `X-Callback-Timestamp` 三个 HMAC-SHA256 签名头，
  接收方按[附录 A](#附录-a回调签名鉴权hmac)验签即可确认来源可信、报文完整且未被重放。
  未开启签名时，接收方可自行校验来源 IP 或在 `callbackUrl` 中加自定义 token 查询参数。

**回调报文结构**

| 字段 | 类型 | 说明 |
|------|------|------|
| `runId` | string | 运行 ID |
| `state` | string | 终态：`succeeded` / `needs_review` / `failed` / `degraded` / `cancelled` |
| `reportId` | string | 报告 ID |
| `severity` | string | 严重程度 |
| `summary` | string | 根因摘要 |
| `tenantId` | string | 租户 ID |
| `taskId` | string | 任务 ID |
| `attempt` | int | 重试次数 |
| `error` | string | 失败原因（可选） |
| `rootCause` | object | 完整根因（同 §5.5） |
| `patches` | object[] | 补丁列表（同 §5.5） |
| `reportUrl` | string | 报告跳转链接 |

**回调报文示例**
```json
{
  "runId": "run_01HX...",
  "state": "succeeded",
  "reportId": "rep_01HX...",
  "severity": "critical",
  "summary": "OrderService.create 未判空解引用",
  "tenantId": "t-demo",
  "taskId": "task_01HX...",
  "attempt": 1,
  "error": "",
  "rootCause": {
    "summary": "OrderService.create 在未判空的情况下解引用了 order 对象",
    "category": "null_pointer",
    "confidence": 0.92,
    "detail": "…",
    "evidence": [],
    "blastRadius": []
  },
  "patches": [
    {
      "repositoryId": "r1",
      "repoKey": "order-service",
      "filePath": "service/order.go",
      "action": "modify",
      "unifiedDiff": "--- a/service/order.go\n+++ …",
      "rationale": "增加空值保护"
    }
  ],
  "reportUrl": "https://console.x/r/rep_01HX..."
}
```

> 飞书渲染建议：用 `markdown` + `reportUrl` 生成「查看完整报告」按钮；用 `rootCause.summary`/`category` 作为卡片标题，`patches[].filePath` 列出受影响文件。若异步回调未携带 `markdown`，可用 `reportUrl` 在飞书卡片中以链接形式展示。

---

## 7. 配置说明（服务端）

- **`PUBLIC_URL`**（环境变量）或 `server.publicUrl`（JSON 配置）：对外可访问的基础地址（含协议与端口，如 `https://console.x`）。
  - 用于生成 `reportUrl`（`{PUBLIC_URL}/r/{reportId}`），让第三方在飞书/工单中点击跳转到完整报告。
  - 未配置时 `reportUrl` 为空，但 `reportId` 仍返回，调用方可自行拼装。

---

## 8. 对接检查清单

1. 申请 API Key，确认具备 `task:write` 权限。
2. 在控制台注册目标仓库，并填写 `hostPatterns`/`keywords`，使第三方仅凭 `host` 即可命中。
3. 同步场景：调用 `/api/v1/openapi/analyze`，设置合理 `timeout`（建议 ≤120）。
4. 异步场景：调用 `/api/v1/openapi/tasks` 并填 `callbackUrl`，做好 `POST` 接收与失败重放（轮询 `GET /api/v1/runs/{runId}`）。
5. 服务端配置 `PUBLIC_URL`，确保回调/响应中的 `reportUrl` 可点击。

---

## 9. 相关参考

- 回调 HMAC 验签：见[附录 A](#附录-a回调签名鉴权hmac)
- nightjar 平台侧配置与验收：见[附录 B](#附录-b平台侧接入验收清单)
- 平台对接的另一档「通用协议」（`question` / `task_id` / `Authorization: Bearer`）形状不同，不在本文档范围
- 仓库匹配规则字段（AI 服务侧）：`domain.RepoMatchRules`（hostPatterns / endpointPatterns / keywords / …）
- 根因与补丁模型（AI 服务侧）：`domain.RootCause` / `domain.Patch`

---

## 附录 A：回调签名鉴权（HMAC）

> 本附录说明接收方如何验证 AI 服务的终态回调。设计目标：在不暴露密钥的前提下，让接收方能够
> **验证回调确实来自 AI 服务（来源可信）**、**报文未被篡改（完整性）**、**请求未被重放（时效性）**。

### A.1 开启方式

在 AI 服务控制台「接入密钥」创建时勾选「回调签名鉴权」，并（可选）填写**允许回调 Host 白名单**
（如 `hook.your-svc.com`、`*.inner.net`）。

创建成功后，接口会**一次性**返回 `callbackSecret`（明文仅出现这一次，请立即保存；服务端只存储其密文）。它就是回调验签的共享密钥。

```json
{
  "plainKey": "ca_live_xxx...",
  "apiKey": { "id": "key-xxx", "callbackEnabled": true, "...": "..." },
  "callbackSecret": "K8sV2...40位随机串...",
  "callbackNotice": "回调签名密钥仅此一次返回，请配置到对端服务；服务端仅保存密文。"
}
```

### A.2 回调请求结构

AI 服务以 `POST` 向 `callbackUrl` 发送任务终态报文，并附带三个签名头：

| Header | 示例 | 含义 |
|---|---|---|
| `X-Callback-Signature` | `sha256=AbC1..._3` | HMAC-SHA256 签名值（URL-safe Base64，**无填充**） |
| `X-Callback-Key-Id` | `key-xxx` | 发起任务的接入密钥 ID，用于对端按 ID 查对应 `callbackSecret` |
| `X-Callback-Timestamp` | `1710000000` | Unix 秒级时间戳，用于防重放与过期校验 |

### A.3 签名算法

```
签名串 = timestamp + "." + rawRequestBody

signature = HMAC-SHA256(secret, 签名串)
X-Callback-Signature = "sha256=" + BASE64URL_RAW(signature)   // RFC 4648 §5，无填充
```

关键约定：

- **`secret`**：创建密钥时返回的 `callbackSecret`（仅一次）。
- **`timestamp`**：即 `X-Callback-Timestamp` 头的原始字符串值。
- **`rawRequestBody`**：HTTP 请求体的**原始字节**。验签必须使用**收到的原始字节**
  （例如 `request.get_data()` / `req.body` 的二进制 / `Buffer`），
  **不要二次 `JSON.stringify` 后再签名**，否则字节可能因空格 / key 顺序不同而不一致。
- **Base64 形态**：使用 `URL-safe Base64` 且**去掉尾部 `=` 填充**
  （Go `base64.RawURLEncoding` / Node `digest('base64url')` / Python `urlsafe_b64encode(...).rstrip('=')`）。
  跨语言比较时，建议对两边签名值都先去除 `=` 填充再比对，避免编码差异。

### A.4 验签步骤（务必按顺序）

1. **取时间戳**：读取 `X-Callback-Timestamp`，转整数。
2. **防重放 / 过期**：校验 `|now - timestamp| <= 300`（秒，建议窗口 5 分钟）。超出直接拒绝。
3. **取密钥**：用 `X-Callback-Key-Id` 定位到本地保存的 `callbackSecret`。
4. **重算签名**：`expected = BASE64URL_RAW( HMAC-SHA256(secret, timestamp + "." + rawBody) )`。
5. **恒定时间比对**：去掉 `X-Callback-Signature` 的 `sha256=` 前缀，与 `expected` 做**恒定时间比较**
   （Python `hmac.compare_digest`、Node `crypto.timingSafeEqual`、Go `hmac.Equal`）。
6. **全部通过**才认为回调合法，继续业务处理。

> 第 4–5 步保证报文任意字节被改动都会使签名失配；时间戳窗口保证同一签名无法在窗口外复用，
> `timestamp` 本身也参与签名串，无法被单独篡改。

### A.5 验签代码示例

#### Python（Flask / FastAPI 通用）

```python
import hmac, hashlib, base64, time

def verify_callback(raw_body: bytes, headers: dict, secret: str, window: int = 300) -> bool:
    ts = headers.get("X-Callback-Timestamp")
    if not ts or not ts.isdigit():
        return False
    if abs(int(time.time()) - int(ts)) > window:        # 防重放 / 过期
        return False
    sig_header = headers.get("X-Callback-Signature", "")
    if not sig_header.startswith("sha256="):
        return False
    got = sig_header[len("sha256="):].rstrip("=")         # 去填充，跨语言对齐
    msg = (ts + ".").encode() + raw_body
    expected = base64.urlsafe_b64encode(
        hmac.new(secret.encode(), msg, hashlib.sha256).digest()
    ).decode().rstrip("=")
    return hmac.compare_digest(expected, got)             # 恒定时间比较
```

> 注意：务必用 `request.get_data()`（原始字节）而非已解析的 `request.json`，后者是二次序列化结果。

#### Go

```go
func verifyCallback(body []byte, hdr map[string]string, secret string, window time.Duration) bool {
	ts := hdr["X-Callback-Timestamp"]
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if math.Abs(float64(time.Now().Unix()-tsInt)) > window.Seconds() { // 防重放 / 过期
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	got := strings.TrimPrefix(hdr["X-Callback-Signature"], "sha256=")
	return hmac.Equal([]byte(expected), []byte(got)) // 恒定时间比较
}
```

#### Node.js

```js
const crypto = require('crypto');

function verifyCallback(rawBody, headers, secret, windowSec = 300) {
  const ts = headers['x-callback-timestamp'];
  if (!ts) return false;
  if (Math.abs(Math.floor(Date.now() / 1000) - parseInt(ts, 10)) > windowSec) return false; // 防重放
  const msg = ts + '.' + rawBody; // rawBody 为 Buffer
  const expected = crypto.createHmac('sha256', secret).update(msg).digest('base64url'); // 无填充
  const got = (headers['x-callback-signature'] || '').replace(/^sha256=/, '');
  return crypto.timingSafeEqual(Buffer.from(expected), Buffer.from(got));
}
```

#### Java

```java
boolean verify(byte[] body, Map<String,String> hdr, String secret, long windowSec) throws Exception {
    String ts = hdr.get("X-Callback-Timestamp");
    long delta = Math.abs(Instant.now().getEpochSecond() - Long.parseLong(ts));
    if (delta > windowSec) return false;                       // 防重放 / 过期
    Mac mac = Mac.getInstance("HmacSHA256");
    mac.init(new SecretKeySpec(secret.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
    mac.update((ts + ".").getBytes(StandardCharsets.UTF_8));
    mac.update(body);
    String expected = Base64.getUrlEncoder().withoutPadding().encodeToString(mac.doFinal());
    String got = hdr.get("X-Callback-Signature").replaceFirst("^sha256=", "");
    return expected.equals(got);                              // 生产建议用恒定时间比较
}
```

### A.6 AI 服务侧安全控制（接收方无需实现但应知晓）

- **SSRF 防护**：发送回调前，AI 服务解析 `callbackUrl` 的 host，拒绝内网 / 保留地址段
  （`127.x`、`10.x`、`172.16-31.x`、`192.168.x`、`169.254.x`、链路本地等）。
- **Host 白名单**：若接入密钥配置了 `callbackHosts`，仅放行匹配的 host（支持 `*.example.com` 后缀通配）；内网地址即使通配也需显式放行。
- **跳过即告警**：目标 host 未授权或属内网时不发送回调，并写入审计日志（`callback.skipped`）。
  若始终收不到回调，请检查密钥上配置的 Host 白名单是否包含回调域名。

### A.7 安全最佳实践

1. **`callbackSecret` 等同密码**：仅保存在服务端配置 / 密钥管理系统中，切勿进入前端代码、Git 仓库或日志。
2. **始终校验时间戳窗口**，避免重放攻击。
3. **恒定时间比对**签名，避免时序侧信道泄露。
4. **HTTPS 接入**：回调地址使用 HTTPS，防止签名头在链路上被嗅探。
5. **按 `X-Callback-Key-Id` 轮换**：可对不同对端 / 环境使用不同接入密钥，实现密钥隔离与独立吊销。
6. **业务幂等**：签名相同不代表重复执行业务，接收方应依据回调报文中的任务 ID / `idempotencyKey` 做幂等去重。

### A.8 常见问题

**Q：能否把签名放在 URL 的 `?token=` 而不是 Header？**
A：采用 Header 而非 URL query，因为 query 参数会被网关、代理、访问日志完整记录，存在泄露风险。Header 形态更安全。

**Q：为什么是 Base64 而非 Hex？**
A：URL-safe 无填充 Base64 在 header 中传输更紧凑、兼容性更好，且避免 `+` `/` 在部分解析器中的问题。

**Q：重放窗口内同一请求到达两次怎么办？**
A：接收方依据回调报文中的任务 ID / `idempotencyKey` 做幂等去重，签名只负责验证来源与完整性。

---

## 附录 B：平台侧接入验收清单

> 适用：把 nightjar 平台的日志告警 AI 分析接到本文件正文描述的 OpenAPI v1。
> 协议开关：`AI 设置 → AI 代码分析 → 对接协议 = 开放接口 v1（repoLocator / stacktrace）`。
> 另一档 `通用协议` 是平台自研形状（`question` / `task_id` / `Authorization: Bearer`），本附录不适用。

验收思路是**先把外部服务验通，再验平台**：一半的“平台侧故障”其实是 AI 服务侧的仓库没注册或密钥没权限，
直接跳过第一步会让排查变成在平台里瞎猜。

### B.0 前置清单

| # | 项 | 谁负责 | 完成标志 |
|---|---|---|---|
| 1 | AI 服务可访问（`https://<AI 域名>`） | AI 服务方 | 平台容器能 `curl` 通 |
| 2 | 创建 API Key，权限含 `task:write` | AI 服务方 | 拿到密钥明文 |
| 3 | 在 AI 服务控制台注册目标仓库，填 `hostPatterns` / `keywords` | AI 服务方 | 仅凭服务名能定位到仓库 |
| 4 | 配置 AI 服务的 `PUBLIC_URL` | AI 服务方 | 报告链接可点击 |
| 5 | 平台对外地址能被 AI 服务访问到（回调要用） | 平台运维 | AI 服务方能 `curl` 通平台域名 |
| 6 | 出网白名单放行该服务 | 平台运维 | `MWOPS_SECURITY_OUTBOUND_WHITELIST` 含服务名（默认空 = 全禁） |

### B.1 先绕开平台，直接验 AI 服务（5 分钟）

用正文 §5 的同步接口打一发。**这一步不通过，后面全都不用看。**

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
| `200` + `runId` / `rootCause` | 通了 | 记下 `repo.key`，继续 B.2 |
| `401` | 密钥无效或未携带 | 换 key / 确认权限 `task:write` |
| `404` / `422` | 仓库定位失败 | 回控制台注册仓库并配 `hostPatterns`（服务名要与日志里的 `service` **完全一致**，大小写不敏感） |
| `400` | 缺 `stacktrace` 或栈超 `maxStacktraceBytes`（默认 64KB） | 检查请求体 / 截断堆栈 |
| `429` | 配额或队列超限 | 降频后重试 |

### B.2 平台侧配置

**AI 设置（系统设置 → AI 设置 → AI 代码分析）**

| 字段 | 填什么 |
|---|---|
| 启用 | 开 |
| 服务地址 Base URL | `https://<AI 域名>` |
| API Key | B.1 验过的那把 |
| **对接协议** | 开放接口 v1（repoLocator / stacktrace） |
| **鉴权头** | `X-API-Key` |
| 提交路径 | `/api/v1/openapi/tasks`（切协议后自动带出） |
| 同步提交路径 | `/api/v1/openapi/analyze`（仅 `call_mode=sync` 用） |
| 查询路径 | `/api/v1/runs/{run_id}` |
| 回调地址 | **异步模式必填**：`https://<平台域名>`；要自带校验参数时写 `https://<平台域名>/{path}?token=xxx`。详见 B.5 |
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

**通知渠道（系统设置 → 通知渠道）**：至少一个渠道启用并**发送测试**成功。
卡片上才会出现「查看详情 / 确认」与「查看完整报告」两个按钮。

**出网白名单**：

```bash
# .env
MWOPS_SECURITY_OUTBOUND_WHITELIST=order-service,pay-service
```

默认空 = **禁止任何外发**。没放行时事件状态是 `disabled`（不是 `failed`），原因写明“不在出网白名单内”。

**日志告警规则（日志告警 → 日志告警规则）**：平台**没有默认规则**：没命中任何已启用规则 = 不入库、不通知、不分析。

- `服务名`：与日志的 `service` 一致（留空 = 任意服务）
- `消息包含` / `/正则/`：匹配消息原文
- `级别≥`：`ERROR`
- **AI 分析：开**
- `通知渠道`：勾选验过的渠道

同时确认**屏蔽规则**里没有把这条错误挡掉（屏蔽项优先于所有规则，命中即完全丢弃）。

### B.3 打一条测试日志

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

### B.4 逐环节验收

后处理扫描间隔默认 15 秒（`LOG_ALERT_WORKER_INTERVAL`），所以第 ② 步之后要等十几秒。

| # | 环节 | 在哪看 | 通过标准 |
|---|---|---|---|
| ① | 入库 | 日志告警 → 事件列表 | 出现该事件，且「命中规则」是所建的那条 |
| ② | 提交 | 事件「AI 分析」列 | 由 `待分析` 变 `AI 分析中`（`awaiting`）；后端日志 `已提交 AI 分析任务` / `日志告警已提交 AI 分析` |
| ③ | 外部服务受理 | AI 服务侧 | 出现对应任务；平台库里 `ai_analysis_tasks` 有该行，且 `run_id` **非空** |
| ④ | 结论回来 | 事件「AI 分析」列 | 变 `已分析`（`done`） |
| ⑤ | 通知送达 | 飞书/企微群 | 卡片带根因、修复建议、代码位置、置信度，且有两个按钮 |
| ⑥ | 报告可跳 | 事件详情 → 代码分析报告 | 有「完整报告 → 在 AI 服务查看」链接；卡片按钮能打开报告页 |
| ⑦ | 幂等 | 重复上报同一条错误 | 不应再产生一次新的外部分析（同一 `idempotencyKey` 被服务端复用） |

> ③ 的 `run_id` 是关键：它为空说明响应解析没走到开放接口分支，轮询会查不到任务，
> 事件会一直停在 `AI 分析中` 直到 `task_timeout`（默认 30 分钟）判超时。

### B.5 故障速查

**事件停在 `AI 分析中`，最后变 `分析失败`**

看事件的**分析状态说明**（`analysis_error`），平台把外部服务的返回写在这里：

| 说明里的文字 | 根因 | 处理 |
|---|---|---|
| `仓库定位失败：请先在 AI 服务控制台注册该仓库并配置 hostPatterns/keywords` | HTTP 404/422 | 回 B.1，确认服务名与仓库的 `hostPatterns` 对得上；或在平台配「服务名→git 地址」映射 / 「服务名→repoId」映射 |
| `API Key 无效或未携带：请确认「鉴权头」填的是 X-API-Key 且密钥正确` | HTTP 401 | 鉴权头没配成 `X-API-Key`，或密钥错了 |
| `密钥缺少 task:write 权限` | HTTP 403 | 换一把有权限的 key |
| `请求参数不合法：常见原因是缺 stacktrace、异步模式缺 callbackUrl，或栈超过 maxStacktraceBytes` | HTTP 400 | 检查堆栈是否超 64KB；异步模式必须能生成回调地址 |
| `配额或队列超限` | HTTP 429 | 降频（调大规则的去重窗口与冷却期） |
| `AI 服务返回成功但没有结论内容` | 200 但没有 `rootCause` / `patches` / `markdown` | 让 AI 服务方确认终态响应体 |
| `同步模式下 AI 服务未在等待时间内返回结论` | `call_mode=sync` 且服务端回了 202 | 改用 async，或调大「同步等待上限」（≤300s） |
| `AI 分析超时（超过 30m0s 仍未返回结论）` | 回调没到、轮询也没查到 | 见下「回调与轮询」 |

**回调与轮询**

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

> **异步模式下这项是必填的**：开放接口把 `callbackUrl` 定为异步提交的必填字段（正文 §6.1），
> 留空会被服务端以 400 拒绝——这与通用协议“留空就只靠轮询”的行为不同。

> **查询参数里的令牌会落进 Nginx 访问日志**。能用请求头就用请求头；`?token=` 只在 AI 服务不支持自定义头时使用。

另外两点：

- 地址必须是 **AI 服务能访问到的**地址（不是 `localhost`，也不是只能内网访问的管理地址）；
- 走 Nginx 时 `/api/` 有 `limit_req 10r/s burst=20 nodelay`，回调量远低于此，但**告警风暴 + 回调重试**叠加时理论上可能被限流——若发现回调偶发失败，优先看 Nginx 的限流日志。
- 轮询兜底：`GET https://<AI 域名>/api/v1/runs/{run_id}`，间隔 `poll_interval`（默认 60s）。
- 两者都不通时的现象就是“卡在 `AI 分析中` 直到超时”。先确认 AI 服务能访问到平台的回调地址。
- 若 AI 服务侧为该 API Key 开启了 HMAC 回调签名（附录 A），平台回调接收方同样需要按附录 A 验签逻辑校验签名头。

**事件状态是 `未启用`（disabled）而不是 `失败`**

这是**配置结果**，不是故障：

- `该规则未启用 AI 分析…` → 把规则的「AI 分析」打开，或对该条点「重新分析」；
- `服务 X 不在出网白名单内，已跳过 AI 分析` → 加到 `MWOPS_SECURITY_OUTBOUND_WHITELIST`；
- `平台未装配 AI 分析能力…` → AI 设置里没启用或地址为空。

**完全没产生事件**

按这个顺序查：① 屏蔽项命中；② 没命中任何已启用规则（平台没有默认规则）；③ 处于冷却期（事件「抑制」列有值，
`cooldown_until` 未过）；④ Hook 401（令牌不一致）。

### B.6 回归验证

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
