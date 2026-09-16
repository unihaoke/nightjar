# 接口文档

对应设计文档第八章。所有接口统一前缀 `/api`，统一响应结构：

```json
{ "code": 0, "message": "ok", "data": {}, "truncated": ["logs.source"] }
```

- `code=0` 表示成功；非 0 为业务错误码（400x 参数 / 401x 认证 / 403x 权限 / 404x 不存在 / 500x 系统）。
- 分页参数：`page`（默认 1）、`page_size`（默认 20，上限 100）。
- 认证：`Authorization: Bearer <JWT>`（登录同时下发 SameSite Cookie）。
- `truncated` 仅在上下文预算触发截断时出现，用于向用户透明告知被截断的维度。
- 级别标注：`L0` 只读、`L1` 低危（直接执行并留痕）、`L2` 高危（走审批）。

---

## 1. 认证与账号

| 方法 | 路径 | 权限 | 级别 | 说明 |
|------|------|------|------|------|
| POST | `/api/auth/login` | 公开 | — | 登录，返回 `{user, token, expires_at, permissions, levels, env_scope, group_scope}` |
| POST | `/api/auth/logout` | 登录 | — | 退出并清除 Cookie |
| GET | `/api/auth/profile` | 登录 | L0 | 当前用户、权限点、数据权限范围、AI 引擎状态 |
| POST | `/api/auth/password` | 登录 | L1 | 修改自己的密码（bcrypt 校验原密码） |

**登录请求**

```json
{ "username": "admin", "password": "Admin@12345" }
```

---

## 2. 中间件纳管（4.1）

| 方法 | 路径 | 权限点 | 级别 | 说明 |
|------|------|--------|------|------|
| GET | `/api/middlewares` | `middleware:read` | L0 | 列表；支持 `keyword/mw_type/environment/group` |
| GET | `/api/middlewares/options` | `middleware:read` | L0 | 类型（含默认端口与期次）、分组、环境选项 |
| POST | `/api/middlewares/test` | `middleware:write` | L0 | 对未保存参数做连接探测 |
| GET | `/api/middlewares/:id` | `middleware:read` | L0 | 详情（含规则、最近告警、指标目录） |
| POST | `/api/middlewares` | `middleware:write` | L1 | 新增实例 |
| PUT | `/api/middlewares/:id` | `middleware:write` | L1 | 更新实例（`password` 留空表示不修改） |
| DELETE | `/api/middlewares/:id` | `middleware:write` | L1（**prod 为 L2**） | 删除；prod 环境自动转审批工单，返回 `ticket_id` |
| POST | `/api/middlewares/:id/test` | `middleware:read` | L0 | 已保存实例的连接测试 |
| POST | `/api/middlewares/:id/health` | `middleware:read` | L0 | 触发即时健康探测并落库状态 |

**新增实例请求**

```json
{
  "name": "prod-redis-order",
  "mw_type": "redis",
  "host": "10.0.0.11",
  "port": 6379,
  "username": "monitor",
  "password": "******",
  "environment": "prod",
  "group_name": "payment",
  "tags": ["core"],
  "prom_job": "middleware-exporter-redis",
  "prom_instance": "10.0.0.11:6379"
}
```

> `password` 以 AES-256-GCM 加密落库，任何接口都不会返回密文或明文（仅返回 `has_password`）。

---

## 2.5 集成中心（对齐云厂商控制台的「一键集成」）

