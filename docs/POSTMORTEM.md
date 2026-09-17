# 故障记录（Postmortem）

本文件记录交付过程中真实发生、并在部署阶段暴露的问题，用于避免同类问题复发。
每条包含：现象、定位过程、根因、修复、防复发。

---

## INC-015 · 核验只在部署后跑 3 次，外部修好后状态永远停在「待处理」

**首次暴露**：2026-09-18，使用者反馈：

> 「但是现在 redis 已经能够正确被监控了还是显示待处理。」

**定位过程**

1. 「待处理」= `meta.LastError` 非空。写入它的只有两条路径：部署失败（`runAsync`）与**核验失败**
   （`scheduleVerify` 第 3 次仍未通过）；
2. 读 `scheduleVerify`：核验是 **go 协程里跑 3 次**（约 35s / 60s / 85s），成功即 `markApplied`
   （清错误），3 次都失败就 `markError` 并**结束**——之后再没有任何东西会重新核对；
3. 也就是说：**修好外部原因的时刻如果晚于这 85 秒，状态就永久停在待处理**，
   除非使用者再点一次「重新应用」——而远程集成的重新应用要重填 SSH 凭据、还会真的重装，
   代价与"再看一眼"完全不成比例；
4. 顺带发现两个相关缺陷：
   - `probeIntegration` 在 **Prometheus 不可达**时返回空串（等同于"通过"），
     于是核验过程中恰好读不到 Prometheus，会把待处理**错误地清掉**（假通过）；
   - 匹配目标时，没有 `instance_name` 标签的目标会被直接当作本实例，多个集成时会张冠李戴。

**根因**

把"核验"设计成**一次性收尾动作**，而不是**可持续收敛的状态**。状态一旦落地就没有任何
自我纠正机制；同时"无法判定"与"通过"用了同一个返回值，语义被压扁。

**修复**

1. `probeIntegration` 改返回 `(原因, 是否已判定)`：Prometheus 读不到时是"无法判定"，
   调用方**不得据此改状态**（部署后的核验与自愈都遵守）；
2. 新增 **「重新核验」**（`POST /api/integrations/:id/verify`）：零代价、不需要凭据、
   无副作用，按 Prometheus 现状刷新状态；集成列表行与待处理横幅都有按钮；
3. 新增**周期自愈**（`ReverifyIntegrations`，挂在健康巡检节拍上）：只处理当前处于
   「待处理」的集成，且只有**明确看到目标 `up`** 才清除错误；Prometheus 不可达或目标缺失
   一律保持原状——只做"由坏变好"，绝不凭空造错；
4. `pickTargetStatus`：优先 `instance_name` 精确匹配；只有在"job 下唯一且该目标没有
   instance_name"时才兜底认领，且**唯一但名字是别人的绝不认领**。

**防复发**

1. `TestPickTargetStatusPrefersInstanceName` / `RefusesToGuess` / `SingleTargetFallback`：
   覆盖精确匹配、多目标无标签（不猜）、名字对不上（不认领）、唯一无标签（兜底）；
2. `TestReverifyOnlyClearsOnExplicitUp`：`up` 才清、`down`/`unknown`/缺失/只有别的集成 up 都不清；
   ——**这组测试在实现过程中当场抓到过一个真 bug**：我最初写的"唯一目标兜底"会把别的集成
   当成自己，从而清掉别人的待处理（正是最危险的错法）；
3. 文档：`docs/INTEGRATION.md` 6.2 把入口分工改成三列（重新核验 / 重新应用 / 重试建号），
   并写明"待处理先点重新核验"。

---

## INC-014 · 账号重试把凭据拆成三个弹框，最后一刻才校验 → 点了等于没点

**首次暴露**：2026-09-18，使用者原话：

> 「去重试和测试连接好像没啥用啊？是不是应该先弹出框，然后填完信息，在重试弹框里点击再触发请求才对啊？」

**定位过程**

1. 读前端实现，旧流程是：
   点「重试建号」→ 弹确认框（由平台重建 / 只测连接）→ 弹「管理员账号」→ 弹「管理员口令」
   → **最后**才调用 `ensureSshCreds(row)` 检查**抽屉顶部**的 SSH 字段；
2. 若那两栏没填，函数直接 return，只弹一条 toast：使用者走完三个弹框，最后一个请求都没发出去；
3. 同样的模式出现在「测试连接」「轮换口令」「删除账号」四个入口上——凭据被拆在
   "抽屉顶部" 与 "系统弹框" 两处，用户必须先完成 A 再点 B 才能成功，A 又不在 B 的流程里。
