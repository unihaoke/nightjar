# 远程安装 Exporter（Ansible）

平台支持两种 Exporter 部署位置，在「集成中心」的表单里选择：

| 部署位置 | 平台做什么 | 抓取目标 | 依赖 |
|---|---|---|---|
| **本机**（默认） | 用 Docker Engine API 在平台所在宿主机创建 Exporter 容器 | 普通中间件：`mwops-exporter-<集成名>:<端口>`；主机监控（node）：`host.docker.internal:9100` | `docker.sock` |
| **远程服务器** | 渲染内置 Ansible playbook，SSH 到目标机安装并启动 Exporter | `<目标IP>:<端口>` | 平台镜像内有 `ansible-playbook` + 目标机可 SSH |

### 远程安装的三种方式（`INTEGRATION_ANSIBLE_INSTALL_MODE`）

| 方式 | 做法 | 适用 |
|---|---|---|
| `docker`（默认） | 官方镜像跑容器（`--network host --env-file ...`） | 目标机有 Docker（最常见） |
| `docker-systemd` | 同上，但容器交给 systemd 托管（开机自启、统一运维） | 目标机用 systemd 管理服务 |
| `binary` | 下载官方 release 的 tar.gz → 解压到 `/opt/...` → 写**原生** systemd 单元 → 开机自启 | 目标机**没有 Docker**、或不允许跑容器 |

`binary` 模式的实现要点：

- 由 `uname -m` 自动识别架构（`x86_64→amd64`、`aarch64→arm64`）选包；
- 下载地址来自组件模板的 `Release{Repo,Version,Binary}`（可用 `URLTemplate` 覆盖，
  例如 nginx-prometheus-exporter 的资产名用下划线）；
- 口令依旧走 **0600 的 env 文件**，由 systemd 的 `EnvironmentFile=` 注入；
- `--path.rootfs` 会自动从容器模式的 `/host` 改写为 `/`（原生运行时的根就是宿主根）；
- 新增组件只需在模板里补一段 `Release` 元数据，不需要改 playbook。

### 主机监控（node_exporter）

集成中心的「主机 / Node」组件用于监控**服务器本身**（CPU/内存/磁盘/负载/网络）：

- 「地址」填**被监控的那台机器**：本机场景填 `127.0.0.1`（平台会把抓取目标改写成
  `host.docker.internal:9100`，compose 里已配 `host-gateway`），跨机填目标机 IP；
- 本机 Docker 模式会自动以 `--net=host --pid=host` 运行并只读挂载宿主 `/` 到 `/host`
  （`--path.rootfs=/host`）；远程 docker 模式同样带这些参数；
- 跨机建议直接用「远程服务器 + binary」组合：目标机无需 Docker；
- 防火墙放通 9100；各节点需 NTP/chrony 时间同步，偏差过大样本会被丢弃。

## 1. 平台镜像已默认带 Ansible

远程部署是集成中心的**默认路径**，所以镜像默认就把 `ansible-playbook` 装进去
（`WITH_ANSIBLE` 构建参数默认 `true`，镜像约 +200MB）：

```bash
# 正常构建即可，什么都不用加
docker compose up -d --build backend
docker exec mwops-backend ansible-playbook --version   # 应输出版本号
```

只做「本机 Docker」部署、想给镜像瘦身时：

```bash
WITH_ANSIBLE=false docker compose up -d --build backend
```

对应的 Dockerfile 片段（`middleware-ops/Dockerfile`，构建期会做一次 `--version` 校验，
装不上就直接让构建失败，而不是等到运行时才发现）：

```dockerfile
ARG WITH_ANSIBLE=true
RUN if [ "$WITH_ANSIBLE" = "true" ]; then \
      apk add --no-cache --repository=https://dl-cdn.alpinelinux.org/alpine/v3.20/community ansible && \
      ansible-playbook --version >/dev/null; \
    fi
```

> 只用到 `ansible.builtin.*` 模块，因此不需要 `community.*` 集合。
>
> **构建慢怎么办**：
> 1. `ansible` 在 Alpine 的 `community` 仓库里（默认源不含它，Dockerfile 已显式写入 main+community）；
> 2. 国内构建建议换源：`.env` 里设 `ALPINE_MIRROR=mirrors.aliyun.com`（或 `mirrors.tuna.tsinghua.edu.cn`）；
> 3. 改业务代码不会重复下载 ansible（它是独立的靠前层）；Go 编译缓存也通过 BuildKit
>    `--mount=type=cache` 保留，因此**第二次起构建只编改过的包**；
> 4. 想看清每一步耗时：`docker compose build --progress=plain backend`。
> 5. 只做本机 Docker 部署时用 `WITH_ANSIBLE=false`，构建最快、镜像最小。