| 方法 | 路径 | 权限点 | 级别 | 说明 |
|------|------|--------|------|------|
| GET | `/api/sd/integrations` | **公开**（可选 `integration.sd_token`） | — | Prometheus `http_sd_configs` 服务发现文档（只含地址与标签，不含口令） |
| GET | `/api/integrations/overview` | `middleware:read` | L0 | 组件模板 + 已集成数量 + 产物路径 + 一键部署可用性 |
| GET | `/api/integrations` | `middleware:read` | L0 | 集成列表 |
| GET | `/api/integrations/:id` | `middleware:read` | L0 | 集成详情（含 Exporter 容器状态与查询选择器） |
| POST | `/api/integrations/preview` | `middleware:read` | L0 | **只渲染不落库**：服务发现 JSON / 显式 job / compose / docker run / 核对步骤 |
| POST | `/api/integrations/logs/preview` | `middleware:read` | L0 | **日志接入探测**：读目标容器的 docker 配置反查日志位置；**读不到则返回 400，不猜路径** |
| POST | `/api/integrations/logs` | `middleware:write` | L1 | 创建平台侧日志采集容器（复用平台镜像，`Entrypoint=mwops-agent`），并登记服务器 |
| POST | `/api/integrations` | `middleware:write` | L1 | 新建集成：纳管实例 + 服务发现更新 + 可选拉起容器 + 可选推荐告警规则 |
| PUT | `/api/integrations/:id` | `middleware:write` | L1 | 更新集成（改名会同步 instance_name 标签） |
| POST | `/api/integrations/:id/apply` | `middleware:write` | L1 | 重新应用：重写产物 + 重建 Exporter 容器 |
| DELETE | `/api/integrations/:id` | `middleware:write` | L1 | 删除集成（同时移除抓取目标与容器；历史告警/诊断保留） |

请求体与「中间件纳管」的字段一致，另加集成专属字段：

```json
{
  "name": "jd-redis",
  "mw_type": "redis",
  "address": "jd-redis:6379",
  "username": "monitor",
  "password": "******",
  "environment": "dev",
  "group_name": "interview",
  "labels": { "team": "interview" },
  "options": { "REDIS_EXPORTER_EXCLUDE_SLOWLOG_METRICS": "true" },
  "deploy": true,
  "auto_rules": true
}
```

约定（与 `internal/integration` 一一对应）：

- `name` 唯一，且是 Prometheus 的 `instance_name` 标签值（平台按它定位指标）；
- `options` 的键必须来自模板声明（`GET /api/integrations/overview` 的 `templates[].options`），
  `target=env` 落成环境变量、`target=arg` 落成命令行开关，未知键一律拒绝；
- `labels` 不允许覆盖 `job`/`instance`/`instance_name`/`mw_type`；
- 集成产物同时是一个纳管实例，`prom_job` 自动写为服务发现抓取任务名（默认 `middleware-integration`），
  `prom_instance` 恒为空。

`GET /api/sd/integrations` 是 Prometheus `http_sd_configs` 的服务发现文档，形如：

```json
[
  {
    "targets": ["mwops-exporter-jd-redis:9121"],
    "labels": { "instance_name": "jd-redis", "mw_type": "redis", "env": "dev", "team": "interview" }
  }
]
```

它注册在鉴权组之外（Prometheus 无法携带用户 JWT），只暴露地址与标签、**不含任何口令**；
需要收紧时设置 `integration.sd_token`，Prometheus 侧用 `...?token=<令牌>` 访问。

---

## 3. 统一监控（4.2）
| 方法 | 路径 | 权限点 | 级别 | 说明 |
|------|------|--------|------|------|
| GET | `/api/metrics/catalog` | `monitor:read` | L0 | 指标目录（可按 `mw_type` 过滤） |
| GET | `/api/metrics/compare` | `monitor:read` | L0 | 多实例对比（`metric` + `instance_ids`） |
| GET | `/api/metrics/:id` | `monitor:read` | L0 | 当前指标快照（含阈值与状态判定） |
| GET | `/api/metrics/:id/history` | `monitor:read` | L0 | 历史趋势（`metric`、`hours` 或 `start/end/step_seconds`） |
| GET | `/api/metrics/:id/diagnose` | `monitor:read` | L0 | **接入自检**：选择器、job 抓取状态、逐条指标命中情况与排查建议 |

指标全部来自 Prometheus（PromQL 查询），平台不建自有指标表；未配置 `prometheus.base_url` 时回退内置确定性模拟器，响应中 `source` 字段区分来源、`degraded` 标识降级。