4. 同时暴露两个状态语义问题：
   - 远程部署的 Exporter 容器在目标机上，平台拿不到 `container_status`，界面却显示「未托管」
     （其实是正常的，只是平台"看不到"）；
   - 「待处理」标签只有文案没有原因，要查得滚到底部横幅或猜是哪一条。

**根因**

把"收集输入"与"触发请求"拆成了两个互不知情的位置：输入散落、校验滞后。
用户的心智模型是「点按钮 → 弹窗 → 填完 → 在弹窗里确认」，而不是「先在外面填好，再点按钮」。

**修复**

1. 新增「账号操作」弹窗：**一个弹窗收齐本次动作需要的全部凭据**
   （远程→SSH 口令/私钥；建号/删号→管理员账号口令），并说明"本次将要执行什么"；
   校验通过后由弹窗内的主按钮触发请求，结果也显示在弹窗里；四个入口（测试连接 /
   重试建号 / 轮换口令 / 删除账号）统一走它，原来的一串 `ElMessageBox.prompt` 全部删除；
2. 抽屉顶部的 SSH 输入区移除，改为一句提示（凭据在弹窗里填、仅本次使用、不落库）；
3. Exporter 列：远程部署显示「远程（目标机）」+ 提示（容器归那台机器管、状态看抓取指标），
   不再误报「未托管」；
4. 状态列的「待处理」标签加 tooltip，直接把 `last_error` 原文显示出来，不必去底部找。

**防复发**

1. 交互约定写进 `docs/INTEGRATION.md` 6.2：**凭据必须先在一个弹窗内收齐，再由该弹窗的按钮触发**；
2. 「不需要账号」（Redis 等）与「未托管」（平台看不到的远程容器）在文档与界面上分别说明，
   避免使用者把"不需要"当成"缺东西"；
3. 前端 `vue-tsc` + `npm run build` 纳入本轮验证。

---

## INC-013 · 「地址」填了平台视角的公网 IP，Exporter 打自己的公网 IP 连不上

**首次暴露**：2026-09-18，`jd-redis` 安装成功、端口可达，但 Exporter 容器日志写：

```
level=error msg="Couldn't connect to redis instance (redis://203.195.191.75:6379)"
redis_up 0
```

使用者自己先指出了关键：**Exporter 跑在目标机上，Redis 也在那台机器上，这里应该填 127.0.0.1**。

**定位过程**

1. 日志里的地址来自集成表单的「地址」字段——平台把用户填的值原样注入 Exporter 环境变量；
2. Exporter 用 `--network host` 跑在**目标机**上，它看到的 `203.195.191.75` 就是那台机器自己的
   公网 IP：从机器内部访问自己的公网 IP 要走 **hairpin NAT**，还要过**安全组**（多数云默认不放通
   实例访问自己的公网地址），因此连不上；而 `127.0.0.1:6379` 一定是通的；
3. 根本问题是「地址」这一个字段承担了两种视角的语义：
   - 平台视角：平台自己能不能连（TCP 健康探测用）；
   - 目标机视角：Exporter 能不能连（抓取指标用）。
   两者在被管实例与 Exporter 同机时**恰好相反**——用户按"平台能连"直觉填了公网 IP，
   Exporter 却需要回环。

**根因**

字段语义没有区分视角，也没有任何提示或兜底：平台知道目标机是谁（`target_host`），
完全有能力在"地址主机 == 目标机"时给出正确写法，却把这个坑留给了使用者。

**修复**

1. `exporterSideAddress()`：渲染 Exporter 配置时，若「地址主机 == 目标机」（同机），
   自动改用 `127.0.0.1`，并在部署说明里写明"已自动改用 + 原因"；
   异机地址原样使用（那才是 Exporter 该连的地址）；
2. `platformSideProbeSkipReason()`：远程集成填了回环地址时，**平台侧 TCP 探测直接跳过**
   （否则会打到平台自己的 localhost，产生假警报），健康以 Exporter 指标为准；
3. `docs/INTEGRATION.md` §2.1 新增「地址的两种视角」说明与对照表，§8 排查表新增该现象。

**防复发**

1. `TestExporterSideAddressRewritesSameMachine`：表格化覆盖"同机改写并给说明""回环不改写"
   "异机不改写""目标机未知不改写"；
2. `TestPlatformSideProbeSkipReason`：覆盖"远程+回环→跳过""远程+真实 IP→照常探测"
   "本机部署→照常探测""非集成实例→照常探测"；