## 2. 平台开关（默认已开）

`.env` 里两项**默认就是 true**，通常不用改：

```bash
INTEGRATION_ALLOW_REMOTE_INSTALL=true
INTEGRATION_ANSIBLE_ENABLED=true
```

安全说明：远程安装会在**别的机器**上执行命令。默认开启是因为它是主功能；
合规要求更严时可设为 `false` 并用 `WITH_ANSIBLE=false` 构建
（生产环境 `env=prod` 的集成本就会先创建审批工单，审批通过后才执行）。

## 2.1 其它可调项

```bash
INTEGRATION_ANSIBLE_INSTALL_MODE=docker      # docker | docker-systemd | binary
INTEGRATION_ANSIBLE_DOCKER_NETWORK=host
INTEGRATION_ANSIBLE_INSTALL_DIR=/opt/mwops-exporter
INTEGRATION_ANSIBLE_TIMEOUT=15m
INTEGRATION_ANSIBLE_BECOME=true
```

## 3. 目标机要求

- 可通过 SSH 登录（口令或私钥），有 sudo 权限（`--become`，默认开）；
- **平台侧**（不是目标机）用口令认证时依赖 `sshpass`：镜像默认已装（`WITH_ANSIBLE=true`），
  自建镜像请确保包含它，否则报 §6.3 那句 `you must install the sshpass program`；
- 目标机需有 **Python 3**：除 `command`/`shell`/`raw` 外，`copy`/`file`/`systemd`/`wait_for`
  等模块都在目标机上以 Python 执行（`ansible_python_interpreter=auto_silent` 只是"找不到时不警告"，
  并不会免掉这个依赖）。CentOS 7 自带的 python2 不满足新版 ansible-core，需装 `python3`；
- `docker` / `docker-systemd` 安装方式：目标机需已安装 Docker（`docker version` 可用）；
  **`binary` 方式不需要目标机有 Docker**（下载官方 release 二进制 + 原生 systemd 服务）；
- 建号/改号时：目标机需有 `mysql` / `psql` 客户端，或退回到它自己的 docker；
- 平台能访问目标机的 Exporter 端口（安装后平台会主动探一次，探不通会在集成备注里写明）：
  云主机记得在**安全组/防火墙**里对"平台所在机器的出口 IP"放通该端口（如 9121/9104/9100）；
- 目标机要能拉取镜像（`docker` 方式）：国内直连 Docker Hub 常超时，建议给目标机的 dockerd
  配镜像加速（`/etc/docker/daemon.json` 的 `registry-mirrors`）；平台侧已对 `docker pull`
  做了 3 次重试，失败时报错可读（该任务不带 `no_log`）；

## 4. 安全约定（重要）

- 平台**只渲染内置模板**的 playbook，使用者不能上传任意 playbook —— 避免把平台变成远程命令执行入口；
- SSH 口令/私钥只在本次请求内存中使用，写进 **0600** 的临时 inventory / vars 文件，执行完立即删除；
  **不落库、不写审计、不回显**（Ansible 侧用 `no_log`，平台回传日志前再擦除一次口令）；
  因此「重新应用」无法复用上次的凭据，需要重新填写；
- playbook 落盘到 `integration.output_dir/ansible/<集成名>.yml`（0644）供审计与人工复核，**其中不含任何凭据**
  （Exporter 口令通过 `-e @vars.yml` 传入）；
- 生产环境（`env=prod`）只创建审批工单（`integration_remote_install`），审批通过后才会执行。

## 4.1 只读监控账号也能在目标机上代建（远程不依赖平台 Docker）

「由平台创建只读监控账号」在**本机**模式走平台侧的 docker 一次性容器；
在**远程**模式下改为在**目标主机**上执行同一套内置 SQL，覆盖四类操作：

| 操作 | 远程模式下的执行方式 | 需要什么凭据 |
|---|---|---|
| 建号（保存集成时） | 目标机执行建号 SQL | 管理员凭据 + SSH 凭据 |
| 重试建号 / 连接测试 | 同上（连接测试只跑 `SELECT 1`） | 同上（连接测试只需 SSH + 已存口令） |
| 轮换口令 | 目标机用**账号自己的旧口令**执行 `ALTER` | 只需 SSH 凭据（不需要管理员凭据） |
| 删除账号 | 目标机执行 `DROP` | 管理员凭据 + SSH 凭据（prod 转审批工单） |

