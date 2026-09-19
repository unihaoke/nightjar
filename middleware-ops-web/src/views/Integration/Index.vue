<script setup lang="ts">
/**
 * 集成中心。
 *
 * 集成类型分两类（由后端模板的 category 决定）：
 *   - 指标监控（monitor）：Redis / MySQL / ... —— Exporter 暴露 → Prometheus 抓取 → 实例纳管 → 推荐告警规则；
 *   - 日志采集（log）：Filebeat —— 平台用 Ansible 在目标机安装，日志推送到平台 Kafka 后落入日志事件链路。
 * 两者的表单与状态语义不同：日志集成没有 Exporter/端口/抓取目标，因此这里按 category 分支渲染。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { integrationApi } from '@/api'
import { toastError } from '@/api/http'
import type { AccountSecurePayload, IntegrationAccount, IntegrationArtifacts, IntegrationInput, IntegrationOverview, IntegrationSelfCheck, IntegrationTemplate, IntegrationView } from '@/api/types'
import { envLabels, formatTime } from '@/utils/format'

const router = useRouter()

const loading = ref(false)
const overview = ref<IntegrationOverview | null>(null)
const items = ref<IntegrationView[]>([])

const dialogVisible = ref(false)
const submitting = ref(false)
const previewing = ref(false)
const editing = ref<IntegrationView | null>(null)
const formRef = ref<FormInstance>()
const activeTemplate = ref<IntegrationTemplate | null>(null)

const artifactsVisible = ref(false)
const artifacts = ref<IntegrationArtifacts | null>(null)
const artifactsTab = ref('compose')
/** 产物属于日志集成还是指标集成：同一份产物结构承载两种内容，标签页与文案必须跟着变。 */
const artifactsIsLog = ref(false)

/** 表单模型：通用字段 + 动态标签 + 动态 Exporter 参数。 */
const form = reactive({
  name: '',
  address: '',
  username: '',
  password: '',
  environment: 'dev',
  group_name: '',
  // 默认"由平台负责部署"：这是平台的卖点，关掉它等于退化成手工模式
  deploy: true,
  auto_rules: true,
  // 自动建号：需要账号的组件**默认由平台创建**（使用者不必提前建号）
  bootstrap_account: true,
  admin_username: '',
  admin_password: '',
  // 反向接网：把**目标容器**接入平台网络。默认关闭（正常方向是平台自己接进目标网络）
  join_platform_network: false,
  // Exporter 部署位置：local（本机 Docker）/ remote（远程服务器，Ansible 一键安装）
  // 默认部署到**远程服务器**：被管实例通常不在平台这台机器上。
  // 本机模式留给"平台与被管实例同机"的场景，可随时切换。
  deploy_target: 'remote' as 'local' | 'remote',
  target_host: '',
  exporter_port: undefined as number | undefined,
  install_mode: 'docker' as 'docker' | 'docker-systemd' | 'binary',
  ssh_user: '',
  ssh_port: 22,
  // SSH 认证方式：口令（需平台镜像带 sshpass）或私钥（不需要 sshpass）。
  // 与「重新应用」「账号操作」两个弹窗保持同一套语义：二选一，另一种不提交。
  ssh_method: 'password' as 'password' | 'key',
  ssh_password: '',
  ssh_key: '',
  ssh_become: true,
  labels: [] as { key: string; value: string }[],
  options: {} as Record<string, string>,
})

/** 该组件是否支持由平台代建只读监控账号（目前仅 MySQL / PostgreSQL）。 */
const bootstrapSupported = computed(() => ['mysql', 'pg'].includes(activeTemplate.value?.type || ''))

/** 平台将执行的固定模板 SQL（仅用于向使用者展示，实际语句在服务端内置）。 */
const bootstrapSQLPreview = computed(() => {
  const user = form.username || 'exporter'
  if (activeTemplate.value?.type === 'pg') {
    return `DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${user}') THEN CREATE ROLE ${user} LOGIN PASSWORD '<平台生成>'; ELSE ALTER ROLE ${user} LOGIN PASSWORD '<平台生成>'; END IF; END $$;\nGRANT pg_monitor TO ${user};`
  }
  return [
    `CREATE USER IF NOT EXISTS '${user}'@'%' IDENTIFIED WITH mysql_native_password BY '<平台生成>' WITH MAX_USER_CONNECTIONS 3;`,
    `ALTER USER '${user}'@'%' IDENTIFIED WITH mysql_native_password BY '<平台生成>';`,
    `GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO '${user}'@'%';`,
    'FLUSH PRIVILEGES;',
  ].join('\n')
})

const rules: FormRules = {
  name: [
    { required: true, message: '请输入集成名称', trigger: 'blur' },
    {
      pattern: /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/,
      message: '只能包含小写字母、数字、中划线与点，且以字母或数字开头结尾',
      trigger: 'blur',
    },
  ],
  address: [{ required: true, message: '请输入连接地址', trigger: 'blur' }],
}

/**
 * 预览某个 Exporter 参数**最终会以什么形式**传给 Exporter。
 *
 * 规则与后端 internal/integration/template.go 的 RenderEnv/RenderArgs 保持一致：
 *   - 环境变量：KEY=value（空值不传递）；
 *   - 命令行字符串：--key=value；
 *   - 命令行开关：显式为真 → --key；显式为假但上游默认开 → --no-key（关掉默认采集项的唯一写法）；
 *     显式为假且上游默认关 → 什么都不传。
 * 这样"标了命令行却是开关控件、没法输入文本"就不会再让人困惑。
 */
function optionPreview(option: IntegrationTemplate['options'][number]): string {
  const raw = String(form.options[option.key] ?? option.default ?? '').trim()
  const defaultOn = isTruthyValue(option.default || '')
  if (option.target === 'env') {
    if (option.kind === 'bool') {
      if (!raw) {
        return '（不传递该环境变量）'
      }
      return `${option.key}=${isTruthyValue(raw) ? 'true' : 'false'}`
    }
    return raw ? `${option.key}=${raw}` : '（不传递该环境变量）'
  }
  if (option.kind === 'bool') {
    if (isTruthyValue(raw)) {
      return `--${option.key}`
    }
    return defaultOn ? `--no-${option.key}（关掉上游默认开启的采集项）` : '（不传递该开关）'
  }
  return raw ? `--${option.key}=${raw}` : '（不传递该开关）'
}

/** 与后端 isTruthy 一致的真值判断。 */
function isTruthyValue(value: string): boolean {
  return ['true', '1', 'yes', 'on'].includes(value.trim().toLowerCase())
}

/**
 * 日志集成参数的控件类型。
 *
 * 为什么按键名判定而不是只看模板的 kind：日志路径是"多个 glob"、安装方式是固定三选一，
 * 后端模板把它们声明成 string 也无法表达"该用文本域/下拉"；靠人记这些键名写一次成本最低，
 * 也避免把 Filebeat 的安装方式做成手打字符串（打错只会到目标机上才失败）。
 * 「环境」也单独拿出来：它与表单顶部的「环境」是同一个语义，必须绑同一份数据。
 */
function logOptionControl(option: IntegrationTemplate['options'][number]): 'paths' | 'bool' | 'install_mode' | 'environment' | 'text' {
  switch (option.key) {
    case 'MWOPS_LOG_PATHS':
      return 'paths'
    case 'MWOPS_LOG_MULTILINE':
      return 'bool'
    case 'MWOPS_LOG_INSTALL_MODE':
      return 'install_mode'
    case 'MWOPS_LOG_ENVIRONMENT':
      return 'environment'
    default:
      return option.kind === 'bool' ? 'bool' : 'text'
  }
}

/** 日志路径是否已填写：装了 Filebeat 却没有路径，等于什么都没采。 */
const logPathsFilled = computed(() => String(form.options.MWOPS_LOG_PATHS ?? '').trim().length > 0)

/** 是否处于编辑态。 */
const isEdit = computed(() => Boolean(editing.value?.instance_id))

/** 当前弹窗里的模板是否为日志集成（Filebeat → 平台 Kafka，不涉及 Exporter 与 Prometheus）。 */
const isLog = computed(() => activeTemplate.value?.category === 'log')

/** 提交按钮文案：日志集成走 Filebeat 部署，指标集成走 Exporter 抓取。 */
const submitLabel = computed(() => {
  if (isEdit.value) {
    return isLog.value ? '保存并用 Ansible 重新部署' : '保存并重新应用'
  }
  return isLog.value ? '保存并部署 Filebeat' : '保存并集成'
})

/** 本机一键部署（Docker API）是否可用。 */
const dockerReady = computed(() => Boolean(overview.value?.docker_ok))
/** 远程安装（Ansible）是否可用：镜像带 ansible-playbook 且平台开关已打开。 */
const remoteReady = computed(() => Boolean(overview.value?.remote_ready))
/** 只要本机或远程有一种可用，就算"平台可代部署"。 */
const deployReady = computed(() => dockerReady.value || remoteReady.value)
/** 顶部状态标签：不要再在"仅远程可用"时误导成「仅生成配置」。 */
const deployReadyLabel = computed(() => {
  if (dockerReady.value) {
    return '本机一键部署已启用'
  }
  if (remoteReady.value) {
    return '远程安装可用（本机 Docker 通道不可用）'
  }
  return '仅生成配置'
})
/** 日志集成由 Ansible 在目标机安装 Filebeat，因此与平台本机 Docker 通道无关。 */
const logDeployReady = computed(() => remoteReady.value)

// ---------------------------------------------------------------------------
// 集成类型分类（后端模板的 category）：monitor=指标监控，log=日志采集
// ---------------------------------------------------------------------------
/** 分类中文标签：后端只给 category 值，展示文案统一在前端维护。 */
const categoryLabels: Record<string, string> = { monitor: '指标监控', log: '日志采集' }
/** 分类展示顺序：指标监控是平台主体能力，日志采集排在其后。 */
const categoryOrder = ['monitor', 'log']

/** 按 category 分组的模板；出现未知分类时按原值兜底展示，而不是把它藏起来。 */
const templateGroups = computed(() => {
  const groups: { key: string; label: string; items: IntegrationTemplate[] }[] = []
  for (const tpl of overview.value?.templates || []) {
    const key = tpl.category || 'monitor'
    let group = groups.find((candidate) => candidate.key === key)
    if (!group) {
      group = { key, label: categoryLabels[key] || key, items: [] }
      groups.push(group)
    }
    group.items.push(tpl)
  }
  const orderOf = (key: string) => {
    const index = categoryOrder.indexOf(key)
    return index < 0 ? categoryOrder.length : index
  }
  return groups.sort((a, b) => orderOf(a.key) - orderOf(b.key))
})

/** 已集成总数。 */
const total = computed(() => items.value.length)

/** 当前待处理的集成（有 last_error 的第一条），横幅与按钮都基于它。 */
const pendingItem = computed(() => items.value.find((item) => item.last_error) || null)

