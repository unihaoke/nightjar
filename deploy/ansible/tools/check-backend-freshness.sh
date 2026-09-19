#!/usr/bin/env bash
# 检查「平台跑的是不是最新代码」——远程安装报 YAML/Jinja 语法错时先跑这个。
#
# 背景（INC-005 / INC-006）：连续两次故障的真实原因都是后端镜像没重建，
# 而 `docker compose up -d` **不会**重建已存在的镜像，报错却看起来像模板写错了。
# 本脚本只读，不改任何东西：分别从**运行中的二进制**、**产物文件**、**HTTP 接口**
# 三个角度确认渲染器版本，并给出下一步命令。
#
# 用法：
#   bash deploy/ansible/tools/check-backend-freshness.sh [容器名] [集成名]
# 默认：容器 mwops-backend，集成 jd-redis。

set -uo pipefail

CONTAINER="${1:-mwops-backend}"
INTEGRATION="${2:-jd-redis}"
BIN="/usr/local/bin/middleware-ops"
ARTIFACT="/app/data/integrations/ansible/${INTEGRATION}.yml"
# 与 Go 侧 integration.PlaybookRendererVersion 对应；版本升级时同步改这里。
EXPECTED_RENDERER="mwops-playbook v5"
# 与 Go 侧 service.CodeRevision 对应：平台自身行为（错误呈现/诊断）的修订号。
EXPECTED_REVISION="r13"
# 该变量名是 v3 才引入的：二进制里搜到它，说明镜像至少是 v3。
RENDERER_MARKER="exporter_docker_bin"
# 各修订号的"独有标记"：用来判断镜像里到底有没有这一版修复（只增不改写）。
REVISION_MARKERS="r1:ansibleFailureExcerpt r2:exporterPortConflictFix r3:classifyNextAction r4:redisAddr r5:exporterSideAddress r6:ReverifyIntegrations r7:componentUpExpr r8:不使用任何模拟数据 r9:客户端连接使用率 r10:platform_settings r11:无法解析用量统计的数据表名 r12:未启用日志总线 r13:冷却期内抑制"

pass=0
fail=0
ok() { printf '  \033[32m[通过]\033[0m %s\n' "$1"; pass=$((pass + 1)); }
no() { printf '  \033[31m[失败]\033[0m %s\n' "$1"; fail=$((fail + 1)); }
info() { printf '  [信息] %s\n' "$1"; }

echo "== 1. 容器是否存在并运行 =="
if ! docker inspect "$CONTAINER" >/dev/null 2>&1; then
  no "找不到容器 $CONTAINER（用 docker ps 确认实际容器名/项目名）"
  echo
  echo "结论：容器名不对，后面的检查无法进行。"
  exit 1
fi
state=$(docker inspect -f '{{.State.Status}}' "$CONTAINER")
started=$(docker inspect -f '{{.State.StartedAt}}' "$CONTAINER")
image_ref=$(docker inspect -f '{{.Image}}' "$CONTAINER")
# 必须用镜像 ID 查镜像时间：容器名对 `docker image inspect` 是无效引用。
image_created=$(docker image inspect -f '{{.Created}}' "$image_ref" 2>/dev/null || echo "未知")
ok "容器在运行（status=$state，started=$started）"
info "容器所用镜像创建时间：$image_created"

echo
echo "== 2. 运行中的二进制是哪一版渲染器 =="
# 用 -c 只取计数：busybox grep 不做二进制识别，GNU grep -c 对二进制也返回计数。
marker=$(docker exec "$CONTAINER" grep -c "$RENDERER_MARKER" "$BIN" 2>/dev/null | tr -d '[:space:]')
if [ "${marker:-0}" -gt 0 ] 2>/dev/null; then
  ok "二进制含 v3+ 标记（$RENDERER_MARKER × $marker）"
else
  no "二进制里没有 $RENDERER_MARKER → **镜像至少落后一个版本，请重建**"
fi
# 逐个修订号检查"独有标记"：缺哪个就说明镜像落后于该修订。
for pair in $REVISION_MARKERS; do
  rev="${pair%%:*}"
  mk="${pair##*:}"
  count=$(docker exec "$CONTAINER" grep -c "$mk" "$BIN" 2>/dev/null | tr -d '[:space:]')
  if [ "${count:-0}" -gt 0 ] 2>/dev/null; then
    ok "含 $rev 的标记（$mk）"
  else
    no "缺少 $rev（$mk）→ 镜像落后于该修订，请重建"
  fi
done

echo
echo "== 3. 启动日志里的渲染器版本 =="
logline=$(docker logs "$CONTAINER" 2>&1 | grep -m1 'playbook_renderer' || true)
if [ -n "$logline" ]; then
  ok "$(echo "$logline" | tr -d '\r' | cut -c1-200)"