执行次序：
1. 优先用目标机自带的 `mysql` / `psql` 客户端（口令走 `MYSQL_PWD` / `PGPASSWORD` 环境变量，不进命令行）；
2. 目标机没有客户端时，回退到**目标机自己的 docker** 起一次性客户端容器（`--network host`）；
3. 两者都没有时**明确失败**并给出两个选项（装客户端 / 手工建号），平台不会擅自改目标机的软件包。

因此：**远程集成只需要 SSH，平台可以完全没有 `docker.sock`**（也就不需要 `DOCKER_GID`）。
SSH 凭据与安装/建号时一样**不落库**，所以每次操作都需要在页面上重新填写一次
（集成中心的「监控账号」弹窗顶部有专门的 SSH 凭据输入区）。

**已知边界**：若数据库只存在于容器内、宿主又没有发布端口（例如 compose 起的 MySQL 未映射 3306），
目标机的本地客户端同样连不上——这种场景请改用「本机」部署位置（平台把一次性客户端接进同一张 docker 网络），
或手工建号后只把账号口令填进集成表单。表单里的「地址」在远程模式下应当是**目标机能解析**的地址
（如 `127.0.0.1:3306` 或内网 IP），不要填只在平台侧有效的 docker 容器别名。



## 5. 卸载

```bash
# docker 模式
docker rm -f mwops-exporter-<集成名>
sudo rm -f /opt/mwops-exporter/mwops-exporter-<集成名>.env

# systemd 模式
sudo systemctl disable --now mwops-exporter-<集成名>.service
sudo rm -f /etc/systemd/system/mwops-exporter-<集成名>.service
sudo systemctl daemon-reload
```

在平台上删除该集成不会自动卸载目标机上的 Exporter（平台不假设自己拥有那台机器），
如需彻底清理请按上面命令手工执行。

## 6. 排障

### 6.1 `found unacceptable key (unhashable type: 'AnsibleMapping')`

完整报错形如（`exit status 4`）：

```
ERROR! We were unable to read either as JSON nor YAML ...
Syntax Error while loading YAML.
  found unacceptable key (unhashable type: 'AnsibleMapping')
The error appears to be in '/app/data/integrations/ansible/<集成名>.yml': line 33 ...
        port: {{ exporter_port }}
                    ^ here
```

先确认**是哪一种**（这一步能省掉大量瞎猜）：

```bash
# ① 跑的是哪一版渲染器？字段缺失或低于代码里的 PlaybookRendererVersion（当前 v4）→ 后端镜像是旧的
curl -s http://127.0.0.1:8080/healthz
# ② 落盘的 playbook 第 3 行应带同一个版本戳
docker exec mwops-backend sed -n '1,6p' /app/data/integrations/ansible/<集成名>.yml
```

- **版本戳是旧版 / `playbook_renderer` 字段不存在** → 后端镜像没重建。
  `docker compose up -d` **不会**重建镜像（本地已存在同名镜像时直接复用），必须显式构建：

  ```bash
  cd nightjar
  docker compose build backend && docker compose up -d backend
  curl -s http://127.0.0.1:8080/healthz   # 应看到 "playbook_renderer": "mwops-playbook v4"
  ```

  然后在集成详情页点「重新应用」，重新生成并执行 playbook。

- **版本戳已是最新却仍报错** → 平台模板缺陷：把第 2 步打印出的文件与
  `middleware-ops/internal/integration/ansible.go` 对照提工单。

> v2 渲染器起，平台在把 playbook 交给 `ansible-playbook` **之前**会自己用 YAML 解析器
> 校验一遍（`internal/integration/playbook_validate.go`）：真出错时界面直接提示
> 「第 N 行不是合法 YAML」，不会再抛 ansible 那句 unhashable type。
> 因此**只要还看到这个原始报错，就说明跑的不是 v2 及以后的渲染器，即镜像未重建**。

### 6.2 `template error while templating string: unexpected '.'`

完整报错形如（`exit status 2`）：

```
TASK [校验目标机器上的 Docker 可用] ***
fatal: [203.195.191.75]: FAILED! => {"msg": "template error while templating string:
  unexpected '.'. String: docker version --format '{{.Server.Version}}'. unexpected '.'"}
```

