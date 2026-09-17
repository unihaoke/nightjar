# 故障记录（Postmortem）

本文件记录交付过程中真实发生、并在部署阶段暴露的问题，用于避免同类问题复发。
每条包含：现象、定位过程、根因、修复、防复发。

---

## INC-006 · docker 的 Go 模板串被 Ansible 当 Jinja 渲染，远程安装第一步即失败

**首次暴露**：2026-09-18，修完 INC-005 重建镜像后，同一操作换了一种报错：

```
TASK [校验目标机器上的 Docker 可用] ***
fatal: [203.195.191.75]: FAILED! => {"msg": "template error while templating string:
  unexpected '.'. String: docker version --format '{{.Server.Version}}'. unexpected '.'"}
```

**定位过程**

1. 好消息是 INC-005 已修好：这次是 `PLAY` / `TASK` 正常展开，说明 YAML 解析通过、
   镜像也已重建——报错发生在**执行**阶段而不是加载阶段；
2. 失败的第一个任务就是「校验目标机器上的 Docker 可用」，目标机的 SSH、sudo、Python
   都还没被考验到；
3. 报错句子本身给了答案：`template error while templating string`。
   playbook 是 Ansible 的 Jinja 模板，**每个值**在交给模块前都会先渲染一遍；
   `{{.Server.Version}}` 是 docker 的 **Go 模板**语法，行首的点号在 Jinja 里不是合法表达式
   → `unexpected '.'`；
4. YAML 层面它完全合法（`{{` 不在标量起始位置），PyYAML 实测也照过；
   INC-005 新增的「渲染后自校验」因此抓不到它——**校验 YAML 合法 ≠ 校验 Jinja 合法**。

**根因**

渲染器把"给 docker 看的模板串"和"给 Ansible 看的模板串"混在了同一个字符串里。
这类跨层字符串（YAML → Jinja → docker/Go 模板 → shell）每多一层就多一次转义语义，
而当时的模板没有任何针对 Jinja 层的守卫。

**修复**

1. 该校验任务改为 `command -v docker` + 裸 `docker version`：既拿到绝对路径
   （systemd 单元的 `ExecStart` 必须是绝对路径），又不再产生 `{{.`；
   **注意必须用 `ansible.builtin.shell`**——`command -v` 是 shell 内建命令，而
   `ansible.builtin.command` 不经 shell（直接 execvp），会以
   `No such file or directory: b'command'` 失败；顺带把账号 SQL 里两处同样的
   `command -v mysql` / `command -v docker` 探测一并改为 `shell`（它们在客户端缺失时
   还会让注册变量没有 `rc`，后续 `when: account_client.rc != 0` 直接报"字典没有该属性"）；
2. `playbook_validate.go` 增加 Jinja 层检查：扫描未被 `{% raw %}…{% endraw %}` 包裹的
   `{{.X}}` 并报「第 N 行含 Go 模板语法」，渲染阶段就把问题挡在平台侧；
3. 顺带修掉同一轮审计发现的两处相邻缺陷：
   - `docker-systemd` 模式的 `ExecStart=docker run …` 用的是相对路径，
     systemd 会以 "Executable path is not absolute" 拒绝加载单元 → 改用 `command -v` 的结果；
   - `binary` 模式的 `INSTALLED_FROM` 用单引号包多行文本，YAML 会把换行**折叠成空格**
     （PyYAML 实测：两行挤成 `url: … version: …`），改为块标量 `content: |`。

**防复发**