3. `DescribeExporterLog` 的 unknown-network 结论里把"同机却填公网 IP"列为**第一个**常见原因，
   与 §2.1 相互印证。

---

## INC-012 · REDIS_ADDR 带 scheme，Exporter 把 `redis` 当成网络类型

**首次暴露**：2026-09-18，`jd-redis` 安装完成、端口可达、平台也能取到 Exporter 指标，但

```
redis_exporter_last_scrape_error{err="dial redis: unknown network redis"} 1
redis_up 0
```

**定位过程**

1. 先把"能拿到的证据"分层看：容器 `Up`、宿主 9121 在听、平台侧 `curl .../metrics` 有输出 →
   安装与网络全部正常，问题在 **Exporter → Redis** 这一段；
2. `dial redis: unknown network redis` 是 Go `net.Dial(network, addr)` 的报错格式：
   网络类型被当成了 `redis`。看起来像"地址写法问题"；
3. 读 v1.66.0 的 `exporter/redis.go` 才发现这是**兜底路径的马甲**：

   ```go
   c, err := redis.DialURL(uri, options...)
   if err != nil {                                  // ← 真正的失败（连不上/认证失败）被丢弃
       if frags := strings.Split(e.redisAddr, "://"); len(frags) == 2 {
           c, err = redis.Dial(frags[0], frags[1], options...)  // Dial("redis", "host:port")
       }
   }
   ```

   `DialURL` 已经失败过一次（真正原因只在 debug 级别打印），随后这个"按 `://` 拆分重试"
   必然以 `unknown network redis` 再失败一次——用户看到的永远是后一句；
4. 这解释了为什么 `/scrape?target=127.0.0.1:6379` 也报同一句：换地址不改变"先 DialURL 失败"，
   而失败原因（连接被拒 / 口令不对）被同一段代码吞掉。

**根因**

1. redis_exporter 的兜底分支把"scheme 当网络类型"，掩盖了真实原因；
2. 平台注入的是带 scheme 的 `redis://host:port`，正好会走 `len(frags) == 2` 那条分支；
   若用不带 scheme 的 `host:port`，兜底会走 `Dial("tcp", addr)`，**真实错误就能透出来**；
3. 平台侧只呈现 `up=0` 与这句马甲，容易被归因成"Redis 没起来 / 地址填错"。

**修复**

1. `redisAddr()`：默认注入**不带 scheme** 的 `host:port`（官方 README 明确这种写法合法），
   既绕开马甲分支，又让后续任何失败都显示真实原因；
   只有用户显式带路径（如 `redis://host:6379/2` 指定 db）时才保留 URL 形态；
2. `DescribeExporterLog` 新增该签名的翻译，并刻意放在**最后**兜底：
   日志里若已有 `connection refused` / `WRONGPASS` 等具体原因，就报具体原因，
   不能让"马甲"盖住真话；只有真的只剩马甲时，才给出"真实原因被吞 + 三条自查动作"的结论；
3. 模板 Notes 与 `docs/INTEGRATION.md` §8 写明"不要手工改成 `redis://…`"及真实原因的排查路径。

**防复发**

1. `TestRedisAddrHasNoScheme`：断言 `REDIS_ADDR` 等于 `host:port`、不含 `://`，
   并断言带 db 路径时路径不丢；已用"改回 URL()"注入验证会红；
2. `TestRedisExporterArgsDoNotDuplicateAddr`：断言地址**只有一个来源**（env），
   不额外传 `--redis.addr`（官方说明 flag 优先于 env，两处都写会造成"改了一处不生效"）；
3. `TestDescribeExporterLogUnknownNetwork` / `TestDescribeExporterLogPrefersRealReason`：
   断言马甲被翻译成可执行结论，且当日志里存在真实原因时**必须报真实原因**；
4. 原 `TestRenderRedisIntegration` 断言的正是旧写法（`REDIS_ADDR: redis://…`）——
   和 INC-008 那次一样，**测试在保护 bug**，已改为断言"不得带 scheme"。

---

## INC-011 · 「重新应用」收不到凭据、旧失败先于新结果展示

**首次暴露**：2026-09-18，使用者反馈两件事：

```
① 重新编辑并填入私钥后，界面仍先弹出：
   待处理项：重建 Exporter 失败：远程安装需要 SSH 凭据（用户名 + 口令或私钥）
② 「重新应用」不能填凭据；另外「重试」与「重新应用」看起来重复了
```

**定位过程**

1. 「重新应用」的实现（`Apply`）用 `deployParams{operator: operator}` 发起部署——**从不传凭据**。
   远程集成必须经 SSH + Ansible 在目标机安装，于是这个按钮在远程部署上**必然失败**，
   并留下一条"远程安装需要 SSH 凭据"；