**降级与空结果的边界（务必按此实现扩展）**：

- 只有 Prometheus **不可达/协议错误**（全部指标查询失败）才降级为模拟器；
- Prometheus 正常响应但选择器一条时序都没匹配到时，如实返回空快照，并在 `snapshot.note` 写明原因，
  **绝不用模拟数据补齐**——否则前端会画出似是而非的曲线，把接入错误掩盖掉；
- 快照附带 `selector`、`matched` / `total`、`job_up` 三个诊断字段：
  `job_up = null` 表示该 job 未被 Prometheus 配置，`job_up = 0` 表示 target 抓取失败，
  `job_up = 1` 但 `matched = 0` 则是实例名与 `instance_name` 标签对不上。

`GET /api/metrics/:id/diagnose` 响应示例（回答「为什么新增实例后没有监控/日志」）：

```json
{
  "selector": "job=\"middleware-exporter-redis\",instance_name=\"jd-redis\"",
  "job_up": 1,
  "matched": 0,
  "total": 8,
  "monitor_kind": "prometheus",
  "prometheus_healthy": true,
  "note": "Prometheus 已正常抓取 job=\"middleware-exporter-redis\"（up=1），但选择器 {...} 匹配不到时序…",
  "hints": ["抓取正常但标签对不上：把平台「实例名称」改成与 Prometheus 标签 instance_name 完全一致的值…"],
  "log_checklist": ["日志告警与中间件实例是两条独立链路：日志按「服务器 + 服务名」归集，纳管 MySQL/Redis 实例不会产生任何日志事件。"]
}
```

---

## 4. AI 诊断中心（4.3）

| 方法 | 路径 | 权限点 | 级别 | 说明 |
|------|------|--------|------|------|
| POST | `/api/ai/diagnose` | `ai:use` | L0 | **SSE 流式诊断**，事件 `meta`/`data`/`done`/`error` |
| POST | `/api/ai/diagnose/sync` | `ai:use` | L0 | 同步诊断 |
| GET | `/api/ai/diagnosis-history` | `ai:use` | L0 | 诊断历史（非管理角色仅可见自己的记录） |
| GET | `/api/ai/diagnosis/:id` | `ai:use` | L0 | 诊断详情（含沉淀的知识草稿） |
| POST | `/api/ai/diagnosis/:id/feedback` | `ai:use` | L1 | 反馈：`useful` / `useless` / `adopted` |
| GET | `/api/ai/quality` | `ai:use` | L0 | 质量基线与六道护栏参数、成本快照 |
| POST | `/api/ai/code-analyze` | `code:analyze` | L1 | AI 代码分析（三点式） |
| GET | `/api/ai/code-analyses` | `code:analyze` | L0 | 代码分析报告列表 |
| GET | `/api/ai/stream` | `ai:use` | L0 | 实时流占位（心跳 SSE） |

**诊断请求**

```json
{
  "instance_id": 12,
  "question": "Redis 最近为什么变慢了？请结合指标给出根因与处置建议",
  "mw_type": "redis",
  "alert_id": 88,
  "skip_cache": false
}
```

- `instance_id` 可省略：平台按「规则 + 实体匹配」从问题文本识别目标；命中多个候选时返回 4000 并给出候选清单。
- `alert_id` 非空时，诊断结果会回填到告警的 `diagnosis_id`。
- `skip_cache=true` 跳过确定性缓存并重新消耗 token。

**结构化报告（质量护栏强制 schema）**

```json
{
  "root_cause": "Redis 内存压力偏高，存在触发 maxmemory 策略并伴随淘汰的风险",
  "confidence": 0.72,
  "evidence": [
    { "source": "metric", "ref": "memory_usage_percent", "detail": "当前 92.4%，超过临界阈值 85%", "speculative": false }
  ],
  "suggestions": [
    { "action": "核对 maxmemory-policy 与写入模式", "horizon": "immediate", "risk": "低", "level": "L0" }
  ],
  "impact_scope": "当前实例及其上游写入路径；淘汰会放大下游数据库负载",
  "pending_confirm": ["确认是否存在无 TTL 的大 key"],
  "speculative": false,
  "low_confidence": false,
  "missing_dimensions": [],
  "ai_available": true,
  "grounded_ratio": 1
}
```

