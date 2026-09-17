# 故障记录（Postmortem）

本文件记录交付过程中真实发生、并在部署阶段暴露的问题，用于避免同类问题复发。
每条包含：现象、定位过程、根因、修复、防复发。

---

## INC-004 · 指标序列含 NaN 导致「HTTP 200 + 空响应体」，前端报无关错误

**首次暴露**：2026-09-17，实例详情页看「命中率」指标时报

```
Cannot read properties of undefined (reading 'series')
```

同一页面的「内存使用率」趋势则是一条不动的直线（见文末附注）。

**定位过程**

1. 前端报错点是 `result.series`，而两个调用处都写了 `result.series || []`——
   说明 `result` 本身是 `undefined`，不是 `series` 为空；
2. `undefined` 只可能来自 `request()` 里的 `body?.data`，即 **HTTP 200 但响应体为空**；
3. 后端侧只有一种写法会产生这种响应：gin 的 `c.JSON` **先写状态码、再 `json.Marshal`**，
   Marshal 失败时 body 就没了；
4. 那么是什么数据无法编码？命中率的 PromQL 是
   `rate(redis_keyspace_hits_total[5m]) / (rate(hits)+rate(misses)) * 100`，
   Redis 在该窗口内没有读写时两侧都是 0 → Prometheus 返回字符串 **`"NaN"`**；
   而 Go 的 `strconv.ParseFloat` 会**成功**解析 `"NaN"` / `"+Inf"` / `"-Inf"`，
   于是 NaN 一路进入 `[]monitor.Sample`，`encoding/json` 拒绝编码非有限数。

**根因**

数据源头的取值函数没有区分「有限数」与「Prometheus 的特殊值」，
且响应封装把「序列化失败」降级成了「200 + 空 body」——错误被静默转移给了前端，
最终以与真实原因无关的 TypeError 呈现。

**修复**

1. `internal/monitor/prometheus.go`：新增 `isFinite()`；`firstValue()` 把 NaN/±Inf
   视为 `errEmptyResult`（语义：该指标此刻没有可用数值）；`firstSeries()` 逐点跳过
   非有限值（全部跳过则为空序列，前端显示「暂无采样数据」）；
2. `internal/response/response.go`：新增 `Encode()`，`OK()` 改为**先编码成功再写响应**，
   失败即返回 500 与可读原因，不再产生「200 + 空 body」；
3. `src/api/http.ts`：`request()` 校验响应体确为 `{code,message,data}`，
   否则抛出说明性错误（原文案会让调用方在 `result.series` 处抛出无关 TypeError）；
4. 两个调用点改为 `result?.series || []` 兜底。

**防复发**

`internal/monitor/finite_test.go` 锁定 NaN/±Inf 的四种字面量与嵌套场景；
`internal/response/response_test.go` 断言含 NaN 的负载**必须编码失败**（而不是悄悄输出空）。

**附注：内存使用率为什么是一条直线**

与该缺陷无关，属于展示问题：`redis_memory_used_bytes / redis_memory_max_bytes * 100`
在 低流量 Redis 上长期在 1%~3% 之间微动，而 ECharts 的 value 轴默认
`scale:false`（从 0 起），微小波动被压缩成一条贴底直线。
已改为 `scale: true`（量程按数据自适应），并把时间轴与 tooltip 的时间格式统一为
`yyyy-MM-dd HH:mm:ss`。

---

## INC-003 · 前端白屏：手工分包造成 chunk 循环依赖触发 TDZ

**首次暴露**：2026-09-16，部署后访问页面白屏，浏览器控制台报

```
Uncaught ReferenceError: Cannot access 'Pt' before initialization
    at Ba (element-BwWDw17O.js:14:613)
    at element-BwWDw17O.js:14:1150
    at Array.map (<anonymous>)
    at ge (element-BwWDw17O.js:14:1134)
    at element-BwWDw17O.js:14:1164
```

**为什么构建阶段没有拦住**：`vue-tsc` 类型检查通过、`vite build` 成功、产物文件齐全。
TDZ 只在浏览器执行模块初始化时暴露，静态检查与打包器都不报错——这是本次最关键的教训。

**定位过程**

1. 解析全部 chunk 的 import 关系，发现双向边：

   ```
   element-BwWDw17O.js  ->  vendor-DP6k8YL2.js
   vendor-DP6k8YL2.js   ->  element-BwWDw17O.js
   ```

2. 用 CDP（Chrome DevTools Protocol）加载产物页面，拿到与用户完全一致的报错
   （文件名、行号、列号均吻合），并确认 `#app` 渲染长度为 0（确系白屏而非部分失败）。

**根因**