2. 账号弹窗里的「重试建号」也有同一个洞：账号 SQL 带了 `in.remoteCreds(...)`，
   但紧随其后的 `deploy(...)` 又退回了不带凭据的调用——使用者刚在弹窗里填过凭据，
   却被告知"缺凭据"，这是最容易被当成"平台有 bug"的一种表现；
3. "填完私钥保存后仍先看到旧错误"与上面两点叠加而成：保存/应用都是**异步**的
   （`runAsync`），接口立刻返回视图，而 `last_error` 只在**成功之后**由 `markApplied` 清除。
   于是保存接口返回的 `last_error` 仍是上一次的旧原因，前端 `handleSubmit` 看到非空就弹警示——
   表现成"修复没生效"；
4. 两个入口的重叠感来自"指路"而不是功能：待处理项横幅一律把人引到账号弹窗，
   而账号弹窗的重试其实也重建 Exporter，于是看起来和「重新应用」重复。

**根因**

1. 同一个"远程执行"能力被三条路径各自拼装（保存、重新应用、账号重试），
   凭据只在其中一条路径上接通，缺少统一入口；
2. 把 `last_error` 当成"当前状态"，而它其实是"上一次尝试的结论"——发起新尝试时没有清理；
3. 界面自己猜"该点哪个按钮"，没有唯一的事实来源。

**修复**

1. 「重新应用」接受可选 SSH 凭据（`SSHCredsInput`，口令/私钥二选一）；
   远程集成缺凭据时**立即返回可操作提示**，而不是排一个必然失败的后台任务；
   前端在远程集成上先弹凭据对话框，并新增**私钥**输入（此前账号弹窗只支持口令）；
2. 账号重试路径的 `deploy` 补上 `in.remoteCreds(...)`；
   同时把"账号通但 Exporter 装不上"也判为未完成（`OK = connected && deployErr == nil`），
   否则界面显示成功、指标却没有；
3. 新增 `beginAttempt`：发起新尝试时写下"⏳ 已开始…"并**清掉上一次的失败**，
   保存/重建两条路径统一使用；
4. 后端给出 `next_action` / `next_action_label`（部署类失败→重新应用；账号类失败→重试建号），
   待处理项横幅据此只渲染一个推荐按钮——指路只留一个事实来源；
5. `SSHCredsInput` 成为唯一的 SSH 凭据入参类型（`AccountSecureInput` 改为其别名），
   同一条 SSH 通道不再有第二份字段定义。

**防复发**

1. `TestClassifyNextAction`：表格化覆盖部署类/账号类/未知原因/空错误，
   并专门锁定**优先级**（"Exporter 未跑通：认证失败"里同时含 Exporter 与认证失败，
   必须判成账号类，否则又会把人引去反复重装）；
2. `TestNextActionOfProvidesLabel`：非空错误必须给出动作与文案，空错误不得给出按钮；
3. `TestSSHCredsInputContract`：用 JSON 往返锁定与前端约定的字段名（改字段名会让前端静默失效：
   后端收到空凭据、远程操作全报缺凭据）与"口令/私钥二选一"语义；
4. 文档：`docs/INTEGRATION.md` 6.2 增加「两个入口的分工」表，
   明确「重试建号」是「重新应用」的超集、两者都必须保留，以及"待处理项只推荐一个动作"。

---

## INC-010 · `docker run` 参数顺序错误：Exporter 的开关被 docker 当成自己的选项

**首次暴露**：2026-09-18，远程安装首次跑到"起容器"这一步（上一轮的失败摘要直接把原文摆了出来）：

```
TASK [重建并启动 Exporter 容器] ***
fatal: [203.195.191.75]: FAILED! => {"rc": 125, "cmd": "docker run -d --name mwops-exporter-jd-redis
  --restart unless-stopped --network host --web.listen-address=:6379 --env-file … oliver006/redis_exporter:v1.66.0",
  "stderr": "unknown flag: --web.listen-address\n\nUsage: docker run [OPTIONS] IMAGE [COMMAND] [ARG...]"}
```

**定位过程**

1. 摘要里 `cmd` 一栏把整条命令摆出来了，一眼可见 `--web.listen-address=:6379` **排在镜像之前**；
2. `docker run` 的解析规则：镜像**之前**的参数是 docker 自己的选项，镜像**之后**的才是容器内进程的参数。
   于是 docker 试图解析 `--web.listen-address` 这个它不认识的选项 → `unknown flag`、`rc=125`，
   容器根本没创建；
