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

## 1. 让平台镜像带上 Ansible

默认镜像**不含** Ansible（避免给所有使用者增加约 200MB）。需要远程安装时用这个开关构建：

```bash
# docker-compose.yml 的 backend.build.args 里加（或 .env 里设 WITH_ANSIBLE=true）
WITH_ANSIBLE=true docker compose up -d --build backend
```

对应的 Dockerfile 片段（`middleware-ops/Dockerfile`）：

```dockerfile
ARG WITH_ANSIBLE=false
RUN if [ "$WITH_ANSIBLE" = "true" ]; then \
      apk add --no-cache --repository=https://dl-cdn.alpinelinux.org/alpine/v3.20/community ansible; \
    fi
```

验证：

```bash
docker exec mwops-backend ansible-playbook --version
```

## 2. 打开平台开关

`nightjar/.env`：

```bash
INTEGRATION_ALLOW_REMOTE_INSTALL=true
INTEGRATION_ANSIBLE_ENABLED=true
# 可选：安装方式（docker|systemd）、网络、超时、安装目录
INTEGRATION_ANSIBLE_INSTALL_MODE=docker
INTEGRATION_ANSIBLE_DOCKER_NETWORK=host
INTEGRATION_ANSIBLE_TIMEOUT=15m
INTEGRATION_ANSIBLE_INSTALL_DIR=/opt/mwops-exporter
```

## 3. 目标机要求

- 可通过 SSH 登录（口令或私钥），有 sudo 权限（`--become`，默认开）；
- 已安装 Docker（`docker version` 可用）——远程安装默认用**官方 Exporter 镜像**跑容器，不需要在目标机上装 Python 之外的任何东西；
- 平台能访问目标机的 Exporter 端口（安装后平台会主动探一次，探不通会在集成备注里写明）。

## 4. 安全约定（重要）

- 平台**只渲染内置模板**的 playbook，使用者不能上传任意 playbook —— 避免把平台变成远程命令执行入口；
- SSH 口令/私钥只在本次请求内存中使用，写进 **0600** 的临时 inventory / vars 文件，执行完立即删除；
  **不落库、不写审计、不回显**（Ansible 侧用 `no_log`，平台回传日志前再擦除一次口令）；
  因此「重新应用」无法复用上次的凭据，需要重新填写；
- playbook 落盘到 `integration.output_dir/ansible/<集成名>.yml`（0644）供审计与人工复核，**其中不含任何凭据**
  （Exporter 口令通过 `-e @vars.yml` 传入）；
- 生产环境（`env=prod`）只创建审批工单（`integration_remote_install`），审批通过后才会执行。

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
