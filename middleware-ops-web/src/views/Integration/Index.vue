<script setup lang="ts">
/**
 * 集成中心。
 *
 * 对齐云厂商 Prometheus 控制台的「数据采集 → 集成中心」：
 * 在页面上选组件（Redis / MySQL / PostgreSQL / Kafka / Elasticsearch / Nginx），
 * 填写名称、地址、账号口令、自定义标签与 Exporter 参数，保存即完成
 * 「Exporter 暴露 → Prometheus 抓取 → 实例纳管 → 推荐告警规则」。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { integrationApi } from '@/api'
import { toastError } from '@/api/http'
import type { IntegrationAccount, IntegrationArtifacts, IntegrationInput, IntegrationTemplate, IntegrationView, LogCollectInput, LogCollectPlan } from '@/api/types'
import { envLabels, formatTime } from '@/utils/format'

const router = useRouter()

const loading = ref(false)
const overview = ref<{ total: number; by_type: Record<string, number>; templates: IntegrationTemplate[]; file_sd_path: string; docker_note: string; docker_ok: boolean } | null>(null)
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

/** 表单模型：通用字段 + 动态标签 + 动态 Exporter 参数。 */
const form = reactive({
  name: '',
  address: '',
  username: '',
  password: '',
  environment: 'dev',
  group_name: '',
  deploy: false,
  auto_rules: true,
  // 自动建号：需要账号的组件**默认由平台创建**（使用者不必提前建号）
  bootstrap_account: true,
  admin_username: '',
  admin_password: '',
  // 反向接网：把**目标容器**接入平台网络。默认关闭（正常方向是平台自己接进目标网络）
  join_platform_network: false,
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

/** 是否处于编辑态。 */
const isEdit = computed(() => Boolean(editing.value?.instance_id))

/** 一键部署是否可用。 */
const dockerReady = computed(() => Boolean(overview.value?.docker_ok))

/** 已集成总数。 */
const total = computed(() => items.value.length)

// ---------------------------------------------------------------------------
// 监控账号管理（平台代管的只读账号：查看 / 轮换口令 / 删除）
// ---------------------------------------------------------------------------
const accountsVisible = ref(false)
const accountsLoading = ref(false)
const accounts = ref<IntegrationAccount[]>([])
/** 正在重试/探测的集成 ID（用于按钮 loading）。 */
const retryingId = ref<number | null>(null)
const probingId = ref<number | null>(null)
/** 最近一次重试/探测的结论（就地展示失败原因，而不是只弹一条 toast）。 */
const accountResult = ref<{ ok: boolean; title: string; detail: string } | null>(null)

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

/** 测试连接：只探测，不改配置。 */
async function handleProbeAccount(row: IntegrationAccount): Promise<void> {
  probingId.value = row.integration_id
  accountResult.value = null
  try {
    const result = await integrationApi.probeAccount(row.integration_id)
    accountResult.value = {
      ok: result.ok,
      title: result.ok ? `${row.name}：连接正常` : `${row.name}：连接失败`,
      detail: result.ok ? result.output || result.message : result.message,
    }
  } catch (error) {
    toastError(error)
  } finally {
    probingId.value = null
  }
}

/**
 * 重试建号/连接：失败后不必重填整个表单。
 *
 * 带管理凭据 → 平台幂等重跑建号 SQL（不存在则建、存在则重置口令并授权）；
 * 不带 → 只测试连接并重建 Exporter。
 */
async function handleRetryAccount(row: IntegrationAccount): Promise<void> {
  const retryWithCreds = await ElMessageBox.confirm(
    `重试 ${row.name} 的监控账号 ${row.username}。\n\n` +
      `选「由平台重建」会幂等重跑建号 SQL（需要一次管理员凭据，口令不落库）；\n` +
      `选「只测连接」则只测试现有账号并重建 Exporter。`,
    '重试建号 / 连接',
    { confirmButtonText: '由平台重建', cancelButtonText: '只测连接', distinguishCancelAndClose: true },
  ).then(() => true).catch((action) => (action === 'cancel' ? false : null))
  if (retryWithCreds === null) {
    return
  }

  let adminUser = ''
  let adminPassword = ''
  if (retryWithCreds) {
    const user = await ElMessageBox.prompt('管理员账号（仅本次使用）：', '重试建号', {
      inputPlaceholder: '如 root', inputValue: 'root', confirmButtonText: '下一步', cancelButtonText: '取消',
    }).catch(() => ({ value: '' }))
    if (!user.value) {
      return
    }
    const pass = await ElMessageBox.prompt('管理员口令（仅本次使用，不落库、不写审计）：', '重试建号', {
      inputType: 'password', confirmButtonText: '开始重试', cancelButtonText: '取消',
    }).catch(() => ({ value: '' }))
    if (!pass.value) {
      return
    }
    adminUser = user.value
    adminPassword = pass.value
  }

  retryingId.value = row.integration_id
  accountResult.value = null
  try {
    const result = await integrationApi.retryAccount(row.integration_id, {
      admin_username: adminUser,
      admin_password: adminPassword,
    })
    accountResult.value = {
      ok: result.ok,
      title: result.ok ? `${row.name}：账号已就绪` : `${row.name}：仍未就绪`,
      detail: result.message,
    }
    await Promise.all([loadAccounts(), load()])
  } catch (error) {
    toastError(error)
  } finally {
    retryingId.value = null
  }
}

/** 轮换口令：账号改自己的口令，不需要管理员凭据。 */
async function handleRotateAccount(row: IntegrationAccount): Promise<void> {
  const confirmed = await ElMessageBox.confirm(
    `将为 ${row.name} 的监控账号 ${row.username} 生成新口令，并立即用新口令重建 Exporter。` +
      `旧口令作废（业务侧无需改动）。是否继续？`,
    '轮换监控账号口令',
    { type: 'warning', confirmButtonText: '轮换', cancelButtonText: '取消' },
  ).catch(() => false)
  if (!confirmed) {
    return
  }
  try {
    await integrationApi.rotateAccount(row.integration_id)
    ElMessage.success('已轮换口令并重建 Exporter')
    await Promise.all([loadAccounts(), load()])
  } catch (error) {
    toastError(error)
  }
}

/** 删除账号：破坏性写操作，需要管理员凭据。 */
async function handleDropAccount(row: IntegrationAccount): Promise<void> {
  const { value: adminUser } = await ElMessageBox.prompt(
    `删除 ${row.name} 上的监控账号 ${row.username} 后，该实例将不再有指标。\n请填写管理员账号：`,
    '删除监控账号',
    { inputPlaceholder: '如 root', inputValue: 'root', confirmButtonText: '下一步', cancelButtonText: '取消' },
  ).catch(() => ({ value: '' }))
  if (!adminUser) {
    return
  }
  const { value: adminPassword } = await ElMessageBox.prompt(
    '请填写管理员口令（仅本次使用，不落库、不写审计）：',
    '删除监控账号',
    { inputType: 'password', confirmButtonText: '删除', cancelButtonText: '取消' },
  ).catch(() => ({ value: '' }))
  if (!adminPassword) {
    return
  }
  try {
    await integrationApi.dropAccount(row.integration_id, {
      admin_username: adminUser,
      admin_password: adminPassword,
    })
    ElMessage.success('已删除监控账号')
    await Promise.all([loadAccounts(), load()])
  } catch (error) {
    toastError(error)
  }
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
  form.deploy = dockerReady.value
  form.auto_rules = true
  form.bootstrap_account = bootstrapSupported.value
  form.admin_username = ''
  form.admin_password = ''
  form.join_platform_network = item?.join_platform_network ?? false
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
  submitting.value = true
  try {
    const payload = buildPayload()
    const saved = editing.value
      ? await integrationApi.update(editing.value.instance_id, payload)
      : await integrationApi.create(payload)
    if (saved.last_error) {
      ElMessage({ type: 'warning', message: `已保存，但存在待处理项：${saved.last_error}` })
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

/** 重新应用（重写 file_sd + 重建 Exporter 容器）。 */
async function handleApply(item: IntegrationView): Promise<void> {
  try {
    const saved = await integrationApi.apply(item.instance_id)
    if (saved.last_error) {
      ElMessage({ type: 'warning', message: saved.last_error })
    } else {
      ElMessage({ type: 'success', message: '已重新应用：抓取目标与 Exporter 容器均已刷新' })
    }
    await load()
  } catch (error) {
    toastError(error)
  }
}

/** 按类型取模板。 */
function templateOf(mwType: string): IntegrationTemplate | null {
  return overview.value?.templates.find((candidate) => candidate.type === mwType) || null
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
    artifactsVisible.value = true
  } catch (error) {
    toastError(error)
  }
}

/** 删除集成。 */
async function handleDelete(item: IntegrationView): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `删除集成「${item.name}」会同时移除其抓取目标与 Exporter 容器，历史告警与诊断记录保留。是否继续？`,
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

// ---------------------------------------------------------------------------
// 日志接入：平台读被管容器的 docker 配置反查日志位置，自建采集容器（被管项目零改动）
// ---------------------------------------------------------------------------
const logDialogVisible = ref(false)
const logLoading = ref(false)
const logPlan = ref<LogCollectPlan | null>(null)
const logForm = reactive<LogCollectInput>({
  name: '',
  target_container: '',
  service: '',
  environment: 'dev',
  level_filter: 'ERROR',
})

/** 打开日志接入弹窗。 */
function openLogDialog(): void {
  logForm.name = ''
  logForm.target_container = ''
  logForm.service = ''
  logForm.environment = 'dev'
  logForm.level_filter = 'ERROR'
  logPlan.value = null
  logDialogVisible.value = true
}

/** 预览：读取被管容器配置并反查日志位置（读不到会直接报错）。 */
async function handleLogPreview(): Promise<void> {
  logLoading.value = true
  try {
    logPlan.value = await integrationApi.previewLog(buildLogPayload())
  } catch (error) {
    logPlan.value = null
    toastError(error)
  } finally {
    logLoading.value = false
  }
}

/** 保存并创建采集容器。 */
async function handleLogSubmit(): Promise<void> {
  logLoading.value = true
  try {
    logPlan.value = await integrationApi.createLog(buildLogPayload())
    ElMessage({ type: 'success', message: '日志采集已创建（被管项目无需任何改动）' })
    await load()
  } catch (error) {
    toastError(error)
  } finally {
    logLoading.value = false
  }
}

/** 组装日志接入载荷（服务名缺省取接入名）。 */
function buildLogPayload(): LogCollectInput {
  return {
    ...logForm,
    name: logForm.name.trim(),
    target_container: logForm.target_container.trim(),
    service: (logForm.service || logForm.name).trim(),
  }
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
          选组件 → 填地址与账号 → 保存即完成指标暴露与接入。<span class="mono">{{ overview?.file_sd_path }}</span>
        </p>
      </div>
      <div class="row">
        <el-button size="small" @click="load">刷新</el-button>
        <el-button size="small" :disabled="!dockerReady" @click="openLogDialog">日志接入</el-button>
        <el-button size="small" @click="openAccounts">监控账号</el-button>
        <el-tag size="small" :type="dockerReady ? 'success' : 'info'" effect="light">
          {{ dockerReady ? '一键部署已启用' : '仅生成配置' }}
        </el-tag>
      </div>
    </div>

    <el-alert
      v-if="overview?.docker_note"
      type="info"
      :closable="false"
      show-icon
      :title="overview.docker_note"
      class="mb"
    />

    <!-- 组件模板 -->
    <div class="card">
      <h3 class="card-title">可集成组件</h3>
      <div class="tpl-grid">
        <div v-for="tpl in overview?.templates || []" :key="tpl.type" class="tpl-card">
          <div class="tpl-head">
            <span class="tpl-name">{{ tpl.name }}</span>
            <el-tag v-if="tpl.integrated" size="small" effect="light" type="success">已集成 {{ tpl.integrated }}</el-tag>
            <el-tag v-else-if="tpl.phase === 2" size="small" effect="plain">仅纳管</el-tag>
          </div>
          <p class="tpl-component mono">{{ tpl.component }}</p>
          <p class="tpl-desc">{{ tpl.description }}</p>
          <div class="tpl-foot">
            <span class="muted mono">:{{ tpl.exporter_port }}</span>
            <el-button type="primary" size="small" @click="openInstall(tpl)">集成</el-button>
          </div>
        </div>
      </div>
    </div>

    <!-- 已集成 -->
    <div class="card">
      <h3 class="card-title">已集成实例</h3>
      <div v-if="items.length === 0">
        <el-empty description="还没有集成任何组件，从上面的组件卡片开始" :image-size="72" />
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
          <el-table-column label="Exporter" width="120">
            <template #default="{ row }">
              <el-tooltip v-if="row.deploy_note" :content="row.deploy_note" placement="top">
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
          <el-table-column label="状态" min-width="150">
            <template #default="{ row }">
              <el-tag v-if="row.last_error" size="small" type="danger" effect="light">待处理</el-tag>
              <el-tag v-else-if="row.applied_at" size="small" type="success" effect="light">已应用</el-tag>
              <el-tag v-else size="small" effect="plain">待应用</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="最近应用" width="150">
            <template #default="{ row }">
              <span class="muted">{{ row.applied_at ? formatTime(row.applied_at) : '-' }}</span>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="270" fixed="right">
            <template #default="{ row }">
              <el-button text size="small" @click="goDetail(row)">监控/自检</el-button>
              <el-button text size="small" @click="handleShowArtifacts(row)">配置</el-button>
              <el-button text size="small" @click="openEdit(row)">编辑</el-button>
              <el-button text size="small" @click="handleApply(row)">重新应用</el-button>
              <el-button text size="small" type="danger" @click="handleDelete(row)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
      </div>
      <div v-if="items.some((item) => item.last_error)" class="pending-row">
        <span class="muted">待处理项：{{ items.find((item) => item.last_error)?.last_error }}</span>
        <el-button text size="small" type="primary" @click="openAccounts">去重试 / 测试连接</el-button>
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
              <el-input v-model="form.name" placeholder="如 jd-redis" />
            </el-form-item>
            <p class="field-hint">
              唯一，且必须与 Prometheus 的 <span class="mono">instance_name</span> 一致（平台按它定位指标）。
            </p>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item :label="activeTemplate?.address_label || '连接地址'" prop="address">
              <el-input v-model="form.address" :placeholder="activeTemplate?.address_hint || ''" />
            </el-form-item>
            <p class="field-hint">端口可省略，默认 {{ activeTemplate?.default_port }}。</p>
          </el-col>
          <template v-if="activeTemplate?.needs_auth">
            <el-col :xs="24" :sm="12">
              <el-form-item label="用户名（只读监控账号）">
                <el-input v-model="form.username" placeholder="如 exporter / monitor" />
              </el-form-item>
            </el-col>
            <el-col :xs="24" :sm="12">
              <el-form-item :label="isEdit ? '密码（留空表示不修改）' : '密码'">
                <el-input v-model="form.password" type="password" show-password placeholder="AES-256 加密存储" />
              </el-form-item>
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

        <el-divider content-position="left">自定义标签（写入指标 label）</el-divider>
        <div v-for="(row, index) in form.labels" :key="index" class="kv-row">
          <el-input v-model="row.key" placeholder="标签名，如 team" />
          <el-input v-model="row.value" placeholder="标签值，如 interview" />
          <el-button text type="danger" @click="removeLabel(index)">删除</el-button>
        </div>
        <el-button text size="small" @click="addLabel">+ 添加标签</el-button>

        <template v-if="(activeTemplate?.options || []).length > 0">
          <el-divider content-position="left">Exporter 参数</el-divider>
          <div v-for="option in activeTemplate?.options || []" :key="option.key" class="option-row">
            <div class="option-main">
              <span class="option-label">{{ option.label }}</span>
              <el-tag size="small" effect="plain" type="info">
                {{ option.target === 'env' ? '环境变量' : '命令行' }}
              </el-tag>
              <p class="field-hint">{{ option.help }}</p>
            </div>
            <el-switch
              v-if="option.kind === 'bool'"
              v-model="form.options[option.key]"
              active-value="true"
              inactive-value="false"
            />
            <el-input v-else v-model="form.options[option.key]" class="option-input" :placeholder="option.default" />
          </div>
        </template>

        <el-divider content-position="left">落地方式</el-divider>
        <div class="switch-row">
          <el-switch v-model="form.deploy" :disabled="!dockerReady" />
          <span>由平台一键拉起 Exporter 容器（{{ dockerReady ? 'Docker API 可用' : '需开启 integration.docker_enabled' }}）</span>
        </div>
        <div class="switch-row">
          <el-switch v-model="form.auto_rules" />
          <span>自动创建推荐告警规则（{{ (activeTemplate?.alerts || []).length }} 条）</span>
        </div>

        <!-- 网络接入方向：默认由平台把自己的 Exporter 接进目标网络（不动被管项目） -->
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
            务必注意：若目标容器原本只在 internal 网络里（例如 jd 的 jd-data，刻意做成无出网），
            接入平台网络后它会多一条出网路径，数据面隔离随之失效。
            地址填的是外部地址（非容器名）时无需勾选。
          </p>
        </el-alert>

        <!-- 账号托管：平台代为创建只读监控账号（写操作，需显式授权） -->
        <template v-if="bootstrapSupported">
          <el-divider content-position="left">监控账号</el-divider>
          <div class="switch-row">
            <el-switch v-model="form.bootstrap_account" :disabled="!dockerReady" />
            <span>由平台创建/更新只读监控账号（无需登录被管数据库手工建号）</span>
          </div>
          <p v-if="form.bootstrap_account" class="field-hint">
            账号名留空即用默认值 <span class="mono">{{ activeTemplate?.monitor_user || 'mwops_exporter' }}</span>，
            口令由平台生成十六进制随机串并加密存储；你只需要填一次管理员凭据。
            未填凭据时不会报错，只会在集成备注里提示「填凭据后点重新应用」。
          </p>
          <el-row v-if="form.bootstrap_account" :gutter="12">
            <el-col :xs="24" :sm="12">
              <el-form-item label="管理账号（仅本次使用）">
                <el-input v-model="form.admin_username" placeholder="如 root" />
              </el-form-item>
            </el-col>
            <el-col :xs="24" :sm="12">
              <el-form-item label="管理口令（仅本次使用）">
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
          <el-button :loading="previewing" @click="handlePreview">预览生成的配置</el-button>
          <div class="spacer" />
          <el-button @click="dialogVisible = false">取消</el-button>
          <el-button type="primary" :loading="submitting" @click="handleSubmit">
            {{ isEdit ? '保存并重新应用' : '保存并集成' }}
          </el-button>
        </div>
      </template>
    </el-dialog>

    <!-- 生成的配置 -->
    <el-drawer v-model="artifactsVisible" title="生成的采集配置" size="640px">
      <template v-if="artifacts">
        <el-tabs v-model="artifactsTab">
          <el-tab-pane label="Exporter(compose)" name="compose">
            <el-button text size="small" @click="copy(artifacts.compose)">复制</el-button>
            <pre class="code">{{ artifacts.compose }}</pre>
          </el-tab-pane>
          <el-tab-pane label="Prometheus(file_sd)" name="filesd">
            <p class="muted">平台会写入 <span class="mono">{{ overview?.file_sd_path }}</span>，Prometheus 周期性重读，无需重启。</p>
            <el-button text size="small" @click="copy(artifacts.file_sd)">复制</el-button>
            <pre class="code">{{ artifacts.file_sd }}</pre>
          </el-tab-pane>
          <el-tab-pane label="Prometheus(显式 job)" name="job">
            <p class="muted">file_sd 不便接入时，把下面片段并入 <span class="mono">scrape_configs</span>（两者二选一）。</p>
            <el-button text size="small" @click="copy(artifacts.scrape_job)">复制</el-button>
            <pre class="code">{{ artifacts.scrape_job }}</pre>
          </el-tab-pane>
          <el-tab-pane label="docker run" name="run">
            <el-button text size="small" @click="copy(artifacts.deploy_cmd)">复制</el-button>
            <pre class="code">{{ artifacts.deploy_cmd }}</pre>
          </el-tab-pane>
          <el-tab-pane label="核对步骤" name="verify">
            <ol class="steps">
              <li v-for="(step, index) in artifacts.verify_steps" :key="index">{{ step }}</li>
            </ol>
            <p class="muted">
              平台查询选择器：<span class="mono">{{ artifacts.selector }}</span>
            </p>
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
              :disabled="!row.supports_management || !dockerReady"
              @click="handleRetryAccount(row)"
            >重试建号</el-button>
            <el-button
              text
              size="small"
              :loading="probingId === row.integration_id"
              :disabled="!row.supports_management || !dockerReady"
              @click="handleProbeAccount(row)"
            >测试连接</el-button>
            <el-button
              text
              size="small"
              :disabled="!row.has_password || !row.supports_management || !dockerReady"
              @click="handleRotateAccount(row)"
            >轮换口令</el-button>
            <el-button
              text
              size="small"
              type="danger"
              :disabled="!row.supports_management || !dockerReady"
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
    <!-- 日志接入：从被管容器的 docker 配置反查日志位置（读不到不允许配置） -->
    <el-dialog v-model="logDialogVisible" title="日志接入" width="720px" :close-on-click-modal="false">
      <el-alert
        type="info"
        :closable="false"
        show-icon
        class="mb"
        title="平台会读取被管容器的 docker 配置（环境变量与挂载点）来确定日志位置，并创建自己的采集容器；读不到位置时会拒绝配置，不会猜路径。被管项目无需任何改动。"
      />
      <el-form label-position="top">
        <el-row :gutter="12">
          <el-col :xs="24" :sm="12">
            <el-form-item label="接入名称">
              <el-input v-model="logForm.name" placeholder="如 jd-backend-logs" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="目标容器名">
              <el-input v-model="logForm.target_container" placeholder="docker ps 里的 NAMES，如 interview-backend" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="服务名（日志事件按它归集）">
              <el-input v-model="logForm.service" placeholder="留空取接入名称" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="最低采集级别">
              <el-select v-model="logForm.level_filter" class="mobile-block">
                <el-option label="ERROR" value="ERROR" />
                <el-option label="WARN" value="WARN" />
                <el-option label="INFO（含 GC 等无级别日志）" value="INFO" />
              </el-select>
            </el-form-item>
          </el-col>
        </el-row>
      </el-form>

      <template v-if="logPlan">
        <el-divider content-position="left">发现结果</el-divider>
        <el-descriptions :column="1" border size="small">
          <el-descriptions-item label="日志目录">
            <span class="mono">{{ logPlan.source.dir }}</span>
          </el-descriptions-item>
          <el-descriptions-item label="承载位置">
            {{ logPlan.source.mount_kind === 'volume' ? '命名卷' : '宿主目录' }}
            <span class="mono">{{ logPlan.source.mount_source }}</span> → 采集容器 <span class="mono">{{ logPlan.source.mount_target }}</span>
          </el-descriptions-item>
          <el-descriptions-item label="判断依据">
            <span v-for="(item, index) in logPlan.source.evidence" :key="index">{{ item }}<br /></span>
          </el-descriptions-item>
          <el-descriptions-item label="采集容器">
            <span class="mono">{{ logPlan.collector_name }}</span>（镜像 {{ logPlan.collector_image }}，Entrypoint=mwops-agent）
          </el-descriptions-item>
        </el-descriptions>
        <el-alert v-for="(item, index) in logPlan.warnings" :key="index" class="mt" type="warning" :closable="false" show-icon :title="item" />
        <h4 class="diag-title">平台将执行</h4>
        <ol class="steps">
          <li v-for="(step, index) in logPlan.steps" :key="index">{{ step }}</li>
        </ol>
      </template>

      <template #footer>
        <div class="dialog-footer">
          <el-button :loading="logLoading" @click="handleLogPreview">读取 docker 配置并预览</el-button>
          <div class="spacer" />
          <el-button @click="logDialogVisible = false">取消</el-button>
          <el-button type="primary" :disabled="!logPlan" :loading="logLoading" @click="handleLogSubmit">
            创建采集容器
          </el-button>
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
</style>