3. 模板里 `--web.listen-address` 由 `webListenArg` 单独拼接（"host 网络 + 宿主端口≠默认端口"
   才需要），而模板参数（`in.Args`，如 `--mysqld.address=…`）本来就拼在镜像之后——
   两者走了**两条不同的拼接路径**，只有前者放错了位置，所以此前从未暴露；
4. 顺带发现"为什么会走到这条分支"：集成的 Exporter 端口被填成了 **6379（实例自己的端口）**，
   `webListenArg` 才被触发。host 网络下 Exporter 监听宿主端口，与实例同端口**必然冲突**
   （bind 失败或把实例遮住）——即使参数顺序修好，这个配置也起不来。

**根因**

1. 拼接命令时按"代码书写顺序"而非"docker 的语义位置"组织参数，缺少结构性约束；
2. 端口字段允许填成与实例相同，平台既不校验也不提示——而这是一个必然失败的配置。

**修复**

1. `dockerRunLineWith` 只负责拼 docker 自己的选项（`-d/--name/--restart/--network/-p/-v/--pid=host/--env-file`），
   镜像之后统一追加 `exporterArgs(in)`（`--web.listen-address` + 模板参数），
   两类参数从此只有一个出口；
2. 新增 `exporterPortConflictFix`：Exporter 端口与实例端口相同**且同机**（地址是回环，或地址主机就是目标机）
   时，自动改用模板默认端口并**写回 meta**（保证渲染端口、`/healthz` 抓取目标、Prometheus SD 三处一致），
   在部署说明里明确写出"已自动改用端口 X，原因是……"；实例在别的机器上则不干预（同端口本来无妨）。

**防复发**

1. `TestExporterFlagsComeAfterImage`：端口不一致时断言 `--env-file … <镜像> --web.listen-address=:7777`
   这一顺序，并断言镜像之前不出现该参数；
2. `TestDockerRunFlagsAreWhitelisted`：**结构性**守卫——把 `docker run` 行切成 token，
   镜像之前的每个 token 必须是已知 docker 选项或其取值，否则失败；
   同时覆盖 host/bridge/宿主模式与 systemd 单元里的 `ExecStart=`（tokenizer 会把 `{{ … }}` 当整体）；
   已用"把参数挪回镜像前"注入验证两条守卫都会红；
3. `TestExporterPortConflictFix` / `TestSameMachine`：表格化覆盖同机冲突、异机同端口、未配置、
   模板端口恰好也冲突等分支；
4. 渲染器升到 v5，平台代码修订号升到 r2；自检脚本按"每修订一个独有标记"逐个核对。

---

## INC-009 · 平台把 ansible 输出"截头"呈现，恰好砍掉失败原因

**首次暴露**：2026-09-18，远程安装跑到第 5 个任务时失败，界面上给出的却是：

```
PLAY [安装并启动 Exporter（redis_exporter）] ***
TASK [准备 Exporter 配置目录] ***        changed: [203.195.191.75]
TASK [校验目标机器已安装 docker] ***     ok: [203.195.191.75]
TASK [校验 docker 守护进程可用] ***      ok: [203.195.191.75]
TASK [写入 Exporter 环境变量…] ***       changed: [203.195.191.75]
TASK [拉取官方镜像] **************************…
```

**定位过程**

1. 这份"报错"里全是**成功**的任务，失败任务只露出个标题就被切断了；
2. 回到代码：`deployRemote` 用 `truncateText(safe, 600)` —— 取输出**前 600 字符**；
3. 而 ansible 是从前往后打印的：任务越靠后越晚出现，**失败必然在尾部**。
   于是"截断"这件事本身制造了这次故障的不可诊断性：
   使用者看到的永远是最不重要的开头，想看的原因永远在省略号后面。

**根因**

把"容器日志截断"的直觉套用到了"进度式输出"上。对进度式输出，头部信息量最低；
正确做法是按**语义**挑出失败相关片段，而不是按位置截取。

**修复**

1. 新增 `ansibleFailureExcerpt`：定位第一条失败标记（`fatal:` / `unreachable:` /
   `FAILED!` / `ERROR!`），回溯它所属的 `TASK [` 行，再带上紧随其后的若干行
   （`msg` 常是多行，遇到下一个 `TASK [` / `PLAY RECAP` 停止），并在开头注明省略了多少行；
   没有失败标记时（例如平台侧超时）退化为"头 + 尾"摘要，保证任何情况下都有信息量；