`ai_available=false` 表示引擎降级（规则引擎半自动结论），此时 `engine_note` 说明降级原因。

**SSE 事件流**

```text
event: meta
data: {"instance_id":12,"instance_name":"prod-redis-order","engine_name":"third_party:deepseek-chat",
       "engine_status":"ok","cache_hit":false,"truncated":[],"missing":["config.read（工具调用超时）"],
       "prompt_tokens":3120,"scope":"角色=ops 环境=prod 分组=payment","data_source":"prometheus"}

event: data
data: {"delta":"{\"root_cause\":\"Redis 内存压力"}

event: done
data: { ...完整 DiagnosisResponse... }

event: error
data: {"code":5002,"message":"AI 引擎不可用"}
```

---

## 5. 告警治理（4.4）

| 方法 | 路径 | 权限点 | 级别 | 说明 |
|------|------|--------|------|------|
| GET | `/api/alerts` / `/api/alerts/history` | `alert:read` | L0 | 告警历史（`instance_id/mw_type/level/status/cluster_id/from/to`） |
| GET | `/api/alerts/options` | `alert:read` | L0 | 规则配置选项与通知渠道状态 |
| GET | `/api/alerts/rules` | `alert:read` | L0 | 规则列表 |
| POST | `/api/alerts/rules` | `alert:write` | L1 | 创建规则 |
| PUT | `/api/alerts/rules/:id` | `alert:write` | L1 | 更新规则 |
| DELETE | `/api/alerts/rules/:id` | `alert:write` | L1 | 删除规则 |
| POST | `/api/alerts/evaluate` | `alert:write` | L1 | 手动触发一轮阈值评估 |
| POST | `/api/alerts/cluster` | `alert:write` | L1 | 触发离线语义聚类（仅合并展示，不改状态） |
| POST | `/api/alerts/:id/ack` | `alert:write` | L1 | 确认告警 |
| POST | `/api/alerts/:id/resolve` | `alert:write` | L1 | 标记恢复 |
| POST | `/api/alerts/notify-test?channel=feishu` | `alert:write` | L1 | 通知渠道自检 |

**创建规则请求**

```json
{
  "name": "Redis 内存使用率过高",
  "instance_id": 12,
  "mw_type": "redis",
  "metric_name": "memory_usage_percent",
  "operator": ">",
  "threshold": 85,
  "level": "critical",
  "time_window": 5,
  "cooldown": 10,
  "notify_channels": ["feishu", "wecom"],
  "ai_enabled": true,
  "description": "内存水位超过 85% 时告警并自动诊断"
}
```

- `operator` 支持 `>` `>=` `<` `<=` `==` `!=`（阈值型指标用前者，命中率型指标用 `<`）。
- 收敛：`time_window` 分钟内同指纹（规则 + 实例 + 级别）合并为一条并累加 `count`；`cooldown` 分钟内静默不外发通知。
- 字段名为 `time_window` 而非 `window`：后者是 PostgreSQL 保留关键字，裸写会触发语法错误，因此从字段名到数据库列名统一规避。

---

## 6. 知识库（4.5）

| 方法 | 路径 | 权限点 | 级别 | 说明 |
|------|------|--------|------|------|
| GET | `/api/knowledge` | `knowledge:read` | L0 | 列表（`keyword/mw_type/status/source/tag/only_published`） |
| GET | `/api/knowledge/stats` | `knowledge:read` | L0 | 状态分布与采纳率 |
| GET | `/api/knowledge/options` | `knowledge:read` | L0 | 状态与来源选项 |
| GET | `/api/knowledge/:id` | `knowledge:read` | L0 | 详情 |
| POST | `/api/knowledge` | `knowledge:write` | L1 | 新增（人工录入默认 `published`） |
| PUT | `/api/knowledge/:id` | `knowledge:write` | L1 | 更新（草稿转正：`status=published`） |
| POST | `/api/knowledge/:id/adopt` | `knowledge:write` | L1 | 采纳（累加采纳数并回写诊断反馈） |
| DELETE | `/api/knowledge/:id` | `knowledge:write` | L1 | 删除 |