// ---------------------------------------------------------------------------
// 监控账号管理（平台代管的只读账号：查看 / 轮换口令 / 删除）
// ---------------------------------------------------------------------------
const accountsVisible = ref(false)
const accountsLoading = ref(false)
const accounts = ref<IntegrationAccount[]>([])
/** 正在重试/探测的集成 ID（用于按钮 loading）。 */
const retryingId = ref<number | null>(null)
/**
 * 「重新应用」的 SSH 凭据弹窗状态。
 *
 * 为什么单独放一份而不是复用表单里的字段：表单保存会落库（表单里的 SSH 字段只在提交时用），
 * 而重新应用是"就地重做一次部署"，需要当场收集凭据又不写回表单。
 */
const applyVisible = ref(false)
const applying = ref(false)
const applyTarget = ref<IntegrationView | null>(null)
const applySsh = reactive({ user: '', method: 'password' as 'password' | 'key', password: '', key: '', port: 22 })
/** 「重新应用」的对象是否为日志集成：决定弹窗文案（Filebeat 还是 Exporter）。 */
const applyIsLog = computed(() => (applyTarget.value ? isLogItem(applyTarget.value) : false))
/** 正在"重新核验"的集成 ID。 */
const verifyingId = ref<number | null>(null)
const probingId = ref<number | null>(null)
/** 最近一次重试/探测的结论（就地展示失败原因，而不是只弹一条 toast）。 */
const accountResult = ref<{ ok: boolean; title: string; detail: string } | null>(null)
/**
 * 「账号操作」弹窗：**先收齐凭据，再在弹窗里点按钮触发请求**。
 *
 * 为什么这样改（真实反馈：点了重试"好像没啥用"）：旧流程是
 *   点「重试建号」→ 先弹两个输入框问管理员账号口令 → 最后才发现抽屉顶部的 SSH 字段没填
 *   → 中止，什么也没发生。
 * 使用者填了半天却被一句"请先填写 SSH 凭据"打回，而凭据散落在两个地方。
 * 现在统一成一个弹窗：SSH（远程时）+ 管理员（建号/删号时）都收齐，
 * 校验通过后由弹窗里的主按钮触发请求，结果也显示在同一个弹窗里。
 */
const accountActionVisible = ref(false)
const accountActionRow = ref<IntegrationAccount | null>(null)
const accountActionKind = ref<'probe' | 'retry' | 'rotate' | 'drop'>('probe')
const accountActionLoading = ref(false)
const accountAdmin = reactive({ user: 'root', password: '' })

/** 动作是否需要管理员凭据（建号 / 删号是写操作）。 */
const accountActionNeedsAdmin = computed(() => accountActionKind.value === 'retry' || accountActionKind.value === 'drop')

/** 弹窗标题与主按钮文案。 */
const accountActionTitle = computed(() => {
  switch (accountActionKind.value) {
    case 'retry':
      return '重试建号 / 连接'
    case 'rotate':
      return '轮换监控账号口令'
    case 'drop':
      return '删除监控账号'
    default:
      return '测试连接'
  }
})
const accountActionConfirmLabel = computed(() => {
  switch (accountActionKind.value) {
    case 'retry':
      return accountAdmin.password ? '由平台重建并测试' : '只测连接并重建 Exporter'
    case 'rotate':
      return '轮换口令'
    case 'drop':
      return '确认删除'
    default:
      return '开始测试'
  }
})

/** 打开账号操作弹窗（凭据每次清空：不跨动作、跨集成复用）。 */
function openAccountAction(row: IntegrationAccount, kind: 'probe' | 'retry' | 'rotate' | 'drop'): void {
  accountActionRow.value = row
  accountActionKind.value = kind
  accountSsh.user = ''
  accountSsh.method = 'password'
  accountSsh.password = ''
  accountSsh.key = ''
  accountSsh.port = 22
  accountAdmin.user = 'root'
  accountAdmin.password = ''
  accountResult.value = null
  accountActionVisible.value = true
}

/** 弹窗内提交：先本地校验，再带着**同一份**凭据发请求。 */
async function submitAccountAction(): Promise<void> {
  const row = accountActionRow.value
  if (!row) {
    return
  }
  if (row.deploy_target === 'remote' && !sshCredsReady(accountSsh)) {
    ElMessage({ type: 'warning', message: '这是远程部署：请填写 SSH 用户名，并选择口令或私钥其中一种认证方式' })
    return
  }
  if (accountActionKind.value === 'drop' && !accountAdmin.password) {
    ElMessage({ type: 'warning', message: '删除账号需要管理员口令（仅本次使用，不落库）' })
    return
  }
  if (accountActionKind.value === 'drop') {
    const confirmed = await ElMessageBox.confirm(
      `删除 ${row.name} 上的监控账号后，该实例将不再有指标。是否继续？`,
      '删除监控账号',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    ).catch(() => false)
    if (!confirmed) {
      return
    }
  }
  accountActionLoading.value = true
  retryingId.value = row.integration_id
  probingId.value = row.integration_id
  try {
    const payload = buildSshPayload(accountSsh)
    if (accountActionKind.value === 'probe') {
      const result = await integrationApi.probeAccount(row.integration_id, payload)
      accountResult.value = {
        ok: result.ok,
        title: result.ok ? `${row.name}：连接正常` : `${row.name}：连接失败`,
        detail: result.ok ? result.output || result.message : result.message,
      }
    } else if (accountActionKind.value === 'retry') {
      // 填了管理员口令 → 幂等重跑建号 SQL；没填 → 只测连接并重建 Exporter。
      const result = await integrationApi.retryAccount(row.integration_id, {
        admin_username: accountAdmin.password ? accountAdmin.user : '',
        admin_password: accountAdmin.password,
        ...payload,
      })
      accountResult.value = {
        ok: result.ok,
        title: result.ok ? `${row.name}：账号已就绪` : `${row.name}：仍未就绪`,
        detail: result.message,
      }
      await Promise.all([loadAccounts(), load()])
    } else if (accountActionKind.value === 'rotate') {
      const result = await integrationApi.rotateAccount(row.integration_id, payload)
      accountResult.value = {
        ok: true,
        title: `${row.name}：口令已轮换`,
        detail:
          `新口令（只显示这一次，手工执行 Exporter 的 compose/docker run 时需要它）：\n${result.new_password}\n\n` +
          `平台已用新口令重建 Exporter；账号未过期，业务侧无需改动。`,
      }
      await Promise.all([loadAccounts(), load()])
    } else {
      await integrationApi.dropAccount(row.integration_id, {
        admin_username: accountAdmin.user,
        admin_password: accountAdmin.password,
        ...payload,
      })
      accountResult.value = { ok: true, title: `${row.name}：监控账号已删除`, detail: '如需恢复，请重新保存该集成并勾选自动建号。' }
      await Promise.all([loadAccounts(), load()])
    }
    accountActionVisible.value = false
  } catch (error) {
    toastError(error)
  } finally {
    accountActionLoading.value = false
    retryingId.value = null
    probingId.value = null
  }
}
/**
 * 远程集成的账号操作在**目标主机**上执行（只经 SSH + Ansible，不依赖平台 Docker），
 * 因此需要在弹窗里填一次 SSH 凭据；凭据仅本次使用、平台不落库。
 * 口令与私钥二选一——很多人只用私钥登录，界面必须支持（否则只能去改整个集成表单）。
 */
const accountSsh = reactive({ user: '', method: 'password' as 'password' | 'key', password: '', key: '', port: 22 })

/** 账号清单里是否存在远程部署的集成（决定是否显示 SSH 凭据提示）。 */
const hasRemoteAccount = computed(() => accounts.value.some((row) => row.deploy_target === 'remote'))

/**
 * 把界面上的 SSH 输入组装成后端载荷。
 *
 * 只提交选中的那种认证方式：另一种留空，避免把用户没填的字段当成"空口令"传过去，
 * 也避免口令与私钥两个字段里混进上一次留下的内容。
 */
function buildSshPayload(src: { user: string; method: 'password' | 'key'; password: string; key: string; port: number }): AccountSecurePayload {
  return {
    ssh_user: src.user.trim(),
    ssh_port: src.port,
    ssh_password: src.method === 'password' ? src.password : '',
    ssh_key: src.method === 'key' ? src.key : '',
  }
}

/** 判断一组 SSH 输入是否可用（用户 + 口令或私钥）。 */
function sshCredsReady(src: { user: string; method: 'password' | 'key'; password: string; key: string }): boolean {
  if (!src.user.trim()) {
    return false
  }
  return src.method === 'key' ? src.key.trim().length > 0 : src.password.length > 0
}

/**
 * 主表单里的 SSH 输入（字段带 ssh_ 前缀，与两个弹窗里的 state 形状不同）。
 *
 * 做一次适配就能复用同一套校验与载荷组装：口令/私钥二选一的语义只在一处定义，
 * 不会出现"弹窗支持私钥、主表单不支持"这种割裂。
 */
function formSshInput(): { user: string; method: 'password' | 'key'; password: string; key: string; port: number } {
  return { user: form.ssh_user, method: form.ssh_method, password: form.ssh_password, key: form.ssh_key, port: form.ssh_port }
}

/** 载入账号清单。 */
async function loadAccounts(): Promise<void> {
  accountsLoading.value = true
  try {
    const result = await integrationApi.accounts()
    accounts.value = result.items || []
  } catch (error) {
    toastError(error)
  } finally {
    accountsLoading.value = false
  }
}

/** 打开账号管理弹窗。 */
function openAccounts(): void {
  accountsVisible.value = true
  accountResult.value = null
  void loadAccounts()
}

/** 测试连接：只探测，不改配置。凭据在弹窗里收齐后才发请求。 */
function handleProbeAccount(row: IntegrationAccount): void {
  openAccountAction(row, 'probe')
}

/** 重试建号 / 连接：凭据在弹窗里收齐后由弹窗按钮触发。 */
function handleRetryAccount(row: IntegrationAccount): void {
  openAccountAction(row, "retry")
}

/** 轮换口令：账号改自己的口令，不需要管理员凭据。 */
function handleRotateAccount(row: IntegrationAccount): void {
  openAccountAction(row, "rotate")
}

/** 删除账号：破坏性写操作，需要管理员凭据（弹窗里填、弹窗里确认）。 */
function handleDropAccount(row: IntegrationAccount): void {
  openAccountAction(row, "drop")
}