2. 远程安装与目标机账号 SQL 两条路径都改用它；界面上同时保留原有的 `censored` 指引；
3. **完整**（已脱敏）输出写进平台日志（`docker logs mwops-backend`，上限 8000 字符），
   界面给摘要、日志给全文，各司其职；
4. 新增平台代码修订号 `service.CodeRevision`（当前 `r1`），`/healthz` 与启动日志都输出：
   以后遇到"这个修复到底在不在我跑的镜像里"，一条命令即可判断。

**防复发**

1. `remote_excerpt_test.go`：用**真实形态**的 ansible 输出（含 `FAILED - RETRYING` 重试行、
   多行 msg、censored、无标记）逐条断言——摘要必须包含失败任务名与原因、
   **不得**包含更早的成功任务、不得包含 `PLAY RECAP`；
2. 自检脚本同时校验 `code_revision` 与二进制里的 `ansibleFailureExcerpt` 标记；
3. 经验沉淀见"经验提炼"第 10 条。

---

## INC-008 · 漏建配置目录 + `no_log` 吞掉报错，且 env 文件被错误地做了 shell 转义

**首次暴露**：2026-09-18，修完 INC-005/006/007 后，远程安装第一次真正跑起来：

```
TASK [校验目标机器已安装 docker] ***            ok: [203.195.191.75]
TASK [校验 docker 守护进程可用] ***             ok: [203.195.191.75]
TASK [写入 Exporter 环境变量（含口令，权限 0600）] ***
fatal: [203.195.191.75]: FAILED! => {"censored": "the output has been hidden due to
  the fact that 'no_log: true' was specified for this result", "changed": false}
```

**定位过程**

1. 前三个任务全绿，说明渲染、SSH、sshpass、docker 都已就绪，问题只在写 env 文件这一步；
2. 报错被 `no_log` 整段替换成 `censored`——这是**设计使然**（含密任务的 module args 会回显口令），
   但也意味着使用者拿不到任何线索；
3. 对照模板即可确认根因：env 文件路径是 `<安装目录>/<容器名>.env`，而 **docker 模式没有任何任务创建
   `<安装目录>`**——只有 `binary` 模式有"创建安装目录"。`ansible.builtin.copy` 写一个不存在的目录
   必然失败，报错恰好又被 no_log 吞掉；
4. 顺带审计同一处 env 渲染逻辑时发现第二个更隐蔽的缺陷：`renderEnvFile` 用 `shellArg` 给值加
   shell 引号，但这个文件的两个消费者**都不经 shell**——docker 的 `--env-file` 与 systemd 的
   `EnvironmentFile=`。Postgres 的 `DATA_SOURCE_NAME` 含 `?` `:`，会被包成 `'postgresql://…'`，
   Postgres Exporter 拿到带引号的 DSN → invalid DSN、`pg_up=0`；
   本机 Docker 路径用的是 `SanitizeEnvValue`（不转义），两条路径行为不一致，正是"只在远程暴露"的原因。

**根因**

1. 模板把"写文件"和"建目录"拆在两个模式分支里，公共前提只写在了其中一个分支；
2. `no_log` 的使用没有配套的**可读失败点**设计：失败被屏蔽时既没有前置检查，也没有排查指引；
3. env 文件被当成 shell 片段转义，忽略了它真正的消费者是 docker / systemd 的**非 shell** 解析器；
   该缺陷此前一直被"本机部署"路径掩盖（本机走 Docker API，直接传 env 数组，不生成文件）。

**修复**

1. 目录创建提升为**公共前置任务**「准备 Exporter 配置目录」（两种模式各恰好一条），
   它本身不含密，失败时报错可读；
2. `renderEnvFile` 改为 `SanitizeEnvValue`（原样输出、只处理换行），与本机路径一致；
3. 平台在 ansible 输出出现 `censored` 时追加排查指引（该任务被隐藏、请看哪些文件/命令）；
4. 渲染器版本升到 v4；`/healthz`、启动日志、自检脚本三处都能看到版本；
5. 只给**真正含密**的任务加 `no_log`："重建并启动 Exporter 容器""systemd 单元写入"的命令里
   本来就没有口令（口令在 0600 的 env 文件里），原先的 `no_log` 纯属多此一举，
   却把"容器起不来"的原因一起吞了；同时给 `docker pull` 加 3 次重试（国内拉 Hub 易抖动）。

**防复发**

1. `TestRemotePlaybookPreparesConfigDir`：断言三种模式都有且只有一条目录创建任务，
   并且**排在**写 env 之前（顺序错=白建）；