质量闭环：AI 诊断沉淀的条目 `source=auto`、`status=draft`，**不参与向量检索**；人工确认转正后参与检索，且检索时按采纳率加权（低采纳率降权）。

---

## 7. 修复建议与执行（4.6）

| 方法 | 路径 | 权限点 | 级别 | 说明 |
|------|------|--------|------|------|
| GET | `/api/fix/options` | `fix:preview` | L0 | 动作目录、分级说明、SQL 校验参数、执行器说明 |
| POST | `/api/fix/preview` | `fix:preview` | L0 | 影响预览 + 级别判定 + 高危识别 |
| POST | `/api/fix/execute` | `fix:execute` | L1/L2 | L1 直接执行；L2 创建审批工单或凭已批准工单执行 |
| GET | `/api/fix/history` | `fix:preview` | L0 | 修复历史 |
| POST | `/api/fix/validate-sql` | `sql:read` | L0 | SQL 只读安全校验（返回规范化语句与高危判定） |

**动作分级**

| 动作 | 级别 |
|------|------|
| `view_metrics`、`run_readonly_sql` | L0 |
| `ack_alert` | L1 |
| `clean_redis_key`、`restart_service`、`update_config`、`sql_write` | L2（未知动作按 L2 兜底） |

**执行请求**

```json
{
  "instance_id": 12,
  "action_type": "restart_service",
  "params": { "command": "systemctl restart redis" },
  "reason": "内存泄漏需重启释放",
  "dry_run": false,
  "ticket_id": "AP9f2c31ab77d0"
}
```

- 无 `ticket_id` 时：L2 返回 `status=pending_approval` 与新工单号（30 分钟未审批自动拒绝）。
- 带 `ticket_id` 时：仅当工单状态为 `approved` 才执行，执行结果回填工单 `exec_result`。

---

## 8. 审批（6.2）

| 方法 | 路径 | 权限点 | 级别 | 说明 |
|------|------|--------|------|------|
| GET | `/api/approvals` | `approval:read` | L0 | 工单列表（`status/environment/mine`） |
| GET | `/api/approvals/:id` | `approval:read` | L0 | 工单详情 |
| POST | `/api/approvals/:id/decide` | `approval:decide` | L2 | 审批决策（申请人与审批人不得为同一人） |

```json
{ "approved": true, "comment": "已确认影响范围，同意执行" }
```

---

## 9. 审计（4.7 / 6.4）

| 方法 | 路径 | 权限点 | 级别 | 说明 |
|------|------|--------|------|------|
| GET | `/api/audit/logs` | `audit:read` | L0 | 审计日志（`user_id/instance_id/action_type/level/result/keyword/from/to`） |
| GET | `/api/audit/logs/:id` | `audit:read` | L0 | 详情（含哈希链前后值） |
| GET | `/api/audit/verify` | `audit:read` | L0 | 校验哈希链完整性（`from_id`/`to_id` 可选） |
| GET | `/api/audit/snapshots` | `audit:read` | L0 | 快照列表 |
| POST | `/api/audit/snapshot` | `audit:snapshot` | L1 | 生成当日哈希链快照（落盘并校验） |

平台**不提供**审计日志的修改/删除接口（只追加语义）。

---

## 10. 日志告警与代码分析（4.8）