/** 载入概览与列表。 */
async function load(): Promise<void> {
  loading.value = true
  try {
    const [ov, list] = await Promise.all([integrationApi.overview(), integrationApi.list()])
    overview.value = ov
    items.value = list.items || []
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

/** 打开集成弹窗（template 为空表示编辑已有集成）。 */
function openInstall(template: IntegrationTemplate, item?: IntegrationView): void {
  activeTemplate.value = template
  editing.value = item || null
  form.name = item?.name || ''
  form.address = item?.address || ''
  form.username = item?.username || ''
  form.password = ''
  form.environment = item?.environment || template.default_environment || 'dev'
  form.group_name = item?.group_name || ''
  // 默认由平台部署（新建时）；编辑已有集成时沿用它的部署位置语义
  form.deploy = true
  form.auto_rules = true
  form.bootstrap_account = bootstrapSupported.value
  form.admin_username = ''
  form.admin_password = ''
  form.join_platform_network = item?.join_platform_network ?? false
  // 部署位置可从已有集成回填；SSH 凭据绝不回填（平台不保存）
  form.deploy_target = (item?.deploy_target as 'local' | 'remote') || 'remote'
  form.target_host = item?.target_host || ''
  form.exporter_port = item?.exporter_host_port || undefined
  form.install_mode = (item?.install_mode as 'docker' | 'docker-systemd' | 'binary') || 'docker'
  form.ssh_user = ''
  form.ssh_port = 22
  form.ssh_method = 'password'
  form.ssh_password = ''
  form.ssh_key = ''
  form.ssh_become = true
  form.labels = Object.entries(item?.labels || {}).map(([key, value]) => ({ key, value }))
  const options: Record<string, string> = {}
  for (const option of template.options) {
    options[option.key] = item?.options?.[option.key] ?? option.default ?? ''
  }
  form.options = options
  dialogVisible.value = true
  formRef.value?.clearValidate()
}

/** 组装提交载荷。 */
function buildPayload(): IntegrationInput {
  const labels: Record<string, string> = {}
  for (const row of form.labels) {
    if (row.key.trim()) {
      labels[row.key.trim()] = row.value
    }
  }
  const options: Record<string, string> = {}
  for (const [key, value] of Object.entries(form.options)) {
    if (value !== '' && value !== undefined && value !== null) {
      options[key] = String(value)
    }
  }
  // 日志集成的「环境」只保留表单顶部这一个入口（后端优先读模板参数），
  // 提交时把两者对齐，避免表单显示 prod 而 Filebeat 下发 dev。
  if (isLog.value) {
    options.MWOPS_LOG_ENVIRONMENT = form.environment || 'dev'
  }
  const payload: IntegrationInput = {
    name: form.name.trim(),
    mw_type: activeTemplate.value?.type || '',
    address: form.address.trim(),
    username: form.username.trim(),
    labels,
    options,
    environment: form.environment,
    group_name: form.group_name,
    deploy: form.deploy,
    auto_rules: form.auto_rules,
    // 始终显式提交，避免"取消勾选后编辑保存仍生效"
    join_platform_network: form.join_platform_network,
    deploy_target: form.deploy_target,
  }
  // 远程安装：只在选中 remote 时提交目标与 SSH 凭据（凭据仅本次请求使用，平台不落库）
  if (form.deploy_target === 'remote') {
    // 日志集成的表单只有一个地址栏（就是目标服务器），这里把它同时作为 target_host 下发，
    // 避免同一个地址在两个输入框里各存一份、改了一个另一个还是旧值。
    payload.target_host = isLog.value ? form.address.trim() : form.target_host.trim()
    payload.ssh_user = form.ssh_user.trim()
    payload.ssh_port = form.ssh_port
    // 二选一：只提交选中的那种认证方式，另一种留空——
    // 否则上次留下的内容会被当成"空口令/空私钥"传过去，或在目标机上混用两种认证。
    payload.ssh_password = form.ssh_method === 'password' ? form.ssh_password : ''
    payload.ssh_key = form.ssh_method === 'key' ? form.ssh_key : ''
    payload.ssh_become = form.ssh_become
    // Filebeat 的安装方式由模板参数（MWOPS_LOG_INSTALL_MODE）决定，
    // 指标集成的 install_mode/exporter_port 对日志集成没有意义，不能顺手带上。
    if (!isLog.value) {
      payload.install_mode = form.install_mode
      if (form.exporter_port) {
        payload.exporter_port = form.exporter_port
      }
    }
  }
  // 代建账号：只有勾选时才提交管理凭据（否则一个字节也不上传）
  if (form.bootstrap_account) {
    payload.bootstrap_account = true
    payload.admin_username = form.admin_username.trim()
    payload.admin_password = form.admin_password
  }
  if (form.password) {
    payload.password = form.password
  }
  return payload
}

/** 预览生成的采集配置。 */
async function handlePreview(): Promise<void> {
  if (!formRef.value) {
    return
  }
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) {
    return
  }
  previewing.value = true
  try {
    artifacts.value = await integrationApi.preview(buildPayload())
    artifactsIsLog.value = isLog.value
    artifactsTab.value = 'compose'
    artifactsVisible.value = true
  } catch (error) {
    toastError(error)
  } finally {
    previewing.value = false
  }
}

/** 保存（新建或更新）。 */
async function handleSubmit(): Promise<void> {
  if (!formRef.value) {
    return
  }
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) {
    return
  }
  // 日志路径不是表单 rules 里的字段（它来自模板）却决定采集有没有内容，必须在这里拦住。
  if (isLog.value && !logPathsFilled.value) {
    ElMessage({ type: 'warning', message: '请填写日志路径（可多行或逗号分隔），否则 Filebeat 装好也不会采集任何日志' })
    return
  }
  // 远程部署且勾选了"由平台部署"：凭据缺了不会让保存失败，只会留下一条
  // 「远程安装需要 SSH 凭据」的待处理项，等于白跑一趟——这里当场收齐（口令或私钥二选一）。
  if (form.deploy_target === 'remote' && form.deploy && !sshCredsReady(formSshInput())) {
    ElMessage({
      type: 'warning',
      message: '远程部署需要 SSH 凭据：请填写 SSH 用户名，并选择「口令」或「私钥」其中一种认证方式（凭据仅本次使用、不落库）',
    })
    return
  }
  submitting.value = true
  try {
    const payload = buildPayload()
    const saved = editing.value
      ? await integrationApi.update(editing.value.instance_id, payload)
      : await integrationApi.create(payload)
    if (saved.last_error) {
      ElMessage({ type: 'warning', message: `已保存，但存在待处理项：${saved.last_error}` })
    } else if (isLog.value) {
      ElMessage({
        type: 'success',
        message: editing.value
          ? '日志集成已更新：平台将重放一次 Filebeat 部署（已安装则只校验配置）'
          : '日志集成已创建：平台将用 Ansible 在目标机安装 Filebeat 并把日志推送到平台 Kafka',
      })
    } else {
      ElMessage({ type: 'success', message: editing.value ? '集成已更新' : '集成已创建，指标将在 30 秒内出现在监控页' })
    }
    dialogVisible.value = false
    await load()
  } catch (error) {
    toastError(error)
  } finally {
    submitting.value = false
  }
}

/**
 * 重新应用（重写 file_sd + 重建/重装 Exporter）。
 *
 * 远程集成必须先收集 SSH 凭据：安装动作发生在目标机上，凭据不落库，
 * 因此**不能**在按钮里直接发请求——否则必然留下一条"远程安装需要 SSH 凭据"的待处理项，
 * 使用者还得到处找地方补凭据（真实反馈）。
 */
async function handleApply(item: IntegrationView): Promise<void> {
  if (item.deploy_target === 'remote') {
    applyTarget.value = item
    applySsh.user = ''
    applySsh.method = 'password'
    applySsh.password = ''
    applySsh.key = ''
    applySsh.port = 22
    applyVisible.value = true
    return
  }
  await runApply(item, {})
}

/** 提交重新应用（带可选的 SSH 凭据）。 */
async function runApply(item: IntegrationView, payload: AccountSecurePayload): Promise<void> {
  applying.value = true
  try {
    const saved = await integrationApi.apply(item.instance_id, payload)
    if (saved.last_error) {
      ElMessage({ type: 'warning', message: saved.last_error })
    } else if (isLogItem(item)) {
      ElMessage({ type: 'success', message: '已开始重新应用：后台用 Ansible 安装/校验 Filebeat（已装则跳过），完成后此处显示结果（可点刷新）' })
    } else {
      ElMessage({ type: 'success', message: '已开始重新应用：后台重建/重装 Exporter，完成后此处显示结果（可点刷新）' })
    }
    applyVisible.value = false
    await load()
  } catch (error) {
    toastError(error)
  } finally {
    applying.value = false
  }
}

/** 弹窗内的"开始重新应用"。 */
async function submitApply(): Promise<void> {
  if (!applyTarget.value) {
    return
  }
  if (!sshCredsReady(applySsh)) {
    ElMessage({ type: 'warning', message: '请填写 SSH 用户名，并选择口令或私钥其中一种认证方式' })
    return
  }
  await runApply(applyTarget.value, buildSshPayload(applySsh))
}

/**
 * 集成自检：一次点击按**环节**回答"哪一环断了"。
 *
 * 覆盖：平台→Exporter 端口、Exporter 是否在位、Prometheus 是否已抓取、业务指标是否真的有数据。
 * 日志集成由后端换成另一条链路（平台 Kafka → 目标机 Filebeat → 目标机到 Kafka 的连通性 → 是否真的收到日志）。
 * 只读、不需要凭据、不重装 —— 比"翻日志猜"直接得多（真实反馈）。
 */
const selfCheckVisible = ref(false)
const selfCheckLoading = ref(false)
const selfCheckResult = ref<IntegrationSelfCheck | null>(null)
const selfCheckName = ref('')
/** 当前自检对象是否为日志集成：决定加载文案与失败后的动作，别把 Filebeat 说成 Exporter。 */
const selfCheckIsLog = ref(false)

async function handleSelfCheck(item: IntegrationView): Promise<void> {
  selfCheckName.value = item.name
  selfCheckIsLog.value = isLogItem(item)
  selfCheckResult.value = null
  selfCheckVisible.value = true
  selfCheckLoading.value = true
  try {
    selfCheckResult.value = await integrationApi.selfCheck(item.instance_id)
  } catch (error) {
    toastError(error)
  } finally {
    selfCheckLoading.value = false
  }
}

/** 自检结果里的状态 → 标签样式/图标。 */
function selfCheckTagType(status: string): 'success' | 'warning' | 'danger' {
  if (status === 'ok') return 'success'
  if (status === 'warn') return 'warning'
  return 'danger'
}
function selfCheckTagText(status: string): string {
  if (status === 'ok') return '通过'
  if (status === 'warn') return '无法判定'
  return '失败'
}

/** 按自检结论的 next_action 跳转（与待处理横幅同一套判定）。 */
function followSelfCheckAction(): void {
  const row = items.value.find((item) => item.name === selfCheckName.value)
  if (!row) {
    return
  }
  selfCheckVisible.value = false
  if (selfCheckResult.value?.next_action === 'retry_account') {
    openAccounts()
    return
  }
  void handleApply(row)
}