`vite.config.ts` 里用 `manualChunks` 把依赖强行切成 `vendor`（含 dayjs）与 `element`
（element-plus）两个 chunk，而 element-plus 的日期组件会引入 dayjs 的 locale/plugin，
两组包互相引用，形成 chunk 级循环依赖。ES module 处理循环依赖时依赖 Rollup 的变量提升，
element-plus 顶层存在「导入后立即在模块初始化期求值」的常量与数组，于是访问到了尚未初始化的绑定：

```js
// 概念示意：循环依赖下 a 尚未求值就被读取
const x = compute(X_LIST)   // X_LIST 来自尚未完成初始化的模块 → TDZ
```

**修复**

移除 `manualChunks`，交回 Rollup 按真实依赖图自动分包（它保证求值顺序）。
`rollupOptions.output` 仅保留文件名模板，不再干预 chunk 划分。
另将 `element-plus` 与 `@element-plus/icons-vue` 固定为精确版本（此前 `^2.8.8` 被 npm
解析为 2.14.5，环境之间不可复现）。

**验证**

| 指标 | 修复前 | 修复后 |
|------|--------|--------|
| `#app` 渲染长度 | 0 | 3206 |
| 文档标题 | 中间件智能问题解决平台 | 登录 · 中间件智能问题解决平台 |
| 未捕获异常 | 1（TDZ） | 0 |
| 跨 chunk 双向边 | 2 | 0 |

**防复发**

新增 `middleware-ops-web/scripts/frontend-smoke.cjs`（`npm run smoke`）：

1. 起内置静态服务器托管 `dist/`；
2. 用无头 Chrome/Edge 通过 CDP 打开首页，捕获 `console.error` 与未捕获异常；
3. 断言 `#app` 已渲染且无运行时错误，否则以非 0 退出码失败，可直接接入 CI。

可在任意已部署地址上复检：`node scripts/frontend-smoke.cjs http://<host>:8000/`。

---

## INC-002 · `alert_rules.window` 命中 PostgreSQL 保留关键字

**首次暴露**：2026-09-16，数据库初始化报

```
ERROR: syntax error at or near "window"
LINE 12: window INTEGER DEFAULT 5,
```

**根因**

- `WINDOW` 属 PostgreSQL reserved 关键字（`reserved_keywords` 类），不能作裸列名；
  而 `COUNT` / `RESULT` / `LEVEL` / `STATUS` / `KEY` / `VALUE` / `SOURCE` 属 non-reserved，裸写安全；
- 初始版本的 `deploy/postgres/init/02-schema.sql` 手写且未给标识符加引号；
- 已核对 GORM 的 `QuoteTo` 内部无条件补引号，模型侧用 `window` 也能建表，
  但初始化脚本 / 手工 SQL / BI 工具不会，故从命名上规避才根治。

**修复**

列名与 API 字段统一改为 `time_window`（`gorm:"column:time_window"`），
涉及 model / repository / service / 前端 types 与两个视图 / `docs/SCHEMA.sql` / `docs/API.md`。

**防复发**

`internal/model/schema_test.go` 新增两个用例：遍历全部模型列名与 `docs/SCHEMA.sql` 列名，
禁止命中保留字表。已用「注入 window 列」验证守卫确实会失败（非空跑）。

---

## INC-001 · 两套建表来源导致 AutoMigrate 启动失败

**首次暴露**：2026-09-16，后端启动即失败

```
auto migrate: ERROR: constraint "uni_users_username" of relation "users"
does not exist (SQLSTATE 42704)
```

**根因**

`deploy/postgres/init/` 下的脚本在后端启动前建表（且因 INC-002 中途失败），
PostgreSQL 把内联 `UNIQUE` 命名为 `users_username_key`；
而 GORM 迁移「列唯一性」时走 `migrateColumnUnique`，用 `NamingStrategy.UniqueName`
生成 `uni_users_username` 并执行 `DropConstraint` —— 名字不一致直接报 42704。

**修复**

让表结构只有一个来源：初始化脚本只创建扩展与数据库参数，不再建表；
表结构全部交给 GORM AutoMigrate；DBA 参考实现移到 `docs/SCHEMA.sql`（不参与容器初始化），
其中唯一约束按 GORM 策略显式命名。

**防复发**

`internal/model/schema_test.go` 断言模型唯一索引命名等于 `idx_<表>_<列>`、
迁移期期望名等于 `uni_<表>_<列>`，并校验 `docs/SCHEMA.sql` 与模型一致。

---

## 经验提炼

1. **"构建成功"不等于"能跑"**：类型检查与打包器都看不到运行时模块求值顺序问题，
   前端必须有「加载页面并捕获运行时错误」的检查（INC-003）。
2. **表结构只能有一个来源**：任何"顺手的初始化 SQL"都会与 ORM 迁移争夺权威（INC-001）。
3. **手写 DDL 必须考虑保留字与引号**：ORM 会自动加引号，人不会（INC-002）。
4. **回归测试要验证"能失败"**：新增守卫后用注入故障的方式确认它会红，否则可能是空跑。