2. `TestEnvFileIsNotShellQuoted`：断言 `DATA_SOURCE_NAME` 原样落盘（含 `?`/`:`/空格/单引号的口令
   都不出现 `'\''` 或包裹引号），换行被处理；
3. `TestRemoteEnvFileEscapesQuotes` 重写为"**原样**写入口令"（原断言锁定的正是错误行为——
   这条测试曾经在保护 bug）；
4. `TestCensoredHintOnlyForCensoredOutput`：断言只在 `censored` 时给指引，可读失败不追加噪音；
5. `TestNoLogOnlyOnSecretTasks`：遍历全组件 × 全安装方式的任务块，断言
   **含 `exporter_env_content` 的任务必须有 no_log、其余任务必须没有**——
   把"不要过度遮蔽"变成可执行的约束，而不是靠人记得；
6. 自检脚本增加"产物第 3 行版本戳必须等于期望版本"，避免"镜像新、产物旧"的混淆。

---

## INC-007 · 平台镜像缺 sshpass，口令方式的远程安装卡在连接阶段

**首次暴露**：2026-09-18，修完 INC-005/006 并重建镜像后，同一操作第三次报错（这次已经能连到目标机）：

```
fatal: [203.195.191.75]: FAILED! => {"msg": "to use the 'ssh' connection type with
  passwords or pkcs11_provider, you must install the sshpass program"}
```

**定位过程**

1. 报错阶段变了：playbook 渲染通过、`TASK` 正常展开、开始建连——说明前两轮的修复已生效，
   问题换到了下一层；
2. 主语是谁？这句话里没有"目标机"：`ansible` 的 ssh 连接插件（`ansible.builtin.ssh`）
   本身不实现认证，它调用 **OpenSSH**，而 OpenSSH 不接受命令行口令，
   必须由 `sshpass` 代答——**缺的是平台容器里的程序**；
3. 平台镜像 `WITH_ANSIBLE=true` 只装了 `ansible`，Alpine 的 `ansible` 包并不拉 `sshpass`
   （也没有 `openssh-client` 依赖保证），于是"默认开箱可用"的承诺在口令认证下不成立；
4. sshpass 在 Alpine 3.20 的 **main** 仓库（不是 community），镜像里 main+community 都已配置，
   因此 `apk add sshpass` 可直接装上。

**根因**

Dockerfile 把"装 ansible"等同于"能远程安装"，漏掉了 ssh 认证链路上的外部依赖；
而平台在凭据层允许"用户名 + 口令"这种默认用法，两者不匹配。

**修复**

1. Dockerfile：`apk add --no-cache ansible openssh-client sshpass`，
   并在构建期校验 `ansible-playbook` / `ssh` / `sshpass` 都存在（装不上就让构建失败）；
2. `remote.go` 增加前置检查 `checkSSHPass`：口令认证且平台无 sshpass 时，**执行前**就返回
   「平台容器内缺少 sshpass…① 重建镜像 ② 改用私钥认证」，不再把 ansible 原话抛给使用者；
3. inventory 现在同时写 `ansible_become_password`（与 SSH 口令同源）：
   登录普通用户 + sudo 需要密码时，缺它会报 `Missing sudo password`，属于同一条链路上的坑；
4. `/healthz` 暴露 `sshpass: true|false`，排障脚本与使用者都能一眼判断镜像能力
   （它与"镜像新旧"无关：同一版渲染器可能来自装了 sshpass 的新镜像）。

**防复发**

1. `internal/service/remote_sshpass_test.go`：用替身 `lookPath` 覆盖三种情形——
   口令认证缺 sshpass 必须拦下且提示包含两条出路、私钥认证不得因此被拦（且不该去查 sshpass）、
   有 sshpass 时放行；断言错误信息不回显口令；
2. `TestRemotePlaybookNeverContainsSecrets` 增加 `ansible_become_password` 断言，
   并继续保证展示版 inventory 里没有明文；
3. `deploy/ansible/tools/check-backend-freshness.sh` 增加 `ssh` / `sshpass` 与 `/healthz` 能力位检查；
4. 文档：README §3 明确"平台侧靠 sshpass 支撑口令认证"，§6.3 给出该报错的两种解法
   （**改用私钥可立即绕过，无需重建**）。

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
7. **"装了工具"不等于"链路可用"**：外部能力（ansible、docker、systemd）都有一条依赖链，
   链上任意一环缺失都会在**最远端**以一句与使用者无关的报错暴露（INC-007：装了 ansible
   但没装 sshpass，口令认证直接不可用）。做法有两条：构建期把链上关键程序都校验一遍
   （`command -v ssh sshpass`），运行期在**动手之前**做前置检查并把两条出路写进错误信息。