/**
 * 重新核验：按 Prometheus 现状刷新状态。
 *
 * 为什么需要这个按钮（真实反馈：Redis 已经正常被监控了，状态却一直是「待处理」）：
 * 核验只在**部署之后**跑几次（约 35s/60s/85s），之后不再自动核对；
 * 若外部原因在那之后才修好（改地址、放通安全组、Exporter 自己起来了），
 * 状态就会一直停在待处理。重新应用能解决，但它要重填 SSH 凭据、还会真的重装——
 * 代价完全不成比例。这个按钮不需要凭据、没有副作用，只是"再看一眼"。
 */
async function handleVerify(item: IntegrationView): Promise<void> {
  verifyingId.value = item.instance_id
  try {
    const saved = await integrationApi.verify(item.instance_id)
    if (saved.last_error) {
      ElMessage({ type: 'warning', message: '仍未通过：' + saved.last_error })
    } else if (isLogItem(item)) {
      ElMessage({ type: 'success', message: '核验通过：部署结论已按现状刷新，待处理已清除' })
    } else {
      ElMessage({ type: 'success', message: '核验通过：抓取目标已 up，待处理已清除' })
    }
    await load()
  } catch (error) {
    toastError(error)
  } finally {
    verifyingId.value = null
  }
}

/** 按类型取模板。 */
function templateOf(mwType: string): IntegrationTemplate | null {
  return overview.value?.templates.find((candidate) => candidate.type === mwType) || null
}

/** 某条集成是否为日志集成（按模板 category 判定；模板缺失时按指标集成处理，避免误显示）。 */
function isLogItem(item: IntegrationView): boolean {
  return templateOf(item.mw_type)?.category === 'log'
}

/**
 * 日志集成的部署结论提示。
 *
 * 平台不持有目标机的进程视图（Ansible 装完就结束），所以这里只回答
 * "装在哪台机器、什么时候装的/核验的"，运行状态由自检与日志页数据回答。
 */
function logDeployTooltip(item: IntegrationView): string {
  const host = item.target_host || item.address || '目标机'
  const stamp = item.remote_installed_at ? `，最近一次安装/核验：${formatTime(item.remote_installed_at)}` : ''
  return `目标机 ${host}：由平台经 Ansible 安装并校验 Filebeat（已装则跳过）${stamp}。采集是否在跑请看自检结论或日志页数据。`
}

/** 打开编辑弹窗。 */
function openEdit(item: IntegrationView): void {
  const template = templateOf(item.mw_type)
  if (!template) {
    ElMessage({ type: 'warning', message: '未找到该组件的模板' })
    return
  }
  openInstall(template, item)
}

/** 查看该集成生成的配置。 */
async function handleShowArtifacts(item: IntegrationView): Promise<void> {
  if (!templateOf(item.mw_type)) {
    ElMessage({ type: 'warning', message: '未找到该组件的模板' })
    return
  }
  try {
    artifacts.value = await integrationApi.preview({
      name: item.name,
      mw_type: item.mw_type,
      address: item.address,
      username: item.username,
      labels: item.labels,
      options: item.options,
      environment: item.environment,
      group_name: item.group_name,
    })
    artifactsTab.value = 'compose'
    artifactsIsLog.value = isLogItem(item)
    artifactsVisible.value = true
  } catch (error) {
    toastError(error)
  }
}