1. `TestRenderedPlaybooksHaveNoGoTemplate`：全组件 × 全安装方式扫描产物里的裸 `{{.`；
2. `TestCommandModuleAvoidsShellFeatures`：断言 `ansible.builtin.command` 的值里不出现
   shell 内建与 `|` `>` `;` `&&` 等元字符（[command 模块不经 shell](https://docs.ansible.com/ansible/latest/collections/ansible/builtin/command_module.html)）；
3. `TestSystemdUnitUsesAbsoluteExecPath`：断言每个 `Exec*` 的首个 token 是绝对路径或解析出的变量；
4. `TestPlaybookHasNoMultilineQuotedScalar`：断言引号标量都在同一行闭合（防 YAML 折叠）；
5. `TestValidatePlaybookYAML` 增加 Go 模板样例（并断言 `{% raw %}` 包裹后可通过）；
6. 以上守卫逐个用「注入原始缺陷」验证确实会红，不是空跑；
7. 审计方法固化：渲染器单测只能证明"我们渲染得对"，证明不了"下游工具怎么读"。
   本轮改用**下游解析器交叉验证**（PyYAML 复核全部产物）+ 人工逐行读渲染结果 +
   查下游官方文档确认语义，一次性找出 4 个同类问题，详见 `deploy/ansible/README.md` §6.4。

---

## INC-005 · 生成的 playbook 中裸 `{{ }}` 让远程安装整体失败，且报错与根因无关

**首次暴露**：2026-09-18，在「集成中心」对 `jd-redis` 勾选「创建只读账号并拉起 Exporter」时报

```
创建只读账号并拉起 Exporter失败：Ansible 执行失败：exit status 4
（输出：ERROR! We were unable to read either as JSON nor YAML ...
  Syntax Error while loading YAML.
  found unacceptable key (unhashable type: 'AnsibleMapping')
The error appears to be in '/app/data/integrations/ansible/jd-redis.yml': line 33 ...
        port: {{ exporter_port }}
                    ^ here ）
```

**定位过程**

1. 报错来自 `ansible.builtin.wait_for` 的 `port:` 字段——问题不在 SSH、不在目标机、
   也不在那台 Redis；playbook **连解析都没通过**，所以「建号 + 装 Exporter」全流程一步没走；
2. YAML 规范里，标量位置以 `{` 开头即 flow mapping 起始，`{{ exporter_port }}` 会被解析成
   「以 mapping 为 key 的 mapping」，PyYAML 拒绝 → ansible 抛 exit 4；
3. 模板里同一文件其余 `{{ }}` 都出现在 `docker pull {{ exporter_image }}` 这类
   **行中**位置（合法），只有这一处落在值起始位置，因此此前从未暴露。

**根因**

`internal/integration/ansible.go` 用字符串拼接生成 YAML，`renders` 时直接写了
`port: ` + `{{ exporter_port }}`，既没有加引号，也没有任何「生成物是否合法」的校验；
模板一旦写错，只能在**别的机器上**由 ansible 以一句与根因无关的报错反馈。

**修复**

1. `port: '{{ exporter_port }}'`——所有以 `{{` 开头的 YAML 值统一走 `yamlScalar()` 加引号；
2. 新增 `internal/integration/playbook_validate.go`：渲染完成后**先自己解析**一遍
   （先扫「裸 `{{` 起始值」，再用 `gopkg.in/yaml.v3` 做真正的语法解析），
   失败就返回「第 N 行 … 不是合法 YAML」并附原文，属于平台缺陷、不再让使用者看天书；
3. playbook 第 3 行写入渲染器版本戳 `# 渲染器: mwops-playbook v2`，
   `/healthz` 同时暴露 `playbook_renderer` 字段——用于区分「模板有缺陷」与
   **「后端镜像没重建、仍跑旧渲染器」**（本次现场即为后者：
   `docker compose up -d` 不会重建已存在的镜像，必须 `docker compose build backend`）。

**防复发**

1. `TestRenderedPlaybooksQuoteJinjaValues`：遍历全部组件 × 全部安装方式 + 账号 SQL 产物，
   扫描「冒号后的值以裸 `{{` 开头」的行；
2. `TestValidatePlaybookYAML`：直接给校验器喂坏样例，断言**能**报出带行号的错误，
   并断言行中出现 `{{`（shell 命令）不会被误判——守卫必须验证「能失败」，不是空跑；
3. `TestPlaybookRendererVersionStamped`：锁定版本戳存在且在第 3 行；
4. 排障步骤写入 `deploy/ansible/README.md` §6.1（先 `curl /healthz` 看渲染器版本）。

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
5. **生成给外部工具消费的文件必须自校验、并带版本戳**：playbook / SQL / 配置一旦交给
   ansible、mysql 这类外部程序，错误只会在**别的机器上**以难懂的形式返回；
   渲染后先本地解析一遍，把「第几行、原文、为什么错」直接还给使用者（INC-005）。
   同时产物要写渲染器版本戳，否则「模板有缺陷」与「镜像/产物是旧的」无法区分——
   后者在现场排查中占了大半时间。
6. **跨层字符串要逐层验证，测试要用下游的解析器**：playbook 同时被 YAML、Jinja、
   docker/Go 模板、shell 四层解析，一层合法不代表下一层合法（INC-006：
   YAML 合法的 `{{.Server.Version}}` 被 Jinja 拒绝）。只测"我渲染出的字符串对不对"
   是自证；要拿**下游真正使用的解析器**复核（PyYAML/`--syntax-check`），
   并至少在一个真机上跑通一次完整流程。INC-005 与 INC-006 是同一个缺陷的两次暴露，
   说明当时只补了"这一行"而没补"这一类"。