8. **日志遮蔽要配套"可读失败点"**：`no_log` 保住了口令，也吞掉了根因（INC-008 只回了
   一行 `censored`）。含密任务周围必须铺开不含密的前置检查（目录、权限、外部命令），
   失败时至少能二分定位；平台侧再补一句"下一步看什么"。
9. **转义要跟着消费者走，不是跟着"看起来危险"走**：同一个 `KEY=VALUE` 文件，
   shell 需要引号转义，docker `--env-file` 与 systemd `EnvironmentFile=` 却把引号当值的一部分
   （INC-008 的 PG DSN）。判断依据只能是"谁解析它"，并且**同一份数据的不同路径必须行为一致**
   ——本机路径不转义、远程路径转义，这种不一致会让缺陷只在某一条路径上暴露。
10. **呈现诊断信息要按语义挑，不能按位置截**：进度式输出（ansible、构建、迁移日志）的
    信息量集中在**尾部**，`head -c 600` 恰好把原因砍掉（INC-009）。要么摘出失败片段
    （失败标记 + 所属任务 + 多行 msg），要么头尾都给；界面给摘要、日志留全文。
    另外每解决一个"跑的是不是新代码"的问题，都该问一次：**下次怎么在 1 条命令内确认**——
    `/healthz` 的 `playbook_renderer` / `code_revision` 就是这么来的。
11. **拼命令行要按"消费者怎么解析"分区，而不是按代码顺序**：`docker run` 以镜像为界，
    前后参数属于两个不同的解析器（docker 自己 vs 容器内进程），放错位置就是 `unknown flag`
    （INC-010）。凡是拼接多段参数的命令（docker、ssh、systemd `ExecStart`），
    都该把"每一段归谁"写成函数并用**结构性守卫**（token 白名单）锁住，而不是逐条断言字符串。
12. **必然失败的配置要在执行前变成自愈或明确拒绝**：Exporter 端口填成实例端口，在 host 网络下
    100% 起不来（INC-010）。平台既然知道实例端口与拓扑，就不该把这个错误留给目标机去报——
    能安全自愈的自愈（并把原因写进部署说明），不能自愈的就在提交前拒绝。
13. **同一条能力只留一个入口，且每个入口都要能就地补齐前提**：远程执行被"保存/重新应用/账号重试"
    三条路径各拼一遍，凭据只接通了一条（INC-011），于是按钮在远程集成上必然失败、
    使用者被两个弹窗来回推。做法：把凭据收成一个类型、把"发起尝试"收成一个函数
    （清旧错、写进度），并让**后端**决定"下一步点哪个按钮"（`next_action`），
    界面只负责渲染——指路只能有一个事实来源。
14. **异步流程里 `last_error` 是"上一次的结论"，不是"当前状态"**：发起新尝试时必须立刻清掉，
    否则接口返回的旧错误会被界面当成"刚刚这次的结果"（INC-011 的"填完凭据仍弹旧错"）。
    进度用"⏳ 已开始…"表达，结论等后台任务结束时再写。
15. **"两边都符合各自文档"也会不工作**：平台按 URL 规范写 `redis://…`，Exporter 按自己的
    实现把 scheme 当网络类型（INC-012）——单看任何一侧都挑不出错。跨组件集成时，
    **能直接验证的那一侧就用最小实验验证**，比读两边文档猜快得多；确认后再把结论固化进模板与测试。
16. **报错信息可能是"马甲"，要读被集成的那个程序的源码**：`unknown network redis` 看起来是地址
    写法问题，实际是 redis_exporter 兜底分支的产物，真正的失败（连接被拒/口令不对）被它吞了
    （INC-012）。遇到"这句报错解释不通"时，去读**那个程序**对应版本的源码，往往几十行就能定性；
    同时平台侧要把这种马甲翻译成"真实原因可能是什么 + 三条自查动作"。
17. **兜底分支/降级路径必须把原始错误带上**：如果 redis_exporter 把第一次的 err 一起返回，
    这个故障根本不需要排查（INC-012）。平台自己写降级时同理——宁可串上 `%w`，也不要覆盖。
16. **测试可能正在保护 bug**：`TestRenderRedisIntegration` 断言的正是错误的地址写法，
    `TestRemoteEnvFileEscapesQuotes` 断言的正是错误的 shell 转义（INC-008）。
    改行为时要**先看有没有旧测试在锁定旧行为**，并在提交信息/注释里写明为什么改。