else
  no "启动日志里没有 playbook_renderer 字段 → 镜像早于 v2，请重建"
fi

echo
echo "== 4. HTTP 接口自报的渲染器版本与平台能力 =="
health=$(curl -fsS --connect-timeout 3 --max-time 8 http://127.0.0.1:8080/healthz 2>/dev/null || true)
if [ -z "$health" ]; then
  info "本机 8080 未响应（平台可能不在本机或端口不同），可换地址：curl -s http://<平台IP>:8080/healthz"
else
  if echo "$health" | grep -q "$EXPECTED_RENDERER"; then
    ok "healthz 报告 $EXPECTED_RENDERER"
  else
    no "healthz 没有报告 $EXPECTED_RENDERER：$(echo "$health" | cut -c1-200)"
  fi
  if echo "$health" | grep -q "\"code_revision\":\"$EXPECTED_REVISION\""; then
    ok "healthz 报告 code_revision=$EXPECTED_REVISION"
  else
    no "healthz 没有报告 code_revision=$EXPECTED_REVISION（字段缺失=镜像更旧）"
  fi
  if echo "$health" | grep -q '"sshpass":true'; then
    ok "healthz 报告 sshpass 可用"
  else
    no "healthz 报告 sshpass 不可用（或字段缺失）→ 口令方式登录目标机会失败"
  fi
fi

echo
echo "== 5. 平台容器内的 SSH 相关程序 =="
for bin in ssh sshpass; do
  if docker exec "$CONTAINER" sh -c "command -v $bin" >/dev/null 2>&1; then
    ok "容器内有 $bin"
  else
    no "容器内没有 $bin"
  fi
done
info "口令认证必须有 sshpass（OpenSSH 不接受命令行口令，由 sshpass 代答）"
info "私钥认证不需要 sshpass —— 想立刻绕过就先改用「SSH 私钥」"

echo
echo "== 6. 落盘产物（上次执行时生成的 playbook） =="
if docker exec "$CONTAINER" test -f "$ARTIFACT" 2>/dev/null; then
  ver=$(docker exec "$CONTAINER" sed -n '3p' "$ARTIFACT" | tr -d '\r')
  info "第 3 行：$ver"
  if echo "$ver" | grep -q "$EXPECTED_RENDERER"; then
    ok "产物由 $EXPECTED_RENDERER 生成"
  else
    no "产物不是 $EXPECTED_RENDERER 生成的 → 这是上次执行时留下的旧产物（点「重新应用」会覆盖）"
  fi
  if docker exec "$CONTAINER" grep -q "Server.Version" "$ARTIFACT" 2>/dev/null; then
    no "产物里仍有 Go 模板串 --format '{{.Server.Version}}' → 是旧渲染器生成的"
  else
    ok "产物里没有 Go 模板串"
  fi
  docker exec "$CONTAINER" sed -n '13,20p' "$ARTIFACT" | sed 's/^/    /'
else
  info "还没有产物（$ARTIFACT）——保存集成并点「重新应用」后再看"
fi

echo
echo "== 结论 =="
if [ "$fail" -eq 0 ]; then
  echo "  平台已是最新代码。若远程安装仍失败，把集成详情页的备注与 ansible 输出贴出来。"
  exit 0
fi
cat <<'EOF'
  平台上还有未通过项。按失败项对号入座：

  A. 第 2/3/6 项失败（渲染器版本旧）→ 镜像没重建。注意 `docker compose up -d`
     **不会**重建镜像，必须显式 build（首次构建含 ansible，约 2~6 分钟）：

       cd <nightjar 目录>            # 必须是包含 docker-compose.yml 的那个目录
       docker compose build backend  # 这一步必须成功，失败了看输出的最后 20 行
       docker compose up -d backend
       docker logs --tail 5 mwops-backend   # 应看到 playbook_renderer=mwops-playbook v3

     常见"重建了但没生效"：
       * 只跑了 `up -d`（不重建）→ 补 `docker compose build backend`
       * build 失败（拉 Alpine 源超时）→ `.env` 里设 ALPINE_MIRROR=mirrors.aliyun.com 后重试
       * 在别的目录/别的 checkout 里 build → `docker compose config | grep -A2 'build:'` 看上下文
       * 容器名不同（多项目共存）→ 本脚本第一个参数传真实容器名

  B. 第 4/5 项失败（缺 sshpass）→ 口令方式登录目标机必失败，ansible 只会抛
     "you must install the sshpass program"。两条路：
       * 立刻可用：集成表单里改用「SSH 私钥」认证（不需要 sshpass，当前镜像即可跑）；
       * 根治：重建镜像（WITH_ANSIBLE=true 会一并装 sshpass 与 openssh-client）。

  确认后：集成详情页点「重新应用」，再跑一次本脚本，应全部通过。
EOF
exit 1