/** 删除集成。 */
async function handleDelete(item: IntegrationView): Promise<void> {
  try {
    await ElMessageBox.confirm(
      isLogItem(item)
        ? `删除日志集成「${item.name}」后平台不再下发/校验它的 Filebeat 配置（目标机上已装的 Filebeat 不会自动卸载），历史日志事件与告警记录保留。是否继续？`
        : `删除集成「${item.name}」会同时移除其抓取目标与 Exporter 容器，历史告警与诊断记录保留。是否继续？`,
      '删除集成',
      { type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await integrationApi.remove(item.instance_id)
    ElMessage({ type: 'success', message: '集成已删除' })
    await load()
  } catch (error) {
    toastError(error)
  }
}

/** 复制文本到剪贴板。 */
async function copy(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
    ElMessage({ type: 'success', message: '已复制' })
  } catch {
    ElMessage({ type: 'warning', message: '浏览器禁止访问剪贴板，请手动选择复制' })
  }
}

/** 跳转到实例详情（监控/自检入口）。 */
function goDetail(item: IntegrationView): void {
  void router.push({ name: 'middleware-detail', params: { id: String(item.instance_id) } })
}

/** 新增一行自定义标签。 */
function addLabel(): void {
  form.labels.push({ key: '', value: '' })
}

/** 删除一行自定义标签。 */
function removeLabel(index: number): void {
  form.labels.splice(index, 1)
}

onMounted(load)
</script>

<template>
  <div class="page" v-loading="loading">
    <div class="page-header">
      <div>
        <h2 class="page-title">
          集成中心
          <el-tag v-if="total > 0" size="small" effect="plain">已集成 {{ total }}</el-tag>
        </h2>
        <p class="page-subtitle">
          指标监控：选组件 → 填地址与账号 → 保存即完成 Exporter 暴露与 Prometheus 抓取（<span class="mono">{{ overview?.file_sd_path }}</span>）；
          日志采集：填目标服务器与日志路径 → 平台用 Ansible 安装 Filebeat 并推送到平台 Kafka。
        </p>
      </div>
      <div class="row">
        <el-button size="small" @click="load">刷新</el-button>
        <el-button size="small" @click="openAccounts">监控账号</el-button>
        <el-tag size="small" :type="deployReady ? 'success' : 'info'" effect="light">
          {{ deployReadyLabel }}
        </el-tag>
      </div>
    </div>


    <!-- 集成类型：按后端返回的 category 分组（指标监控 / 日志采集）。
         日志类型的文案全部取自模板本身，前端不写死任何中间件名。 -->
    <div class="card">
      <h3 class="card-title">可集成类型</h3>
      <div v-for="group in templateGroups" :key="group.key" class="tpl-group">
        <div class="tpl-group-head">
          <el-tag size="small" effect="plain" :type="group.key === 'log' ? 'warning' : 'info'">{{ group.label }}</el-tag>
          <span class="muted">
            {{ group.key === 'log'
              ? 'Filebeat 在目标机采集 → 平台 Kafka 消费 → 日志事件链路（不涉及 Exporter 与 Prometheus）'
              : 'Exporter 暴露指标 → Prometheus 抓取 → 实例纳管 → 推荐告警规则' }}
          </span>
          <span v-if="group.key === 'log' && !logDeployReady" class="text-warning">
            Ansible 通道不可用：日志集成当前无法自动部署
          </span>
        </div>
        <div class="tpl-grid">
          <div v-for="tpl in group.items" :key="tpl.type" class="tpl-card">
            <div class="tpl-head">
              <span class="tpl-name">{{ tpl.name }}</span>
              <el-tag v-if="tpl.integrated" size="small" effect="light" type="success">已集成 {{ tpl.integrated }}</el-tag>
              <el-tag v-else-if="tpl.phase === 2" size="small" effect="plain">仅纳管</el-tag>
            </div>
            <p class="tpl-component mono">{{ tpl.component }}</p>
            <p class="tpl-desc">{{ tpl.description }}</p>
            <div class="tpl-foot">
              <!-- 日志集成没有 Exporter 端口：显示端口号只会让人以为要去开放它 -->
              <span v-if="tpl.category === 'log'" class="muted">由 Ansible 部署到目标机</span>
              <span v-else class="muted mono">:{{ tpl.exporter_port }}</span>
              <el-button type="primary" size="small" @click="openInstall(tpl)">集成</el-button>
            </div>
          </div>
        </div>
      </div>
      <el-empty v-if="templateGroups.length === 0" description="模板注册表为空（后端未返回任何集成类型）" :image-size="72" />
    </div>

    <!-- 已集成 -->
    <div class="card">
      <h3 class="card-title">已集成实例</h3>
      <div v-if="items.length === 0">
        <el-empty description="还没有集成任何类型，从上面的类型卡片开始" :image-size="72" />
      </div>
      <div v-else class="table-scroll">
        <el-table :data="items" size="small">
          <el-table-column prop="name" label="集成名称" min-width="150" show-overflow-tooltip />
          <el-table-column label="组件" width="110">
            <template #default="{ row }">
              <el-tag size="small" effect="plain">{{ row.component || row.mw_type }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="地址" min-width="170">
            <template #default="{ row }">
              <span class="mono">{{ row.address }}</span>
            </template>
          </el-table-column>
          <el-table-column label="环境/分组" width="130">
            <template #default="{ row }">{{ envLabels[row.environment] || row.environment }} / {{ row.group_name || '-' }}</template>
          </el-table-column>
          <el-table-column label="采集部署" width="150">
            <template #default="{ row }">
              <!-- 日志集成没有 Exporter 容器可 inspect，结论由 Ansible 的安装/校验结果给出 -->
              <el-tooltip v-if="isLogItem(row)" placement="top" :content="logDeployTooltip(row)">
                <el-tag size="small" type="info" effect="plain">Filebeat（目标机）</el-tag>
              </el-tooltip>
              <!-- 远程部署：容器在**目标机**上，平台没有那台机器的 docker 通道，
                   因此 container_status 必然为空——此时说「未托管」是误导（真实反馈）。 -->
              <el-tooltip
                v-else-if="row.deploy_target === 'remote'"
                placement="top"
                :content="`由平台经 Ansible 安装到 ${row.target_host || '目标机'}:${row.exporter_host_port || ''}，` +
                  `容器归那台机器管理${row.remote_installed_at ? '（' + formatTime(row.remote_installed_at) + ' 安装）' : ''}。` +
                  `平台无法 inspect 它，运行状态请看抓取指标（如 redis_up）或到目标机 docker ps。`"
              >
                <el-tag size="small" type="info" effect="plain">远程（目标机）</el-tag>
              </el-tooltip>
              <el-tooltip v-else-if="row.deploy_note" :content="row.deploy_note" placement="top">
                <el-tag v-if="row.container_status" size="small" :type="row.container_status === 'running' ? 'success' : 'danger'">
                  {{ row.container_status }}
                </el-tag>
                <span v-else class="muted">未托管</span>
              </el-tooltip>
              <template v-else>
                <el-tag v-if="row.container_status" size="small" :type="row.container_status === 'running' ? 'success' : 'danger'">
                  {{ row.container_status }}
                </el-tag>
                <span v-else class="muted">未托管</span>
              </template>
            </template>
          </el-table-column>
          <el-table-column label="部署结论" min-width="150">
            <template #default="{ row }">
              <!-- 待处理必须能就地看到原因：否则只能去底部横幅猜是哪一条（真实反馈）。
                   日志集成与指标集成同用 last_error / next_action，只是结论口径不同。 -->
              <el-tooltip v-if="row.last_error" :content="row.last_error" placement="top">
                <el-tag size="small" type="danger" effect="light">待处理</el-tag>
              </el-tooltip>
              <el-tooltip
                v-else-if="row.applied_at"
                placement="top"
                :content="isLogItem(row) ? '部署结论：Ansible 已在目标机安装/校验 Filebeat' : '部署结论：抓取目标已写入并核验通过'"
              >
                <el-tag size="small" type="success" effect="light">已应用</el-tag>
              </el-tooltip>
              <el-tooltip
                v-else
                placement="top"
                :content="isLogItem(row) ? '尚未部署：点「重新应用」让平台用 Ansible 在目标机安装 Filebeat' : '尚未应用：保存后平台会写入抓取目标'"
              >
                <el-tag size="small" effect="plain">待应用</el-tag>
              </el-tooltip>
            </template>
          </el-table-column>
          <el-table-column label="最近应用" width="150">
            <template #default="{ row }">
              <span class="muted">{{ row.applied_at ? formatTime(row.applied_at) : '-' }}</span>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="270" fixed="right">
            <template #default="{ row }">
              <!-- 实例详情页是 Prometheus 指标视图，对日志集成没有意义；日志看自检与日志页 -->
              <el-button v-if="!isLogItem(row)" text size="small" @click="goDetail(row)">监控/自检</el-button>
              <el-button text size="small" @click="handleShowArtifacts(row)">
                {{ isLogItem(row) ? 'Filebeat 配置' : '配置' }}
              </el-button>
              <el-button text size="small" @click="openEdit(row)">编辑</el-button>
              <el-button
                text
                size="small"
                :loading="verifyingId === row.instance_id"
                @click="handleVerify(row)"
              >重新核验</el-button>
              <el-button text size="small" @click="handleSelfCheck(row)">自检</el-button>
              <el-button text size="small" @click="handleApply(row)">重新应用</el-button>
              <el-button text size="small" type="danger" @click="handleDelete(row)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
      </div>
      <div v-if="pendingItem" class="pending-row">
        <span class="muted">待处理项：{{ pendingItem.last_error }}</span>
        <!-- 先给"零代价"的核验：外部原因若已修好，点一下就清除，不必重填凭据重装。 -->
        <el-button
          text
          size="small"
          :loading="verifyingId === pendingItem.instance_id"
          @click="handleVerify(pendingItem)"
        >重新核验</el-button>
        <el-button text size="small" @click="handleSelfCheck(pendingItem)">自检</el-button>
        <!-- 按钮由后端的 next_action 决定：部署类失败→重新应用（远程会先问 SSH 凭据），
             账号类失败→去重试建号。避免两个入口互相推诿、让人以为功能重复。
             日志集成没有监控账号，那类动作（重试建号）不出现，只留自检与重新应用。 -->
        <el-button
          v-if="pendingItem.next_action === 'retry_account' && !isLogItem(pendingItem)"
          text
          size="small"
          type="primary"
          @click="openAccounts"
        >{{ pendingItem.next_action_label || '去重试建号' }}</el-button>
        <el-button
          v-else-if="pendingItem.next_action === 'reapply'"
          text
          size="small"
          type="primary"
          @click="handleApply(pendingItem)"
        >{{ pendingItem.next_action_label || '重新应用' }}</el-button>
        <el-button
          v-else-if="!isLogItem(pendingItem)"
          text
          size="small"
          type="primary"
          @click="openAccounts"
        >去重试 / 测试连接</el-button>
      </div>
      <p v-if="items.some((item) => item.deploy_note)" class="muted note">
        平台自动完成：{{ items.find((item) => item.deploy_note)?.deploy_note }}
      </p>
    </div>

    <!-- 集成配置弹窗 -->
    <el-dialog
      v-model="dialogVisible"
      :title="`${isEdit ? '编辑' : '集成'} ${activeTemplate?.name || ''}`"
      width="720px"
      :close-on-click-modal="false"
    >
      <el-form ref="formRef" :model="form" :rules="rules" label-position="top">
        <el-row :gutter="12">
          <el-col :xs="24" :sm="12">
            <el-form-item label="集成名称" prop="name">
              <el-input v-model="form.name" placeholder="如 legacy-redis" />
            </el-form-item>
            <p v-if="isLog" class="field-hint">
              唯一，同时作为日志事件里的服务标识与目标机上 Filebeat 的配置目录名。
            </p>
            <p v-else class="field-hint">
              唯一，且必须与 Prometheus 的 <span class="mono">instance_name</span> 一致（平台按它定位指标）。
            </p>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item :label="activeTemplate?.address_label || (isLog ? '目标服务器' : '连接地址')" prop="address">
              <el-input
                v-model="form.address"
                :placeholder="activeTemplate?.address_hint || (isLog ? '如 10.0.0.31；平台与被管机同机时填 127.0.0.1' : '')"
              />
            </el-form-item>
            <!-- 日志集成的地址就是"哪台机器"，不存在端口可省略这回事 -->
            <p v-if="isLog" class="field-hint">
              被管服务器地址：平台经 SSH + Ansible 在它上面安装 Filebeat，它也是日志事件的服务器标识。
            </p>
            <p v-else class="field-hint">端口可省略，默认 {{ activeTemplate?.default_port }}。</p>
          </el-col>
          <template v-if="activeTemplate?.needs_auth && !isLog">
            <el-col :xs="24" :sm="12">
              <el-form-item label="只读监控账号（采集用）">
                <el-input v-model="form.username" placeholder="留空用默认 mwops_exporter" />
              </el-form-item>
              <p class="field-hint">
                ① 这是<b>采集指标</b>用的账号：平台把它填进 Exporter，长期存在。
                <template v-if="bootstrapSupported && form.bootstrap_account">
                  勾选了下方「由平台建号」时，平台会按这个名字在建号 SQL 里创建它。
                </template>
                <template v-else-if="bootstrapSupported">未勾选建号时，它必须是<b>你已经建好</b>的账号。</template>
              </p>
              <p v-if="activeTemplate?.type === 'redis'" class="field-hint">
                Redis 请<b>留空</b>：自建 Redis 通常只配了 requirepass（default 用户），
                填了用户名会让 Exporter 发 <span class="mono">AUTH &lt;用户名&gt; &lt;口令&gt;</span>，
                结果 <span class="mono">WRONGPASS</span>、<span class="mono">redis_up=0</span>。
              </p>
            </el-col>
            <el-col :xs="24" :sm="12">
              <el-form-item :label="isEdit ? '口令（留空表示不修改）' : '口令（采集用账号的）'">
                <el-input v-model="form.password" type="password" show-password placeholder="AES-256 加密存储" />
              </el-form-item>
              <p class="field-hint">
                ② 这是上面那个<b>只读监控账号</b>的口令，平台加密保存并注入 Exporter。
                勾选建号且此处留空时，口令由平台生成。
              </p>
            </el-col>
          </template>
          <el-col :xs="24" :sm="12">
            <el-form-item label="环境">
              <el-select v-model="form.environment" class="mobile-block">
                <el-option label="开发" value="dev" />
                <el-option label="预发" value="staging" />
                <el-option label="生产" value="prod" />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="分组">
              <el-input v-model="form.group_name" placeholder="如 interview" />
            </el-form-item>
          </el-col>
        </el-row>

        <!-- 自定义标签写入的是指标 label，日志集成用的是模板参数，此处不显示以免产生"标签会进日志字段"的误解 -->
        <template v-if="!isLog">
          <el-divider content-position="left">自定义标签（写入指标 label）</el-divider>
          <div v-for="(row, index) in form.labels" :key="index" class="kv-row">
            <el-input v-model="row.key" placeholder="标签名，如 team" />
            <el-input v-model="row.value" placeholder="标签值，如 interview" />
            <el-button text type="danger" @click="removeLabel(index)">删除</el-button>
          </div>
          <el-button text size="small" @click="addLabel">+ 添加标签</el-button>
        </template>

        <!-- 采集参数（日志集成）：日志路径/多行合并/安装方式是这套集成的核心，
             不能像 Exporter 参数那样折叠进"高级"里。 -->
        <template v-if="isLog">
          <el-divider content-position="left">采集参数</el-divider>
          <el-alert
            type="info"
            :closable="false"
            show-icon
            class="mb"
            title="平台会用 Ansible 在目标机安装 Filebeat（已安装则跳过），Filebeat 将日志推送到平台 Kafka"
          >
            <p class="field-hint">
              采集在目标机上由 Filebeat 完成：日志先落到平台 Kafka，再由平台消费进既有的日志事件链路
              （错误指纹聚合 → 告警 → AI 诊断）。平台不接触你的应用容器，也不需要目标机额外开放端口，
              只需目标机能连上平台 Kafka 的对外地址。
            </p>
            <p class="field-hint">
              平台按<b>渲染后的配置内容</b>判断是否需要重启：内容没变就不动它，重复保存不会反复重启采集。
            </p>
          </el-alert>
          <div v-for="option in activeTemplate?.options || []" :key="option.key" class="option-row">
            <div class="option-main">
              <span class="option-label">{{ option.label }}</span>
              <p class="field-hint">{{ option.help }}</p>
              <p v-if="logOptionControl(option) === 'environment'" class="field-hint">
                与顶部的「环境」是同一个值：这里改了那里也会变。
              </p>
            </div>
            <!-- 日志路径常是多个 glob：普通输入框会把它们挤成一行 -->
            <el-input
              v-if="logOptionControl(option) === 'paths'"
              v-model="form.options[option.key]"
              type="textarea"
              :rows="3"
              class="option-textarea"
              :placeholder="option.default || '/var/log/app/*.log'"
            />
            <!-- 值是固定的枚举/布尔，用控件而不是让使用者手打 -->
            <el-switch
              v-else-if="logOptionControl(option) === 'bool'"
              v-model="form.options[option.key]"
              active-value="true"
              inactive-value="false"
            />
            <el-select
              v-else-if="logOptionControl(option) === 'install_mode'"
              v-model="form.options[option.key]"
              class="option-input"
            >
              <el-option label="auto（已有 Filebeat 就复用，否则按 Docker/安装包二选一）" value="auto" />
              <el-option label="package（官方 deb/rpm + systemd 单元）" value="package" />
              <el-option label="docker（官方 Filebeat 容器，需目标机有 Docker）" value="docker" />
            </el-select>
            <!-- 与顶部「环境」是同一个值：后端解析时优先取模板参数，绑同一份数据才不会两边打架 -->
            <el-select
              v-else-if="logOptionControl(option) === 'environment'"
              v-model="form.environment"
              class="option-input"
            >
              <el-option label="开发" value="dev" />
              <el-option label="预发" value="staging" />
              <el-option label="生产" value="prod" />
            </el-select>
            <el-input v-else v-model="form.options[option.key]" class="option-input" :placeholder="option.default" />
          </div>
          <p class="field-hint">平台只透传模板声明的参数，不接受任意自定义配置。</p>
        </template>

        <!-- Exporter 参数（指标集成）：默认收起。绝大多数场景用模板默认值即可，
             只有少数"看环境"的开关（云 Redis 集群架构、node 的挂载排除正则）才需要动。 -->
        <el-collapse v-else-if="(activeTemplate?.options || []).length > 0" class="advanced-collapse">
          <el-collapse-item name="exporter-options">
            <template #title>
              <span class="collapse-title">
                高级：Exporter 参数（{{ (activeTemplate?.options || []).length }} 项，一般不用改）
              </span>
            </template>
            <p class="field-hint">
              默认值已按官方镜像适配，直接保存即可。参数分两类，<b>控制项不同</b>：
              <b>开关型</b>（如 <span class="mono">--collect.global_status</span>）用开关控制，
              <b>不能输入文本</b>；<b>字符串型</b>（如 <span class="mono">--path.rootfs</span>）才提供输入框。
              每行下方的「将传递」显示了它最终传给 Exporter 的形式。
            </p>
            <p class="field-hint">
              平台只透传模板声明的参数，<b>不接受任意自定义 flag</b>（避免把平台变成任意命令入口）；
              确实需要某个未暴露的开关时，请把它加进组件模板。
            </p>
            <div v-for="option in activeTemplate?.options || []" :key="option.key" class="option-row">
              <div class="option-main">
                <span class="option-label">{{ option.label }}</span>
                <el-tag size="small" effect="plain" type="info">
                  {{ option.target === 'env' ? '环境变量' : '命令行' }}
                </el-tag>
                <p class="field-hint">{{ option.help }}</p>
                <p class="field-hint mono">将传递：{{ optionPreview(option) }}</p>
              </div>
              <el-switch
                v-if="option.kind === 'bool'"
                v-model="form.options[option.key]"
                active-value="true"
                inactive-value="false"
              />
              <el-input v-else v-model="form.options[option.key]" class="option-input" :placeholder="option.default" />
            </div>
          </el-collapse-item>
        </el-collapse>

        <el-divider content-position="left">{{ isLog ? '部署位置与目标机' : 'Exporter 部署位置' }}</el-divider>
        <el-form-item label="部署到">
          <el-select v-model="form.deploy_target" class="mobile-block">
            <el-option
              :label="isLog
                ? '远程服务器（平台用内置 Ansible playbook + SSH 安装 Filebeat）— 默认'
                : '远程服务器（平台用内置 Ansible playbook 一键安装）— 默认'"
              value="remote"
            />
            <el-option
              :label="isLog
                ? '本机（平台在这台机器上执行 Ansible，目标服务器请填 127.0.0.1）'
                : '本机（平台用 Docker API 创建容器，需要 docker.sock）'"
              value="local"
            />
          </el-select>
        </el-form-item>
        <!-- Docker 通道提示只在「本机」且是指标集成时出现：日志集成本机走的是 Ansible，不是 Docker -->
        <el-alert
          v-if="!isLog && form.deploy_target === 'local' && overview?.docker_note"
          type="info"
          :closable="false"
          show-icon
          class="mb"
          :title="overview.docker_note"
        />
        <el-alert
          v-if="form.deploy_target === 'remote' && !remoteReady"
          type="warning"
          :closable="false"
          show-icon
          class="mb"
          title="远程安装当前不可用：平台镜像没有 ansible-playbook 或开关未打开"
        >
          <p class="field-hint">
            请用 <span class="mono">WITH_ANSIBLE=true</span> 重新构建后端镜像，并在平台
            <span class="mono">.env</span> 里设
            <span class="mono">INTEGRATION_ALLOW_REMOTE_INSTALL=true</span>、
            <span class="mono">INTEGRATION_ANSIBLE_ENABLED=true</span>（详见 deploy/ansible/README.md）。
          </p>
        </el-alert>
        <template v-if="form.deploy_target === 'remote'">
          <!-- 日志集成的目标服务器就是上面的地址栏，这里不再重复一个同义输入框；
               Exporter 端口 / Exporter 安装方式对 Filebeat 也不适用（安装方式在「采集参数」里选） -->
          <el-row v-if="!isLog" :gutter="12">
            <el-col :xs="24" :sm="10">
              <el-form-item label="目标服务器">
                <el-input v-model="form.target_host" placeholder="如 10.0.0.31（被管实例所在机器）" />
              </el-form-item>
            </el-col>
            <el-col :xs="24" :sm="7">
              <el-form-item label="Exporter 端口">
                <el-input-number v-model="form.exporter_port" :min="1" :max="65535" class="mobile-block"
                  :placeholder="`模板默认 ${activeTemplate?.exporter_port || ''}`" />
              </el-form-item>
            </el-col>
            <el-col :xs="24" :sm="7">
              <el-form-item label="安装方式">
                <el-select v-model="form.install_mode" class="mobile-block">
                  <el-option label="docker（官方镜像跑容器，需目标机有 Docker）" value="docker" />
                  <el-option label="docker-systemd（容器交给 systemd 托管）" value="docker-systemd" />
                  <el-option label="binary（下载官方二进制 + 原生 systemd，无需 Docker）" value="binary" />
                </el-select>
              </el-form-item>
            </el-col>
          </el-row>
          <el-row :gutter="12">
            <el-col :xs="24" :sm="8">
              <el-form-item label="SSH 用户">
                <el-input v-model="form.ssh_user" placeholder="如 ops" />
              </el-form-item>
            </el-col>
            <el-col :xs="24" :sm="4">
              <el-form-item label="SSH 端口">
                <el-input-number v-model="form.ssh_port" :min="1" :max="65535" class="mobile-block" />
              </el-form-item>
            </el-col>
            <el-col :xs="24" :sm="12">
              <el-form-item label="SSH 认证方式">
                <el-radio-group v-model="form.ssh_method">
                  <el-radio value="password">口令</el-radio>
                  <el-radio value="key">私钥</el-radio>
                </el-radio-group>
              </el-form-item>
            </el-col>
          </el-row>
          <el-form-item v-if="form.ssh_method === 'password'" label="SSH 口令（仅本次使用）">
            <el-input v-model="form.ssh_password" type="password" show-password
              placeholder="不落库、不写审计、不回显" />
          </el-form-item>
          <el-form-item v-else label="SSH 私钥（仅本次使用）">
            <el-input v-model="form.ssh_key" type="textarea" :rows="3"
              placeholder="粘贴私钥全文（-----BEGIN OPENSSH PRIVATE KEY----- …）：写入 0600 临时文件、执行后立刻删除" />
            <p class="field-hint">
              私钥认证不经过 sshpass（口令方式才要求平台镜像装 sshpass）；
              私钥方式下 sudo 需要密码时 ansible 会报 Missing sudo password，请改用口令方式或在目标机为登录用户配置 sudo 免密。
            </p>
          </el-form-item>
          <div class="switch-row">
            <el-switch v-model="form.ssh_become" />
            <span>远程安装使用 sudo（装到 /opt、写 systemd 单元时需要）</span>
          </div>
          <el-alert type="warning" :closable="false" show-icon class="mb"
            title="远程安装会在目标服务器上执行命令">
            <p class="field-hint">
              平台只渲染「内置」playbook（不接受自定义脚本），SSH 凭据只在本次请求内存中使用、
              写入 0600 临时文件并在执行后立即删除，因此「重新应用」需要重新填写凭据；
              生产环境会先创建审批工单，审批通过后才执行。
            </p>
            <p v-if="isLog" class="field-hint">
              目标机需有 sudo 权限；平台会先探测是否已有 Filebeat（在位则复用，只下发/校验配置），
              安装后由自检回答「Filebeat 是否在位」与「目标机能否连上平台 Kafka」。
              该能力同样需要平台镜像带 ansible-playbook 且已开启 INTEGRATION_ALLOW_REMOTE_INSTALL。
            </p>
            <p v-else class="field-hint">
              目标机需有 sudo 权限；选 docker/docker-systemd 时还需已安装 Docker（选 binary 则不需要）；
              安装完成后平台会主动探测一次「目标IP:端口」，探不通会把原因写进集成备注。
              该能力还需平台镜像带 ansible-playbook 且已开启 INTEGRATION_ALLOW_REMOTE_INSTALL。
            </p>
          </el-alert>
        </template>

        <el-divider content-position="left">落地方式</el-divider>
        <div class="switch-row">
          <!-- 开关本身永远可选：
               能力没就绪时只做提示，不阻止使用者勾选——
               因为"平台不部署"本身也是合法选择（手工模式），
               而灰掉开关只会让人不知道发生了什么（本次反馈的问题）。 -->
          <el-switch v-model="form.deploy" />
          <span v-if="isLog">
            由平台用 Ansible 在目标机安装 Filebeat（{{ remoteReady ? 'Ansible 通道可用' : '需平台镜像带 ansible-playbook' }}）
          </span>
          <span v-else-if="form.deploy_target === 'remote'">
            由平台远程安装 Exporter（{{ remoteReady ? 'Ansible 通道可用' : '需平台镜像带 ansible-playbook' }}）
          </span>
          <span v-else>
            由平台一键拉起 Exporter 容器（{{ dockerReady ? 'Docker API 可用' : '需开启 integration.docker_enabled' }}）
          </span>
        </div>
        <!-- 推荐告警规则基于 Prometheus 指标，日志集成的告警来自日志指纹，不在此处开关 -->
        <div v-if="!isLog" class="switch-row">
          <el-switch v-model="form.auto_rules" />
          <span>自动创建推荐告警规则（{{ (activeTemplate?.alerts || []).length }} 条）</span>
        </div>
        <p v-if="form.deploy_target === 'remote' && !remoteReady" class="field-hint">
          当前平台镜像没有 ansible-playbook：本地开发可先用「本机」部署位置，
          生产请用 <span class="mono">WITH_ANSIBLE=true</span> 构建后端镜像（见 deploy/ansible/README.md）。
        </p>

        <!-- 反向接网：只对本机容器有意义（远程目标是别的机器上的容器，平台碰不到它的网络）；
             日志集成不接管任何容器，也不存在接网这件事 -->
        <template v-if="form.deploy_target === 'local' && !isLog">
          <div class="switch-row">
            <el-switch v-model="form.join_platform_network" :disabled="!dockerReady" />
            <span>改为把「目标容器」接入平台网络（仅在平台接不进去时使用）</span>
          </div>
          <el-alert v-if="form.join_platform_network" type="warning" :closable="false" show-icon class="mt"
            title="这是反向接网：会修改被管容器的网络配置">
            <p class="field-hint">
              平台将执行等价于 <code>docker network connect &lt;平台网络&gt; &lt;目标容器&gt;</code> 的操作。
              默认方向本来就是「平台自动接进目标网络」，正常情况下不需要勾选它。
            </p>
            <p class="field-hint">
              务必注意：若目标容器原本只在 internal 网络里（例如 被管项目的 internal 网络，刻意做成无出网），
              接入平台网络后它会多一条出网路径，数据面隔离随之失效。
              地址填的是外部地址（非容器名）时无需勾选。
          </p>
        </el-alert>
        </template>

        <!-- 账号托管：平台代为创建只读监控账号（写操作，需显式授权） -->
        <template v-if="bootstrapSupported">
          <el-divider content-position="left">监控账号</el-divider>
          <el-alert type="info" :closable="false" show-icon class="mb"
            title="两个「账号」不是一回事">
            <p class="field-hint">
              ① <b>只读监控账号</b>（上面的「只读监控账号」栏）：<b>采集用</b>，长期存在，
              权限最小（MySQL 只授 PROCESS / REPLICATION CLIENT / SELECT），被填进 Exporter。
            </p>
            <p class="field-hint">
              ② <b>管理员账号</b>（下面的「管理员账号」栏）：<b>建号用</b>，只在这次请求里用一次，
              用来执行建号 SQL（CREATE USER / GRANT），<b>不落库、不写审计、用完即弃</b>。
              它不是监控账号，也不会被存进平台。
            </p>
            <p class="field-hint">
              所以：只想手工建好账号再用 → 只填①；想省事让平台建 → ①留空（用默认名）+ 填②。
            </p>
          </el-alert>
          <div class="switch-row">
            <el-switch v-model="form.bootstrap_account"
              :disabled="form.deploy_target === 'remote' ? !remoteReady : !dockerReady" />
            <span>由平台创建/更新只读监控账号（无需登录被管数据库手工建号）</span>
          </div>
          <p v-if="form.bootstrap_account" class="field-hint">
            账号名留空即用默认值 <span class="mono">{{ activeTemplate?.monitor_user || 'mwops_exporter' }}</span>，
            口令由平台生成十六进制随机串并加密存储；你只需要填一次管理员凭据。
            未填凭据时不会报错，只会在集成备注里提示「填凭据后点重新应用」。
          </p>
          <el-row v-if="form.bootstrap_account" :gutter="12">
            <el-col :xs="24" :sm="12">
              <el-form-item label="管理员账号（仅本次建号使用）">
                <el-input v-model="form.admin_username" placeholder="如 root（不落库、不回显）" />
              </el-form-item>
            </el-col>
            <el-col :xs="24" :sm="12">
              <el-form-item label="管理员口令（仅本次建号使用）">
                <el-input v-model="form.admin_password" type="password" show-password placeholder="不落库、不写审计、不回显" />
              </el-form-item>
            </el-col>
          </el-row>
          <el-alert v-if="form.bootstrap_account" type="warning" :closable="false" show-icon class="mt"
            title="平台将在被管实例上执行以下固定 SQL（不接受任意语句）">
            <pre class="code">{{ bootstrapSQLPreview }}</pre>
            <p class="field-hint">
              口令留空时由平台生成十六进制随机串（无特殊字符，天然免转义）；执行结果会记入审计（不含口令）。
            </p>
          </el-alert>
        </template>

        <el-alert
          v-for="(note, index) in activeTemplate?.notes || []"
          :key="index"
          class="mt"
          type="warning"
          :closable="false"
          show-icon
          :title="note"
        />
      </el-form>

      <template #footer>
        <div class="dialog-footer">
          <!-- 日志集成同样有可预览的产物：渲染后的 filebeat.yml 与安装命令（后端同一接口承载） -->
          <el-button :loading="previewing" @click="handlePreview">
            {{ isLog ? '预览 Filebeat 配置与安装命令' : '预览生成的配置' }}
          </el-button>
          <div class="spacer" />
          <el-button @click="dialogVisible = false">取消</el-button>
          <el-button type="primary" :loading="submitting" @click="handleSubmit">
            {{ submitLabel }}
          </el-button>
        </div>
      </template>
    </el-dialog>

    <!-- 生成的配置 -->
    <el-drawer
      v-model="artifactsVisible"
      :title="artifactsIsLog ? '将下发的 Filebeat 配置与安装命令' : '生成的采集配置'"
      size="640px"
    >
      <template v-if="artifacts">
        <!-- 日志集成没有 file_sd / scrape_job：产物的语义按 category 换标签，
             否则 filebeat.yml 会挂在「Exporter(compose)」这个标题下 -->
        <el-tabs v-model="artifactsTab">
          <el-tab-pane :label="artifactsIsLog ? 'Filebeat(filebeat.yml)' : 'Exporter(compose)'" name="compose">
            <p v-if="artifactsIsLog" class="muted">
              平台会把它下发到目标机的 <span class="mono">/etc/filebeat/filebeat.yml</span>；
              内容未变化时不重启 Filebeat，避免采集抖动。
            </p>
            <el-button text size="small" @click="copy(artifacts.compose)">复制</el-button>
            <pre class="code">{{ artifacts.compose }}</pre>
          </el-tab-pane>
          <el-tab-pane v-if="!artifactsIsLog" label="Prometheus(file_sd)" name="filesd">
            <p class="muted">平台会写入 <span class="mono">{{ overview?.file_sd_path }}</span>，Prometheus 周期性重读，无需重启。</p>
            <el-button text size="small" @click="copy(artifacts.file_sd)">复制</el-button>
            <pre class="code">{{ artifacts.file_sd }}</pre>
          </el-tab-pane>
          <el-tab-pane v-if="!artifactsIsLog" label="Prometheus(显式 job)" name="job">
            <p class="muted">file_sd 不便接入时，把下面片段并入 <span class="mono">scrape_configs</span>（两者二选一）。</p>
            <el-button text size="small" @click="copy(artifacts.scrape_job)">复制</el-button>
            <pre class="code">{{ artifacts.scrape_job }}</pre>
          </el-tab-pane>
          <el-tab-pane :label="artifactsIsLog ? '安装命令' : 'docker run'" name="run">
            <el-button text size="small" @click="copy(artifacts.deploy_cmd)">复制</el-button>
            <pre class="code">{{ artifacts.deploy_cmd }}</pre>
          </el-tab-pane>
          <el-tab-pane label="核对步骤" name="verify">
            <ol class="steps">
              <li v-for="(step, index) in artifacts.verify_steps" :key="index">{{ step }}</li>
            </ol>
            <p v-if="!artifactsIsLog" class="muted">
              平台查询选择器：<span class="mono">{{ artifacts.selector }}</span>
            </p>
            <p v-if="artifacts.network_note" class="muted">{{ artifacts.network_note }}</p>
          </el-tab-pane>
        </el-tabs>
      </template>
    </el-drawer>
    <!-- 监控账号管理：平台代管的只读账号 -->
    <el-dialog v-model="accountsVisible" title="监控账号管理" width="820px">
      <el-alert
        type="info"
        :closable="false"
        show-icon
        class="mb"
        title="这些只读账号由平台创建并托管（口令加密存储）。建号/连接失败时可直接在这里重试；轮换口令不需要管理员凭据；删除账号是破坏性操作，需要管理员凭据（生产环境会转成审批工单）。"
      />
      <!-- 凭据不再散落在抽屉里：每个动作点开自己的弹窗，在弹窗内填齐再触发（真实反馈：旧流程
           先弹两个输入框、最后才检查这里的 SSH 字段，没填就中止，等于什么也没做）。 -->
      <el-alert
        v-if="hasRemoteAccount"
        type="info"
        :closable="false"
        show-icon
        class="mb"
        title="有远程部署的集成：点任一操作后，在弹窗里填写 SSH 凭据（口令或私钥，仅本次使用、不落库）"
      />
      <el-alert
        v-if="accountResult"
        :type="accountResult.ok ? 'success' : 'error'"
        :closable="true"
        show-icon
        class="mb"
        :title="accountResult.title"
        @close="accountResult = null"
      >
        <p class="field-hint" style="white-space: pre-wrap">{{ accountResult.detail }}</p>
      </el-alert>
      <el-table v-loading="accountsLoading" :data="accounts" size="small">
        <el-table-column prop="name" label="集成" min-width="120" show-overflow-tooltip />
        <el-table-column label="组件" width="95">
          <template #default="{ row }">
            <el-tag size="small" effect="plain">{{ row.component || row.mw_type }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="username" label="监控账号" width="130" />
        <el-table-column label="来源" width="105">
          <template #default="{ row }">
            <el-tag v-if="row.managed" size="small" type="success" effect="light">平台创建</el-tag>
            <el-tag v-else-if="row.supports_management" size="small" effect="plain">外部账号</el-tag>
            <span v-else class="muted">不需要</span>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="120">
          <template #default="{ row }">
            <el-tag v-if="row.last_error" size="small" type="danger" effect="light">待处理</el-tag>
            <el-tag v-else size="small" type="success" effect="light">正常</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="最近轮换" width="140">
          <template #default="{ row }">
            <span class="muted">{{ row.rotated_at ? formatTime(row.rotated_at) : '-' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="250" fixed="right">
          <template #default="{ row }">
            <el-button
              text
              size="small"
              :loading="retryingId === row.integration_id"
              :disabled="!row.supports_management || (!dockerReady && row.deploy_target !== 'remote')"
              @click="handleRetryAccount(row)"
            >重试建号</el-button>
            <el-button
              text
              size="small"
              :loading="probingId === row.integration_id"
              :disabled="!row.supports_management || (!dockerReady && row.deploy_target !== 'remote')"
              @click="handleProbeAccount(row)"
            >测试连接</el-button>
            <el-button
              text
              size="small"
              :disabled="!row.has_password || !row.supports_management || (!dockerReady && row.deploy_target !== 'remote')"
              @click="handleRotateAccount(row)"
            >轮换口令</el-button>
            <el-button
              text
              size="small"
              type="danger"
              :disabled="!row.supports_management || (!dockerReady && row.deploy_target !== 'remote')"
              @click="handleDropAccount(row)"
            >删除</el-button>
          </template>
        </el-table-column>
      </el-table>
      <p v-if="accounts.some((row) => row.last_error)" class="muted note">
        失败原因：{{ accounts.find((row) => row.last_error)?.last_error }}
        —— 修好外部原因（凭据/网络/权限）后点「重试建号」即可，不需要重填整个集成表单。
      </p>
      <p v-if="!dockerReady" class="muted note">
        当前「一键部署」不可用（未挂载 docker.sock 或 integration.docker_enabled=false），
        无法由平台执行建号/重试/轮换/删除；请在平台 .env 打开后重建 backend 容器。
      </p>
    </el-dialog>

    <!-- 账号操作：先在这一处收齐凭据，再由本弹窗的主按钮触发请求（真实反馈：旧流程把输入拆成
         多个系统弹框、最后才发现缺 SSH 凭据而中止，等于什么也没做）。 -->
    <el-dialog
      v-model="accountActionVisible"
      :title="`${accountActionTitle} · ${accountActionRow?.name || ''}`"
      width="640px"
      :close-on-click-modal="false"
    >
      <el-alert
        type="info"
        :closable="false"
        show-icon
        class="mb"
        :title="`${accountActionRow?.component || accountActionRow?.mw_type || ''} · ${accountActionRow?.address || ''}`"
      >
        <p class="field-hint">
          <template v-if="accountActionRow?.deploy_target === 'remote'">
            该集成是<b>远程部署</b>：账号操作由平台 SSH 到目标机执行（只经 SSH + Ansible，不依赖平台 Docker），
            因此需要一次 SSH 凭据。<br />
          </template>
          <template v-if="accountActionNeedsAdmin">
            建号 / 删号是<b>写被管库</b>的操作，需要一次管理员凭据。<br />
          </template>
          所有凭据仅本次使用、<b>不落库、不写审计、不回显</b>；填完点下方按钮才会发请求。
        </p>
      </el-alert>

      <template v-if="accountActionRow?.deploy_target === 'remote'">
        <el-divider content-position="left">SSH 凭据</el-divider>
        <el-row :gutter="12">
          <el-col :xs="24" :sm="10">
            <el-form-item label="SSH 用户">
              <el-input v-model="accountSsh.user" placeholder="如 root" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="6">
            <el-form-item label="SSH 端口">
              <el-input-number v-model="accountSsh.port" :min="1" :max="65535" class="mobile-block" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="8">
            <el-form-item label="认证方式">
              <el-radio-group v-model="accountSsh.method">
                <el-radio value="password">口令</el-radio>
                <el-radio value="key">私钥</el-radio>
              </el-radio-group>
            </el-form-item>
          </el-col>
        </el-row>
        <el-form-item v-if="accountSsh.method === 'password'" label="SSH 口令">
          <el-input v-model="accountSsh.password" type="password" show-password placeholder="不落库、不回显" />
        </el-form-item>
        <el-form-item v-else label="SSH 私钥">
          <el-input v-model="accountSsh.key" type="textarea" :rows="3"
            placeholder="粘贴私钥全文（-----BEGIN OPENSSH PRIVATE KEY----- …）；不需要 sshpass" />
        </el-form-item>
      </template>

      <template v-if="accountActionNeedsAdmin">
        <el-divider content-position="left">管理员凭据（写被管库）</el-divider>
        <el-row :gutter="12">
          <el-col :xs="24" :sm="10">
            <el-form-item label="管理员账号">
              <el-input v-model="accountAdmin.user" placeholder="如 root" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="14">
            <el-form-item :label="accountActionKind === 'retry' ? '管理员口令（留空=只测连接，不建号）' : '管理员口令'">
              <el-input v-model="accountAdmin.password" type="password" show-password placeholder="仅本次使用、不落库" />
            </el-form-item>
          </el-col>
        </el-row>
      </template>

      <el-divider content-position="left">本次将要执行</el-divider>
      <p class="field-hint">
        <template v-if="accountActionKind === 'probe'">
          只读探测：用现有监控账号执行 `SELECT 1`（MySQL 另附 `SHOW GRANTS`），<b>不改任何配置</b>。
        </template>
        <template v-else-if="accountActionKind === 'retry'">
          <template v-if="accountAdmin.password">
            幂等重跑建号 SQL（不存在则建、存在则重置口令并授权），随后测试连接并重建 Exporter。
          </template>
          <template v-else>
            不带管理员口令 → <b>只测试连接并重建 Exporter</b>（不会建号）。
          </template>
        </template>
        <template v-else-if="accountActionKind === 'rotate'">
          账号改自己的口令（`ALTER USER USER()` / `ALTER ROLE CURRENT_USER`），随后用新口令自动重建 Exporter。<b>不需要管理员凭据</b>。
        </template>
        <template v-else>
          删除平台创建的监控账号（`DROP USER/ROLE IF EXISTS`）；生产环境会转成审批工单。
        </template>
      </p>

      <el-alert
        v-if="accountResult"
        :type="accountResult.ok ? 'success' : 'error'"
        :closable="true"
        show-icon
        class="mt"
        :title="accountResult.title"
        @close="accountResult = null"
      >
        <p class="field-hint" style="white-space: pre-wrap">{{ accountResult.detail }}</p>
      </el-alert>

      <template #footer>
        <div class="dialog-footer">
          <div class="spacer" />
          <el-button @click="accountActionVisible = false">取消</el-button>
          <el-button type="primary" :loading="accountActionLoading" @click="submitAccountAction">
            {{ accountActionConfirmLabel }}
          </el-button>
        </div>
      </template>
    </el-dialog>
    <!-- 重新应用：远程部署需要 SSH 凭据（仅本次使用，不落库） -->
    <el-dialog
      v-model="applyVisible"
      :title="`重新应用 ${applyTarget?.name || ''}`"
      width="620px"
      :close-on-click-modal="false"
    >
      <el-alert type="info" :closable="false" show-icon
        :title="applyIsLog
          ? '将用 Ansible 在目标机安装/校验 Filebeat（已安装则跳过）'
          : '将重写抓取配置并在目标机上重建/重装 Exporter'">
        <p class="field-hint">
          平台会{{ applyIsLog ? '重放一次 Filebeat 部署并下发最新配置' : '重写 Prometheus 抓取目标并重新安装 Exporter' }}。
          SSH 凭据仅本次使用、不落库、不回显；用<b>私钥</b>认证时不需要平台安装 sshpass。
        </p>
      </el-alert>
      <el-row :gutter="12" class="mt">
        <el-col :xs="24" :sm="10">
          <el-form-item label="SSH 用户">
            <el-input v-model="applySsh.user" placeholder="如 root" />
          </el-form-item>
        </el-col>
        <el-col :xs="24" :sm="6">
          <el-form-item label="SSH 端口">
            <el-input-number v-model="applySsh.port" :min="1" :max="65535" class="mobile-block" />
          </el-form-item>
        </el-col>
        <el-col :xs="24" :sm="8">
          <el-form-item label="认证方式">
            <el-radio-group v-model="applySsh.method">
              <el-radio value="password">口令</el-radio>
              <el-radio value="key">私钥</el-radio>
            </el-radio-group>
          </el-form-item>
        </el-col>
      </el-row>
      <el-form-item v-if="applySsh.method === 'password'" label="SSH 口令（仅本次使用）">
        <el-input v-model="applySsh.password" type="password" show-password
          placeholder="不落库、不写审计、不回显" />
      </el-form-item>
      <el-form-item v-else label="SSH 私钥（仅本次使用）">
        <el-input v-model="applySsh.key" type="textarea" :rows="4"
          placeholder="粘贴私钥全文（-----BEGIN OPENSSH PRIVATE KEY----- …）" />
      </el-form-item>
      <template #footer>
        <div class="dialog-footer">
          <div class="spacer" />
          <el-button @click="applyVisible = false">取消</el-button>
          <el-button type="primary" :loading="applying" @click="submitApply">开始重新应用</el-button>
        </div>
      </template>
    </el-dialog>
    <!-- 集成自检：分环节给结论，替代"翻日志猜" -->
    <el-dialog
      v-model="selfCheckVisible"
      :title="`集成自检 · ${selfCheckName}`"
      width="720px"
    >
      <el-alert
        v-loading="selfCheckLoading"
        v-if="selfCheckResult"
        :type="selfCheckResult.ok ? 'success' : 'error'"
        :closable="false"
        show-icon
        class="mb"
        :title="selfCheckResult.summary"
      />
      <div v-else-if="selfCheckLoading" class="muted">
        正在按环节检查（{{ selfCheckIsLog ? '平台 Kafka → 目标机 Filebeat → 目标机到 Kafka 的连通性 → 是否收到日志' : '平台端口 → Exporter → Prometheus → 业务指标' }}）…
      </div>

      <el-table v-if="selfCheckResult" :data="selfCheckResult.stages" size="small" :show-header="false">
        <el-table-column label="环节" width="210">
          <template #default="{ row }">
            <el-tag size="small" :type="selfCheckTagType(row.status)" effect="light">{{ selfCheckTagText(row.status) }}</el-tag>
            <span class="ml">{{ row.title }}</span>
          </template>
        </el-table-column>
        <el-table-column label="依据与动作">
          <template #default="{ row }">
            <div>{{ row.detail }}</div>
            <div v-if="row.advice" class="field-hint">→ {{ row.advice }}</div>
          </template>
        </el-table-column>
      </el-table>

      <template #footer>
        <div class="dialog-footer">
          <div class="spacer" />
          <el-button @click="selfCheckVisible = false">关闭</el-button>
          <el-button
            v-if="selfCheckResult && !selfCheckResult.ok && selfCheckResult.next_action"
            type="primary"
            @click="followSelfCheckAction"
          >{{ selfCheckResult.next_action_label || '按建议处理' }}</el-button>
        </div>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.mb {
  margin-bottom: 12px;
}

.card + .card {
  margin-top: 12px;
}

.tpl-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
  gap: 12px;
}

/* 同一分类的卡片成组展示：类型不同，表单与状态语义完全不同，混在一起容易选错 */
.tpl-group + .tpl-group {
  margin-top: 14px;
}

.tpl-group-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 8px;
  font-size: 11.5px;
}

.tpl-card {
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  padding: 12px;
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.tpl-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.tpl-name {
  font-weight: 600;
  font-size: 14px;
}

.tpl-component {
  font-size: 11.5px;
  color: var(--c-text-muted);
}

.tpl-desc {
  font-size: 12px;
  color: var(--c-text-muted);
  line-height: 1.6;
  min-height: 38px;
}

.tpl-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.field-hint {
  margin: 2px 0 0;
  font-size: 11.5px;
  color: var(--c-text-muted);
  line-height: 1.6;
}

.advanced-collapse {
  margin-top: 12px;
  border-top: 1px solid var(--el-border-color-lighter);
  border-bottom: none;
}

.collapse-title {
  font-size: 12.5px;
  color: var(--el-text-color-regular);
}

.note {
  margin: 8px 0 0;
  font-size: 11.5px;
}

/* 待处理提示 + 直达重试入口 */
.pending-row {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  margin-top: 8px;
  font-size: 11.5px;
}

.kv-row {
  display: grid;
  grid-template-columns: 1fr 1fr auto;
  gap: 8px;
  margin-bottom: 8px;
}

.option-row {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  padding: 8px 0;
  border-bottom: 1px dashed var(--c-border);
}

.option-main {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.option-label {
  font-size: 13px;
  font-weight: 500;
}

.option-input {
  width: 200px;
}

/* 日志路径可能是多个 glob（每行一个或逗号分隔），输入框必须放得下多行 */
.option-textarea {
  width: 320px;
  flex: 0 0 320px;
}

.switch-row {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 12.5px;
  margin-bottom: 8px;
}

.dialog-footer {
  display: flex;
  align-items: center;
  gap: 8px;
}

.spacer {
  flex: 1;
}

.code {
  margin: 8px 0 0;
  padding: 10px;
  background: var(--c-surface-2, #f6f7f9);
  border-radius: var(--r-sm, 6px);
  font-size: 11.5px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 460px;
  overflow: auto;
}

.steps {
  margin: 0;
  padding-left: 18px;
  font-size: 12.5px;
  line-height: 1.9;
}

/* 窄屏：参数行改为上下排列，否则"说明 + 控件"会被挤在一起 */
@media (max-width: 767px) {
  .option-row {
    flex-direction: column;
    align-items: stretch;
  }

  .option-input,
  .option-textarea {
    width: 100%;
    flex: none;
  }
}
</style>
