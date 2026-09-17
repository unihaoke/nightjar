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
- `docker` / `docker-systemd` 安装方式：目标机需已安装 Docker（`docker version` 可用）；
  **`binary` 方式不需要目标机有 Docker**（下载官方 release 二进制 + 原生 systemd 服务）；
- 建号/改号时：目标机需有 `mysql` / `psql` 客户端，或退回到它自己的 docker；
- 平台能访问目标机的 Exporter 端口（安装后平台会主动探一次，探不通会在集成备注里写明）。

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