| 方法 | 路径 | 权限点 | 级别 | 说明 |
|------|------|--------|------|------|
| GET | `/api/log-alerts/events` | `logalert:read` | L0 | 日志事件列表 |
| GET | `/api/log-alerts/events/:id` | `logalert:read` | L0 | 事件详情 + 代码分析报告 |
| PUT | `/api/log-alerts/events/:id/status` | `logalert:write` | L1 | 更新状态（pending/analyzing/resolved/ignored） |
| GET/POST/PUT/DELETE | `/api/log-alerts/servers[/:id]` | `logalert:read` / `server:manage` | L1 | 服务器实例管理 |
| GET/POST | `/api/log-alerts/code-repos` | `logalert:read` / `server:manage` | L1 | 服务→仓库映射与**出网白名单开关** |
| POST | `/api/hooks/logs` | Hook 令牌 | — | 日志上报（应用 HTTP Hook / Agent） |
| POST | `/api/hooks/alerts` | Hook 令牌 | — | 外部告警推送（Alertmanager / 自定义） |

**日志上报请求**

```json
{
  "service": "order-service",
  "level": "ERROR",
  "message": "Order 10086 处理失败",
  "stacktrace": "java.lang.NullPointerException\n\tat com.demo.OrderService.process(OrderService.java:42)",
  "context_lines": "…错误前后 20 行…",
  "alert_type": "stack",
  "server_ip": "10.0.0.21"
}
```

响应中的 `signature` 为错误指纹（异常类名 + 消息模板去变量 + 首个业务栈帧），`merged=true` 表示与 5 分钟窗口内的既有事件合并。

**Hook 鉴权**：请求头 `X-Hook-Token: <MWOPS_HOOK_TOKEN>`（或 `?token=`）；平台未配置令牌时不校验（仅建议内网单机使用）。

---

## 11. 系统与大盘（8.2）

| 方法 | 路径 | 权限点 | 级别 | 说明 |
|------|------|--------|------|------|
| GET | `/api/system/overview` | `system:overview` | L0 | 大盘（实例健康、告警趋势、诊断质量与成本、平台自检） |
| GET | `/api/system/info` | 登录 | L0 | 系统信息、AI 引擎与降级说明、基础设施、能力矩阵、护栏参数 |
| GET | `/api/system/config` | `system:config` | L0 | 脱敏后的运行配置 |
| GET | `/healthz` | 公开 | — | 健康检查 |
| GET | `/metrics` | 公开 | — | 平台自身 Prometheus 指标 |

---

## 12. 用户与角色

| 方法 | 路径 | 权限点 | 级别 | 说明 |
|------|------|--------|------|------|
| GET | `/api/users` | `user:manage` | L0 | 用户列表 |
| POST | `/api/users` | `user:manage` | L1 | 新增用户（含数据权限范围） |
| PUT | `/api/users/:id` | `user:manage` | L1 | 更新用户（`password` 非空则重置密码） |
| DELETE | `/api/users/:id` | `user:manage` | L1 | 删除用户（禁止删除自己） |
| GET | `/api/roles` | `user:manage` | L0 | 角色列表与权限点目录 |
| PUT | `/api/roles/:id` | `user:manage` | L1 | 更新自定义角色（内置角色不可改） |

---

## 13. 错误码速查

| 错误码 | HTTP | 含义 |
|--------|------|------|
| 4000 / 4001 / 4002 | 400 | 参数不合法 / 请求体解析失败 / 查询参数不合法 |
| 4003 | 400 | SQL 未通过只读安全校验 |
| 4010 / 4011 / 4012 / 4013 / 4014 | 401 / 403 | 未认证 / 令牌过期 / 令牌无效 / 凭据错误 / 账号禁用 |
| 4030 / 4031 / 4032 / 4033 / 4034 / 4035 | 403 | 无权限 / 超出数据权限 / 级别不允许 / 需要审批 / 出网被拒 / 配额用尽 |
| 4040 | 404 | 资源不存在 |
| 5000 / 5001 / 5002 / 5003 / 5004 | 500 / 502 / 503 / 504 | 内部错误 / 上游异常 / 引擎不可用 / 超时 / 队列已满 |