这条**不是**目标机的问题：playbook 里出现的 `{{.Server.Version}}` 是 **docker 的 Go 模板**
语法，而 playbook 里所有 `{{ }}` 都会**先被 Ansible 当 Jinja 表达式渲染**，
行首的点号在 Jinja 里非法，于是第一个任务（校验 Docker）就直接失败，
后面的建号与安装一步都没跑。

- 现象：报错里出现 `template error while templating string`、`unexpected '.'`；
- 处理：属平台模板缺陷（INC-006）。v3 渲染器起已改为 `command -v docker` + 裸 `docker version`
  （不再用 `--format`），并在渲染后自校验——真出错时界面提示「第 N 行含 Go 模板语法」。
  与 6.1 一样，先 `curl /healthz` 确认 `playbook_renderer` 是否已是最新（`mwops-playbook v4`）。

在目标机上复现同类问题的通用判据：playbook 里**任何一个** `{{ … }}` 都会被 Ansible 渲染，
所以只能写 Jinja 表达式；要保留字面量 `{{ }}`（如 docker/Go 模板串）必须用
`{% raw %}…{% endraw %}` 包裹。

同一类"跨层语义"的坑还有两个，遇到时先怀疑它们：

| 现象 | 原因 | 正确写法 |
|------|------|----------|
| `No such file or directory: b'command'` | `ansible.builtin.command` **不经 shell**，`command -v` 是 shell 内建 | 探测用 `ansible.builtin.shell: command -v xxx` |
| `Executable path is not absolute` | systemd 单元的 `ExecStart` 不接受裸 `docker` | 用 `command -v docker` 解析出的绝对路径 |
| `内容本该两行却挤成一行` | YAML 把多行**引号**标量折叠成空格 | 多行内容用块标量 `content: \|` |

### 6.3 `you must install the sshpass program`

完整报错形如：

```
fatal: [203.195.191.75]: FAILED! => {"msg": "to use the 'ssh' connection type with
  passwords or pkcs11_provider, you must install the sshpass program"}
```

这条报错说的是**平台容器**，不是目标机：ansible 的 ssh 连接插件不自己实现认证，
它把口令交给 OpenSSH，而 OpenSSH **不接受命令行口令**，必须由 `sshpass` 代答。
镜像里缺 `sshpass` 时，连接阶段直接失败——注意此时 playbook 已经渲染成功、
SSH 也已经连过（失败发生在认证），跟"模板对不对"无关。

```bash
# 平台侧自查（两条都应输出路径）
docker exec mwops-backend sh -c 'command -v ssh; command -v sshpass'
curl -s http://127.0.0.1:8080/healthz   # 应包含 "sshpass":true
```

两条出路：

| 方案 | 适用 | 做法 |
|------|------|------|
| 改用「SSH 私钥」认证 | 想立刻跑通，**不需要重建镜像** | 集成表单的凭据方式选「私钥」，粘贴目标机的私钥 |
| 重建镜像 | 想继续用口令 | `docker compose build backend && docker compose up -d backend`（`WITH_ANSIBLE=true` 会一并安装 `sshpass` 与 `openssh-client`，构建期校验存在） |

> v3 起平台会在**执行前**自己检查：口令认证 + 平台无 `sshpass` → 直接返回
> 「平台容器内缺少 sshpass…① 重建镜像 ② 改用私钥」，不再把 ansible 的原话丢给使用者（INC-007）。
> 另外 inventory 现在会同时写 `ansible_become_password`：登录普通用户且 sudo 需要密码时，
> 缺这一项会报 `Missing sudo password`。

### 6.4 报错里只有 `censored`，看不到失败原因

```
TASK [写入 Exporter 环境变量（含口令，权限 0600）] ***
fatal: [203.195.191.75]: FAILED! => {"censored": "the output has been hidden due to
  the fact that 'no_log: true' was specified for this result", "changed": false}
```

含密任务必须 `no_log: true`（否则口令会随 module args 回显），代价就是失败结果被**整段**替换成
`censored`——真实原因（目录不存在、权限不足、磁盘满）全被吞掉。平台的对策是把可能失败的前置步骤
拆成**不含密**的独立任务，让它们自己报错：

| 步骤 | 任务名 | 失败时是否可读 |
|------|--------|----------------|
| 建配置目录 | 准备 Exporter 配置目录 | ✅ 可读 |
| docker 是否可用 | 校验目标机器已安装 docker / 校验 docker 守护进程可用 | ✅ 可读 |
| 写 env 文件（含口令） | 写入 Exporter 环境变量 | ❌ no_log |
| 拉镜像 / 起容器 | 拉取官方镜像 / 重建并启动 Exporter 容器 | 拉取可读，起容器 no_log |

真遇到 `censored` 时（平台会同时附上这段指引），到目标机上手工确认：

```bash
ls -ld /opt/mwops-exporter        # 目录是否存在且可写
df -h /opt                        # 磁盘是否满
docker ps -a | grep mwops-exporter   # 容器是否创建成功
docker logs <容器名>                 # Exporter 自身日志
```

> 历史故障（INC-008）：docker 模式漏了"建目录"这一步，env 文件写不进去，
> 而报错恰好被 `no_log` 吞掉，使用者只看到一行 `censored`。v4 起该目录由**公共前置任务**创建，
> 两种安装模式都覆盖。

### 6.5 重建后仍未生效的常见原因

先跑一次只读自检（会逐项告诉你差在哪）：

```bash
bash deploy/ansible/tools/check-backend-freshness.sh [容器名] [集成名]
```

| 现象 | 原因 | 处理 |
|------|------|------|
| `docker compose up -d` 后行为没变 | 该命令不重建镜像 | 用 `docker compose build backend` 或 `up -d --build backend` |
| 构建很快但代码没变 | 构建上下文不是当前工作区（换了目录/机器） | `docker compose build --progress=plain backend` 看 `COPY` 的源；确认 `docker compose config \| grep context` |
| 改了 `.env` 没生效 | 环境变量在容器创建时注入 | `docker compose up -d --force-recreate backend` |
| 不确定镜像里有没有某修复 | 无 | `curl -s /healthz` 看 `code_revision`（当前 `r1`）与 `playbook_renderer`（当前 v4） |

### 6.6 界面上的报错只有任务名，看不到原因

v4 起界面上给的是**失败摘要**（失败任务名 + `fatal`/`msg` 原文，并注明省略了多少行），
完整（已脱敏）输出在平台日志里：

```bash
docker logs mwops-backend 2>&1 | grep -A40 '集成：远程安装失败' | tail -60
```

若摘要里出现 `censored`，说明失败任务带 `no_log`（含口令，必须遮蔽），
按 §6.4 到目标机上手工确认。

### 6.7 `unknown flag: --web.listen-address`（或其它 Exporter 开关）

```
fatal: [203.195.191.75]: FAILED! => {"rc": 125,
  "stderr": "unknown flag: --web.listen-address\n\nUsage: docker run [OPTIONS] IMAGE [COMMAND] [ARG...]"}
```

`docker run` 以**镜像为界**：镜像之前是 docker 自己的选项，镜像之后才是容器内进程的参数。
Exporter 的开关（`--web.listen-address`、`--mysqld.address` 等）一旦排在镜像之前，
docker 就会报 `unknown flag`，`rc=125`，容器根本没创建。

- v5 渲染器起已把 Exporter 参数统一排到镜像之后，**这个报错只可能出现在 v5 之前的产物上**：
  `curl -s /healthz` 看 `playbook_renderer`，重建后端并「重新应用」即可；
- 同时注意触发它的原因：只有"宿主端口 ≠ 组件默认端口"才会加 `--web.listen-address`。
  如果是因为把 **Exporter 端口填成了实例端口**（如 Redis 6379），host 网络下两者会抢同一个端口——
  v5 起平台会**自动改用模板默认端口**并把原因写进部署说明；
  建议仍按模板默认端口（Redis 9121 / MySQL 9104 / PG 9187 / node 9100）配置。

### 6.8 怀疑产物本身有问题时

平台内部的渲染后自校验用的是 Go 的 YAML 解析器，而 ansible 用的是 PyYAML（同族、不同实现），
所以怀疑产物时可以**用下游的解析器复核**。仓库里带了工具，不需要连任何主机：

```bash
cd middleware-ops
go run ./cmd/renderdump ../render-artifacts      # 渲染全部组件 × 全部安装方式的产物
cd ..
python3 deploy/ansible/tools/check_artifacts.py ./render-artifacts   # 用 PyYAML 逐个复核
```

产物里的口令都是 `${MONITOR_PASSWORD}` 占位（渲染器只输出脱敏版本），可安全留存与比对。
在真机上进一步验证：

```bash
ansible-playbook --syntax-check -i <inventory> <playbook>
```

> 这套流程是 INC-006 之后固化下来的：渲染器单测只能证明「我们渲染得对不对」，
> 证明不了「下游工具怎么读」——playbook 要同时穿过 YAML → Jinja → docker/Go 模板 → shell
> 四层，本轮 4 个缺陷全是在下游那一侧暴露的。

