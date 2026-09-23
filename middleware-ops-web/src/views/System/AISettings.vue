<script setup lang="ts">
/**
 * AI 设置：AI Key（密钥/协议/模型）、额度与 token 消费。
 *
 * 密钥安全约定：接口只返回「是否已配置」与掩码，页面永不回显明文；
 * 输入框留空 = 不修改，点「清除」= 保存后清空（clear_api_key）。
 * 配置统一由平台数据库托管，页面不读取 .env。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { settingApi } from '@/api'
import { toastError } from '@/api/http'
import type { AIProviderTestInput, AISettingsInput, AISettingsView, AITestResult, AIUsageView } from '@/api/types'
import StatCard from '@/components/StatCard.vue'
import { formatTime } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const store = useUserStore()

/** 提供方业务键（与接口字段同名，便于按 key 遍历渲染两组表单）。 */
type ProviderKey = 'third_party' | 'self_hosted'

/** 单个提供方的表单模型。 */
interface ProviderForm {
  enabled: boolean
  kind: string
  base_url: string
  model: string
  max_tokens: number
  price_per_k_token: number
}

const loading = ref(false)
const saving = ref(false)
const testing = ref(false)
/** 设置接口原始返回：用于展示 updated_by / updated_at / providers_active 等只读信息。 */
const view = ref<AISettingsView | null>(null)
const usage = ref<AIUsageView | null>(null)
const testResult = ref<AITestResult | null>(null)

/** 额度与策略表单。 */
const form = reactive({
  strategy: 'third_party',
  daily_token_quota: 0,
  per_user_daily_quota: 0,
})

/**
 * 两组提供方表单（结构相同，避免重复代码）。
 *
 * 必须用 reactive 包起来：模板里是 `v-model="providers[item.key].enabled"` 这种按 key 取值再绑定的写法。
 * 若声明成普通对象，赋值本身会成功（保存时读到的是新值），但**视图不会重渲染**——
 * 表现就是"启用开关点不动、协议下拉选了没反应"（INC-020）。
 */
const providers = reactive<Record<ProviderKey, ProviderForm>>({
  third_party: { enabled: false, kind: 'openai', base_url: '', model: '', max_tokens: 0, price_per_k_token: 0 },
  self_hosted: { enabled: false, kind: 'ollama', base_url: '', model: '', max_tokens: 0, price_per_k_token: 0 },
})

/** 新填的密钥（空 = 不修改，绝不回显后端已有密钥）。 */
const apiKeyDraft: Record<ProviderKey, string> = reactive({ third_party: '', self_hosted: '' })
/** 已请求清空密钥（保存后生效）。 */
const apiKeyClear: Record<ProviderKey, boolean> = reactive({ third_party: false, self_hosted: false })
/** 后端返回的掩码，仅用于 placeholder 提示。 */
const apiKeyMasked: Record<ProviderKey, string> = reactive({ third_party: '', self_hosted: '' })

const providerList: { key: ProviderKey; title: string; hint: string }[] = [
  { key: 'third_party', title: '第三方 API（third_party）', hint: 'OpenAI / Anthropic 等托管服务，按 token 计费' },
  { key: 'self_hosted', title: '自建 / 本地模型（self_hosted）', hint: 'Ollama 等内网推理服务，通常不计费（价格填 0）' },
]

/** 策略候选项。 */
const strategyOptions = [
  { value: 'third_party', label: '仅第三方（third_party）' },
  { value: 'self_hosted', label: '仅自建（self_hosted）' },
  { value: 'hybrid', label: '混合：第三方优先、自建兜底（hybrid）' },
]

/** 协议候选项（与后端 kind 取值一一对应）。 */
const kindOptions = ['openai', 'anthropic', 'ollama', 'mock']

const canWrite = computed(() => store.can('system:config:write'))
const providersActive = computed(() => view.value?.providers_active || [])

/** 每个提供方各自的测试连接状态与结果（与全局「测试连接」互不干扰）。 */
const testingProvider = reactive<Record<ProviderKey, boolean>>({ third_party: false, self_hosted: false })
const testResultProvider = reactive<Record<ProviderKey, AITestResult | null>>({ third_party: null, self_hosted: null })

/** 额度展示：-1 表示不限。 */
const remainingText = computed(() => {
  const value = usage.value?.remaining_today
  if (value === undefined || value === null) {
    return '-'
  }
  return value < 0 ? '不限' : thousand(value)
})

/** 用量进度（0-100）。 */
const usedPercent = computed(() => {
  const ratio = Math.min(Math.max(usage.value?.used_ratio ?? 0, 0), 1)
  return Math.round(ratio * 1000) / 10
})

/** 进度条语义色：接近或超出额度时转为警告/危险。 */
const usageStatus = computed<'success' | 'warning' | 'exception'>(() => {
  if (usedPercent.value >= 100) {
    return 'exception'
  }
  return usedPercent.value >= 80 ? 'warning' : 'success'
})

/** 剩余额度的告警级别（不限时不告警）。 */
const remainingStatus = computed<'neutral' | 'ok' | 'warning' | 'critical'>(() => {
  const value = usage.value?.remaining_today
  if (value === undefined || value === null || value < 0) {
    return 'neutral'
  }
  if (value === 0) {
    return 'critical'
  }
  const quota = usage.value?.daily_quota || 0
  if (quota > 0 && value / quota <= 0.1) {
    return 'critical'
  }
  if (quota > 0 && value / quota <= 0.3) {
    return 'warning'
  }
  return 'ok'
})

/** 趋势表只展示最近 7 天（接口默认返回 30 天，表格不引图表库）。 */
const recentSeries = computed(() => (usage.value?.series || []).slice(-7))
/**
 * 窗口内是否真的有消费：平台只展示真实数据，没有任何调用时（全是 0）不画一条「假」趋势，
 * 而是直接显示「暂无消费数据」，避免把补零的空序列误读成有消耗。
 */
const hasRealUsage = computed(() => recentSeries.value.some((p) => p.tokens > 0 || p.calls > 0))
/** 消费趋势倒排：日期从新到旧，一眼看到最近几天的消耗。 */
const trendData = computed(() => {
  const s = hasRealUsage.value ? recentSeries.value : []
  return [...s].reverse()
})

/** 千分位展示。 */
function thousand(value: number): string {
  return value.toLocaleString('zh-CN')
}

/** 拉取设置。 */
async function load(): Promise<void> {
  loading.value = true
  try {
    const data = await settingApi.ai()
    view.value = data
    form.strategy = data.strategy || 'third_party'
    form.daily_token_quota = data.daily_token_quota ?? 0
    form.per_user_daily_quota = data.per_user_daily_quota ?? 0
    for (const item of providerList) {
      const source = data[item.key]
      Object.assign(providers[item.key], {
        enabled: source?.enabled ?? false,
        kind: source?.kind || (item.key === 'third_party' ? 'openai' : 'ollama'),
        base_url: source?.base_url || '',
        model: source?.model || '',
        max_tokens: source?.max_tokens ?? 0,
        price_per_k_token: source?.price_per_k_token ?? 0,
      })
      apiKeyMasked[item.key] = source?.api_key_masked || ''
      // 每次重新拉取都清空草稿：避免把上一次输入误当成"已提交"。
      apiKeyDraft[item.key] = ''
      apiKeyClear[item.key] = false
    }
    const ca = data.code_analysis
    if (ca) {
      Object.assign(codeAnalysis, {
        enabled: ca.enabled ?? false,
        base_url: ca.base_url || '',
        submit_path: ca.submit_path || '/v1/analyses',
        query_path: ca.query_path || '/v1/analyses/{task_id}',
        callback_url: ca.callback_url || '',
        timeout: ca.timeout || '15s',
        task_timeout: ca.task_timeout || '30m',
        poll_interval: ca.poll_interval || '60s',
        poll_batch: ca.poll_batch ?? 20,
        call_mode: ca.call_mode || 'async',
        sync_timeout: ca.sync_timeout || '5m',
        notify_on_submit: ca.notify_on_submit ?? false,
        protocol: ca.protocol || 'generic',
        auth_header: ca.auth_header || '',
        sync_submit_path: ca.sync_submit_path || '',
        repo_locator_mode: ca.repo_locator_mode || 'service',
        repo_git_url: ca.repo_git_url || '',
        environment: ca.environment || '',
        priority: ca.priority ?? 0,
        auto_verify: ca.auto_verify ?? false,
      })
      analysisKeyMasked.api_key = ca.api_key_masked || ''
      analysisKeyMasked.callback_token = ca.callback_token_masked || ''
    }
    analysisKeyDraft.api_key = ''
    analysisKeyDraft.callback_token = ''
    analysisKeyClear.api_key = false
    analysisKeyClear.callback_token = false
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

/** 拉取用量与额度。 */
async function loadUsage(): Promise<void> {
  try {
    usage.value = await settingApi.aiUsage(30)
  } catch (error) {
    usage.value = null
    toastError(error)
  }
}

/**
 * AI 代码分析（日志告警用的外部服务）表单。
 *
 * 与两个提供方同页管理：地址与密钥只有平台这一个来源，保存后立刻重建客户端生效。
 */
const codeAnalysis = reactive({
  enabled: false,
  base_url: '',
  submit_path: '/v1/analyses',
  query_path: '/v1/analyses/{task_id}',
  callback_url: '',
  timeout: '15s',
  task_timeout: '30m',
  poll_interval: '60s',
  poll_batch: 20,
  call_mode: 'async',
  sync_timeout: '5m',
  notify_on_submit: false,
  protocol: 'generic',
  auth_header: '',
  sync_submit_path: '',
  repo_locator_mode: 'service',
  repo_git_url: '',
  environment: '',
  priority: 0,
  auto_verify: false,
})

/** 是否走「AI 代码分析接口文档 v1」协议：决定下面哪些字段需要暴露。 */
const isOpenAPI = computed(() => codeAnalysis.protocol === 'openapi_v1')

/** AI 代码分析的自测状态与结果（与两个提供方、全局测试各自独立，互不覆盖）。 */
const caTesting = ref(false)
const caTestResult = ref<AITestResult | null>(null)

/** 两把密钥的草稿（明文永不回显，只用于"这次要不要改"）。 */
const analysisKeyDraft = reactive({ api_key: '', callback_token: '' })
/** 清除标记：点了「清除」才会在保存时下发 clear_*。 */
const analysisKeyClear = reactive({ api_key: false, callback_token: false })
/** 后端返回的掩码，仅用于 placeholder。 */
const analysisKeyMasked = reactive({ api_key: '', callback_token: '' })

/** 填入新密钥即撤销「清除」标记（新值优先）。 */
function onAnalysisKeyInput(field: 'api_key' | 'callback_token'): void {
  if (analysisKeyDraft[field]) {
    analysisKeyClear[field] = false
  }
}

/** 切换清除标记。 */
function toggleAnalysisClear(field: 'api_key' | 'callback_token'): void {
  analysisKeyClear[field] = !analysisKeyClear[field]
  if (analysisKeyClear[field]) {
    analysisKeyDraft[field] = ''
  }
}

/** 组装 AI 代码分析入参：空密钥不下发，避免把占位符写进后端。 */
function buildCodeAnalysis(): AISettingsInput['code_analysis'] {
  const payload: AISettingsInput['code_analysis'] = {
    enabled: codeAnalysis.enabled,
    base_url: codeAnalysis.base_url,
    submit_path: codeAnalysis.submit_path,
    query_path: codeAnalysis.query_path,
    callback_url: codeAnalysis.callback_url,
    timeout: codeAnalysis.timeout,
    task_timeout: codeAnalysis.task_timeout,
    poll_interval: codeAnalysis.poll_interval,
    poll_batch: Number(codeAnalysis.poll_batch) || 20,
    call_mode: codeAnalysis.call_mode,
    sync_timeout: codeAnalysis.sync_timeout,
    notify_on_submit: codeAnalysis.notify_on_submit,
    protocol: codeAnalysis.protocol,
    auth_header: codeAnalysis.auth_header,
    sync_submit_path: codeAnalysis.sync_submit_path,
    repo_locator_mode: codeAnalysis.repo_locator_mode,
    repo_git_url: codeAnalysis.repo_git_url,
    environment: codeAnalysis.environment,
    priority: Number(codeAnalysis.priority) || 0,
    auto_verify: codeAnalysis.auto_verify,
  }
  if (analysisKeyDraft.api_key.trim()) {
    payload.api_key = analysisKeyDraft.api_key.trim()
  }
  if (analysisKeyClear.api_key) {
    payload.clear_api_key = true
  }
  if (analysisKeyDraft.callback_token.trim()) {
    payload.callback_token = analysisKeyDraft.callback_token.trim()
  }
  if (analysisKeyClear.callback_token) {
    payload.clear_callback_token = true
  }
  return payload
}

/** 新填了密钥就不再请求清空（新值优先）。 */
function onApiKeyInput(key: ProviderKey): void {
  if (apiKeyDraft[key]) {
    apiKeyClear[key] = false
  }
}

/** 标记清空密钥。 */
function toggleClearKey(key: ProviderKey): void {
  apiKeyClear[key] = !apiKeyClear[key]
  if (apiKeyClear[key]) {
    apiKeyDraft[key] = ''
  }
}

/** 密钥输入框 placeholder。 */
function apiKeyPlaceholder(key: ProviderKey): string {
  if (apiKeyClear[key]) {
    return '保存后将清空已存密钥'
  }
  if (apiKeyDraft[key]) {
    return '将替换为新密钥'
  }
  return apiKeyMasked[key] ? `已保存：${apiKeyMasked[key]}，留空表示不修改` : '尚未配置，填写后保存'
}

/** 组装单个提供方入参：空密钥不下发，避免把占位符写进后端。 */
function buildProvider(key: ProviderKey): AISettingsInput['third_party'] {
  const item = providers[key]
  const payload: AISettingsInput['third_party'] = {
    enabled: item.enabled,
    kind: item.kind,
    base_url: item.base_url,
    model: item.model,
    max_tokens: Number(item.max_tokens) || 0,
    price_per_k_token: Number(item.price_per_k_token) || 0,
  }
  const draft = apiKeyDraft[key].trim()
  if (draft) {
    payload.api_key = draft
  }
  if (apiKeyClear[key]) {
    payload.clear_api_key = true
  }
  return payload
}

/** 保存全部 AI 设置（策略 + 双提供方 + 额度）。 */
async function save(): Promise<void> {
  saving.value = true
  try {
    const payload: AISettingsInput = {
      strategy: form.strategy,
      third_party: buildProvider('third_party'),
      self_hosted: buildProvider('self_hosted'),
      code_analysis: buildCodeAnalysis(),
      daily_token_quota: Number(form.daily_token_quota) || 0,
      per_user_daily_quota: Number(form.per_user_daily_quota) || 0,
    }
    view.value = await settingApi.saveAI(payload)
    ElMessage({ type: 'success', message: 'AI 设置已保存' })
    await load()
    await loadUsage()
  } catch (error) {
    toastError(error)
  } finally {
    saving.value = false
  }
}

/**
 * 自测「AI 代码分析」外部服务的连通性（不保存）。
 *
 * 密钥只在本次真的填了新值时才下发：留空表示沿用已存密钥，
 * 与保存的三态语义一致——否则测的是一把空 key，必然 401。
 */
async function testCodeAnalysis(): Promise<void> {
  caTesting.value = true
  caTestResult.value = null
  try {
    const payload: AISettingsInput['code_analysis'] = { ...buildCodeAnalysis() }
    if (analysisKeyDraft.api_key.trim()) {
      payload.api_key = analysisKeyDraft.api_key.trim()
    }
    caTestResult.value = await settingApi.testCodeAnalysis(payload)
  } catch (error) {
    toastError(error)
  } finally {
    caTesting.value = false
  }
}

/** 刷新：设置与用量一起重拉。 */
async function refresh(): Promise<void> {
  await load()
  await loadUsage()
}

/** 测试连接：失败也返回 ok=false 与原因，就地展示。 */
async function testConnection(): Promise<void> {
  testing.value = true
  testResult.value = null
  try {
    testResult.value = await settingApi.testAI()
    if (testResult.value.ok) {
      ElMessage({ type: 'success', message: 'AI 连接正常' })
    }
  } catch (error) {
    toastError(error)
  } finally {
    testing.value = false
  }
}

/** 按提供方自测连接：用当前合并配置（含本次填写的密钥）验证调用是否正确，不保存。 */
async function testProvider(key: ProviderKey): Promise<void> {
  testingProvider[key] = true
  testResultProvider[key] = null
  try {
    const item = providers[key]
    const payload: AIProviderTestInput = {
      provider: key,
      enabled: item.enabled,
      kind: item.kind,
      base_url: item.base_url,
      // 只在新填了密钥时下发，避免把空串当成「清空」去测一个空 key。
      api_key: apiKeyDraft[key] ? apiKeyDraft[key] : undefined,
      model: item.model,
      max_tokens: Number(item.max_tokens) || 0,
    }
    testResultProvider[key] = await settingApi.testAIProvider(payload)
    if (testResultProvider[key]?.ok) {
      ElMessage({ type: 'success', message: `${providerList.find((p) => p.key === key)?.title} 连接正常` })
    }
  } catch (error) {
    toastError(error)
  } finally {
    testingProvider[key] = false
  }
}

onMounted(async () => {
  await load()
  await loadUsage()
})
</script>

<template>
  <div class="page" v-loading="loading">
    <div class="page-header">
      <div>
        <h2 class="page-title">AI 设置</h2>
        <p class="page-subtitle">
          AI Key、调用策略、token 额度与消费趋势；密钥只写不读，页面上永远只显示掩码
        </p>
      </div>
      <div class="row">
        <el-button :icon="'Refresh'" size="small" @click="refresh">刷新</el-button>
        <el-button
          :icon="'Connection'"
          size="small"
          :loading="testing"
          :disabled="!canWrite"
          @click="testConnection"
        >
          测试连接
        </el-button>
        <el-button
          v-if="canWrite"
          type="primary"
          size="small"
          :icon="'Check'"
          :loading="saving"
          @click="save"
        >
          保存
        </el-button>
      </div>
    </div>

    <!-- 当前生效概览 -->
    <div class="card">
      <h3 class="card-title">
        <span>当前生效</span>
        <el-tag size="small" effect="plain" type="success">
          来源：平台设置
        </el-tag>
      </h3>
      <div class="kv-list">
        <div class="kv">
          <span>生效提供方</span>
          <span class="row">
            <el-tag v-for="item in providersActive" :key="item" size="small" effect="plain" class="mini-tag">
              {{ item }}
            </el-tag>
            <span v-if="providersActive.length === 0" class="muted">暂无（未启用任何提供方）</span>
          </span>
        </div>
        <div class="kv">
          <span>调用策略</span>
          <span class="mono">{{ strategyOptions.find((item) => item.value === form.strategy)?.label || form.strategy }}</span>
        </div>
        <div class="kv">
          <span>最近更新</span>
          <span class="mono">
            {{ formatTime(view?.updated_at) }}
            <span v-if="view?.updated_by" class="muted">（{{ view.updated_by }}）</span>
          </span>
        </div>
      </div>
    </div>

    <!-- AI Key 设置 -->
    <div class="card">
      <h3 class="card-title">
        <span>AI Key 设置</span>
        <el-tag v-if="!canWrite" size="small" type="info" effect="plain">只读：缺少 system:config:write</el-tag>
      </h3>

      <el-form label-position="top" :disabled="!canWrite">
        <el-row :gutter="12">
          <el-col :xs="24" :sm="12">
            <el-form-item label="调用策略">
              <el-select v-model="form.strategy" class="mobile-block">
                <el-option v-for="item in strategyOptions" :key="item.value" :label="item.label" :value="item.value" />
              </el-select>
            </el-form-item>
            <p class="field-hint">
              策略决定实际用哪个提供方：<span class="mono">hybrid</span> 下第三方失败会自动降级到自建模型。
            </p>
          </el-col>
        </el-row>
      </el-form>

      <div v-for="item in providerList" :key="item.key" class="provider">
        <div class="provider-head">
          <div>
            <p class="provider-title">{{ item.title }}</p>
            <p class="field-hint">{{ item.hint }}</p>
          </div>
          <div class="row">
            <el-tag v-if="apiKeyClear[item.key]" size="small" type="danger" effect="plain">保存后清空密钥</el-tag>
            <el-tag v-else-if="apiKeyDraft[item.key]" size="small" type="warning" effect="plain">保存后替换密钥</el-tag>
            <el-button
              :icon="'Connection'"
              size="small"
              :loading="testingProvider[item.key]"
              :disabled="!canWrite || !providers[item.key].enabled"
              @click="testProvider(item.key)"
            >
              测试连接
            </el-button>
            <el-switch v-model="providers[item.key].enabled" :disabled="!canWrite" active-text="启用" />
          </div>
        </div>

        <el-form label-position="top" :disabled="!canWrite">
          <el-row :gutter="12">
            <el-col :xs="24" :sm="8">
              <el-form-item label="协议 kind">
                <el-select v-model="providers[item.key].kind" class="mobile-block">
                  <el-option v-for="kind in kindOptions" :key="kind" :label="kind" :value="kind" />
                </el-select>
              </el-form-item>
            </el-col>
            <el-col :xs="24" :sm="16">
              <el-form-item label="Base URL">
                <el-input
                  v-model="providers[item.key].base_url"
                  class="mono"
                  placeholder="如 https://api.openai.com/v1；Ollama 如 http://127.0.0.1:11434"
                />
              </el-form-item>
            </el-col>
            <el-col :xs="24" :sm="12">
              <el-form-item label="API Key">
                <div class="row key-row">
                  <el-input
                    v-model="apiKeyDraft[item.key]"
                    type="password"
                    show-password
                    autocomplete="new-password"
                    class="key-input"
                    :placeholder="apiKeyPlaceholder(item.key)"
                    @input="onApiKeyInput(item.key)"
                  />
                  <el-button
                    :type="apiKeyClear[item.key] ? 'danger' : 'default'"
                    :plain="apiKeyClear[item.key]"
                    :icon="'Delete'"
                    @click="toggleClearKey(item.key)"
                  >
                    {{ apiKeyClear[item.key] ? '取消清除' : '清除' }}
                  </el-button>
                </div>
              </el-form-item>
              <p class="field-hint">
                已存密钥不显示明文；留空即不改动。
                <span v-if="apiKeyMasked[item.key]" class="mono">{{ apiKeyMasked[item.key] }}</span>
                <span v-else class="muted">当前未配置</span>
              </p>
            </el-col>
            <el-col :xs="24" :sm="12">
              <el-form-item label="模型">
                <el-input v-model="providers[item.key].model" class="mono" placeholder="如 gpt-4o-mini / qwen2.5:14b" />
              </el-form-item>
            </el-col>
            <el-col :xs="24" :sm="8">
              <el-form-item label="单次最大输出 tokens">
                <el-input-number
                  v-model="providers[item.key].max_tokens"
                  :min="0"
                  :max="200000"
                  :step="256"
                  controls-position="right"
                  class="mobile-block"
                />
              </el-form-item>
            </el-col>
            <el-col :xs="24" :sm="8">
              <el-form-item label="价格 / 千 token">
                <el-input-number
                  v-model="providers[item.key].price_per_k_token"
                  :min="0"
                  :max="1000"
                  :step="0.001"
                  :precision="4"
                  controls-position="right"
                  class="mobile-block"
                />
              </el-form-item>
            </el-col>
            <el-col :xs="24" :sm="8">
              <el-form-item label="密钥状态">
                <el-tag size="small" :type="apiKeyMasked[item.key] ? 'success' : 'info'" effect="light">
                  {{ apiKeyMasked[item.key] ? '已配置（只显示掩码）' : '未配置' }}
                </el-tag>
              </el-form-item>
            </el-col>
          </el-row>
        </el-form>

        <!-- 该提供方自测结果：就地展示 ok / 失败原因 + 耗时 -->
        <el-alert
          v-if="testResultProvider[item.key]"
          class="mb-top"
          :type="testResultProvider[item.key]!.ok ? 'success' : 'error'"
          :closable="false"
          show-icon
          :title="testResultProvider[item.key]!.ok ? `连接成功：${testResultProvider[item.key]!.engine}` : `连接失败：${testResultProvider[item.key]!.engine}`"
        >
          <p class="field-hint">{{ testResultProvider[item.key]!.message }}</p>
          <p class="field-hint mono">耗时 {{ testResultProvider[item.key]!.latency_ms }} ms</p>
        </el-alert>
      </div>

      <!-- 全局测试连接结果：就地展示 ok / 失败原因 + 耗时 -->
      <el-alert
        v-if="testResult"
        class="mb-top"
        :type="testResult.ok ? 'success' : 'error'"
        :closable="false"
        show-icon
        :title="testResult.ok ? `连接成功：${testResult.engine}` : `连接失败：${testResult.engine}`"
      >
        <p class="field-hint">{{ testResult.message }}</p>
        <p class="field-hint mono">耗时 {{ testResult.latency_ms }} ms</p>
      </el-alert>

      <div v-if="canWrite" class="row actions">
        <el-button type="primary" :loading="saving" :icon="'Check'" @click="save">保存 AI 设置</el-button>
      </div>
    </div>

    <!-- AI 代码分析：日志告警把问题提交给外部服务，结论由回调/轮询带回 -->
    <div class="card">
      <h3 class="card-title">
        <span>AI 代码分析</span>
        <el-tag v-if="!canWrite" size="small" type="info" effect="plain">只读：缺少 system:config:write</el-tag>
        <span class="muted head-note">日志告警用；平台是唯一来源，保存即生效</span>
      </h3>
      <p class="field-hint">
        日志告警把「问题」（脱敏后的错误信息 + 堆栈）提交给这个服务，由它完成代码分析；
        结论通过回调 <span class="mono">/api/ai/analysis/callback</span> 或平台轮询取回，再送进通知。
      </p>
      <el-alert
        v-if="!codeAnalysis.enabled || !codeAnalysis.base_url"
        class="mb-top"
        type="info"
        :closable="false"
        show-icon
        title="当前不会做 AI 分析"
      >
        <p class="field-hint">
          开关未开或服务地址为空时，日志事件的分析状态会明确标为「未启用」并写明原因，不会假装排队。
        </p>
      </el-alert>

      <el-form label-position="top" :disabled="!canWrite">
        <el-row :gutter="12">
          <el-col :xs="24" :sm="12">
            <el-form-item label="服务地址 Base URL">
              <el-input v-model="codeAnalysis.base_url" class="mono" placeholder="如 http://ai.internal:8000" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="启用与生效状态">
              <div class="row">
                <el-switch v-model="codeAnalysis.enabled" active-text="启用" />
                <el-tag size="small" :type="view?.code_analysis?.configured ? 'success' : 'info'" effect="light">
                  {{ view?.code_analysis?.configured ? '已生效' : '未生效' }}
                </el-tag>
              </div>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="API Key（调用凭据）">
              <div class="row key-row">
                <el-input
                  v-model="analysisKeyDraft.api_key"
                  type="password"
                  show-password
                  autocomplete="new-password"
                  class="key-input"
                  :placeholder="analysisKeyMasked.api_key ? `已保存：${analysisKeyMasked.api_key}，留空表示不修改` : '尚未配置，填写后保存'"
                  @input="onAnalysisKeyInput('api_key')"
                />
                <el-button
                  :type="analysisKeyClear.api_key ? 'danger' : 'default'"
                  :plain="analysisKeyClear.api_key"
                  :icon="'Delete'"
                  @click="toggleAnalysisClear('api_key')"
                >
                  {{ analysisKeyClear.api_key ? '取消清除' : '清除' }}
                </el-button>
              </div>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="回调令牌（AI 服务回传结论时校验）">
              <div class="row key-row">
                <el-input
                  v-model="analysisKeyDraft.callback_token"
                  type="password"
                  show-password
                  autocomplete="new-password"
                  class="key-input"
                  :placeholder="analysisKeyMasked.callback_token ? `已保存：${analysisKeyMasked.callback_token}，留空表示不修改` : '未配置则拒收所有回调（改由轮询兜底）'"
                  @input="onAnalysisKeyInput('callback_token')"
                />
                <el-button
                  :type="analysisKeyClear.callback_token ? 'danger' : 'default'"
                  :plain="analysisKeyClear.callback_token"
                  :icon="'Delete'"
                  @click="toggleAnalysisClear('callback_token')"
                >
                  {{ analysisKeyClear.callback_token ? '取消清除' : '清除' }}
                </el-button>
              </div>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="对接协议">
              <el-select v-model="codeAnalysis.protocol" class="mobile-block">
                <el-option label="通用协议（question / task_id）" value="generic" />
                <el-option label="开放接口 v1（repoLocator / stacktrace）" value="openapi_v1" />
              </el-select>
              <p class="field-hint">
                开放接口 v1 指《AI 代码分析接口文档 v1》：
                <span class="mono">X-API-Key</span> 鉴权、按
                <span class="mono">repoLocator</span> 定位仓库、
                <span class="mono">stacktrace</span> 为必填主输入。
                切换时「提交/查询路径」若还是通用协议的默认值会自动改写。
              </p>
            </el-form-item>
          </el-col>
          <el-col v-if="isOpenAPI" :xs="24" :sm="12">
            <el-form-item label="鉴权头">
              <el-input v-model="codeAnalysis.auth_header" class="mono" placeholder="X-API-Key" />
              <p class="field-hint">留空表示 Authorization: Bearer &lt;key&gt;；开放接口请填 X-API-Key。</p>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="提交路径">
              <el-input v-model="codeAnalysis.submit_path" class="mono" placeholder="/v1/analyses" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="查询路径">
              <el-input v-model="codeAnalysis.query_path" class="mono" placeholder="/v1/analyses/{task_id}" />
            </el-form-item>
          </el-col>
          <el-col v-if="isOpenAPI" :xs="24" :sm="12">
            <el-form-item label="同步提交路径">
              <el-input v-model="codeAnalysis.sync_submit_path" class="mono" placeholder="/api/v1/openapi/analyze" />
              <p class="field-hint">开放接口的同步与异步是两个端点，仅 call_mode=sync 时使用。</p>
            </el-form-item>
          </el-col>
          <el-col v-if="isOpenAPI" :xs="24" :sm="12">
            <el-form-item label="仓库定位方式">
              <el-select v-model="codeAnalysis.repo_locator_mode" class="mobile-block">
                <el-option label="用服务名当 host（推荐）" value="service" />
                <el-option label="固定 git 地址（单仓）" value="git_url" />
              </el-select>
              <p class="field-hint">
                用服务名时，需要在 AI 服务控制台给仓库配好 hostPatterns / keywords。
              </p>
            </el-form-item>
          </el-col>
          <el-col v-if="isOpenAPI && codeAnalysis.repo_locator_mode === 'git_url'" :xs="24" :sm="12">
            <el-form-item label="仓库 git 地址">
              <el-input v-model="codeAnalysis.repo_git_url" class="mono" placeholder="https://git.x/order.git" />
            </el-form-item>
          </el-col>
          <el-col v-if="isOpenAPI" :xs="12" :sm="6">
            <el-form-item label="环境标识">
              <el-input v-model="codeAnalysis.environment" class="mono" placeholder="prod" />
            </el-form-item>
          </el-col>
          <el-col v-if="isOpenAPI" :xs="12" :sm="6">
            <el-form-item label="任务优先级">
              <el-input-number
                v-model="codeAnalysis.priority"
                :min="0"
                :max="999"
                controls-position="right"
                class="mobile-block"
              />
            </el-form-item>
          </el-col>
          <el-col v-if="isOpenAPI" :xs="24" :sm="12">
            <el-form-item label="沙箱验证">
              <div class="row">
                <el-switch v-model="codeAnalysis.auto_verify" active-text="提交时要求验证" />
                <span class="muted">关闭表示不提交该字段，按服务端默认执行</span>
              </div>
            </el-form-item>
          </el-col>
          <el-col :span="24">
            <el-form-item label="回调地址（平台对外地址）">
              <el-input
                v-model="codeAnalysis.callback_url"
                class="mono"
                placeholder="如 http://platform.example.com；反向代理后填对外域名"
              />
              <p class="field-hint">
                <!-- 通用协议下留空可以只靠轮询；开放接口把 callbackUrl 定为异步模式的必填字段，
                     留空会被服务端直接 400，所以这里的措辞必须按协议区分。 -->
                <template v-if="isOpenAPI">
                  开放接口的异步模式<b>必填</b>：留空会被服务端拒绝（HTTP 400）。
                </template>
                <template v-else>
                  AI 服务要能访问到这个地址；留空则不带 callback_url，结论只由轮询兜底。
                </template>
                <template v-if="isOpenAPI">
                  AI 服务要能访问到这个地址；需要自带校验参数时用 <span class="mono">{path}</span> 占位，例如
                  <span class="mono">https://platform.example.com/{path}?token=xxx</span>。
                </template>
              </p>
            </el-form-item>
          </el-col>
          <el-col :xs="12" :sm="6">
            <el-form-item label="单次请求超时">
              <el-input v-model="codeAnalysis.timeout" class="mono" placeholder="15s" />
            </el-form-item>
          </el-col>
          <el-col :xs="12" :sm="6">
            <el-form-item label="任务超时">
              <el-input v-model="codeAnalysis.task_timeout" class="mono" placeholder="30m" />
            </el-form-item>
          </el-col>
          <el-col :xs="12" :sm="6">
            <el-form-item label="轮询间隔">
              <el-input v-model="codeAnalysis.poll_interval" class="mono" placeholder="60s" />
            </el-form-item>
          </el-col>
          <el-col :xs="12" :sm="6">
            <el-form-item label="每轮轮询条数">
              <el-input-number
                v-model="codeAnalysis.poll_batch"
                :min="1"
                :max="200"
                controls-position="right"
                class="mobile-block"
              />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="调用方式">
              <el-select v-model="codeAnalysis.call_mode" class="mobile-block">
                <el-option label="异步回调 / 轮询（推荐）" value="async" />
                <el-option label="同步等待结论" value="sync" />
              </el-select>
              <p class="field-hint">
                异步：提交后只拿 task_id，结论由回调或平台轮询带回（慢分析不占后处理线程）；
                同步：提交后在「同步等待上限」内原地等到结论，不依赖回调，但会占用线程（建议配小批量）。
              </p>
            </el-form-item>
          </el-col>
          <el-col v-if="codeAnalysis.call_mode === 'sync'" :xs="24" :sm="12">
            <el-form-item label="同步等待上限">
              <el-input v-model="codeAnalysis.sync_timeout" class="mono" placeholder="5m" />
              <p class="field-hint">超过该时长仍未返回结论，本次分析按失败处理（事件明确标 failed）。</p>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="提交即通知">
              <div class="row">
                <el-switch v-model="codeAnalysis.notify_on_submit" />
                <span class="muted">先发一条告警，结论到达后再发一条（一条告警变两条消息）</span>
              </div>
            </el-form-item>
          </el-col>
        </el-row>
      </el-form>

      <!-- 自测结果就地展示：连通性自检的目的就是把原因原样给配置人看，
           而不是统一成"服务异常"——地址不通、密钥错、仓库没注册三种原因的处理方式完全不同。 -->
      <el-alert
        v-if="caTestResult"
        class="mb-top"
        :type="caTestResult.ok ? 'success' : 'error'"
        :closable="false"
        show-icon
        :title="caTestResult.ok ? `连接成功：${caTestResult.engine}` : `连接失败：${caTestResult.engine}`"
      >
        <p class="field-hint">{{ caTestResult.message }}</p>
        <p class="field-hint mono">耗时 {{ caTestResult.latency_ms }} ms</p>
      </el-alert>

      <!-- 保存按钮必须在这张卡片里就地出现：原来只有上方「AI Key 设置」卡片底部有，
           填完往下滚动看不到，会以为改了没生效（实际没点保存就不会落库）。 -->
      <div v-if="canWrite" class="row actions">
        <el-button
          :icon="'Connection'"
          :loading="caTesting"
          :disabled="!codeAnalysis.base_url"
          @click="testCodeAnalysis"
        >
          测试连接
        </el-button>
        <el-button type="primary" :loading="saving" :icon="'Check'" @click="save">保存 AI 代码分析</el-button>
      </div>
      <p class="muted foot">
        与「AI Key 设置」、额度共用一次保存请求（<span class="mono">PUT /api/settings/ai</span>），
        密钥留空表示不修改。
      </p>
    </div>

    <!-- 额度与消费 -->
    <div class="card">
      <h3 class="card-title">
        <span>额度与消费</span>
        <span class="muted head-note">统计窗口 {{ usage?.window_days ?? 30 }} 天</span>
      </h3>

      <el-row :gutter="12">
        <el-col :xs="24" :sm="8">
          <StatCard label="今日已用 tokens" :value="thousand(usage?.today_tokens || 0)" hint="按自然日汇总" />
        </el-col>
        <el-col :xs="24" :sm="8">
          <StatCard
            label="剩余额度"
            :value="remainingText"
            :status="remainingStatus"
            :hint="(usage?.daily_quota || 0) > 0 ? `日额度 ${thousand(usage?.daily_quota || 0)} tokens` : '未设置日额度（不限）'"
          />
        </el-col>
        <el-col :xs="24" :sm="8">
          <StatCard label="今日调用次数" :value="thousand(usage?.today_calls || 0)" hint="含缓存命中与降级调用" />
        </el-col>
      </el-row>

      <div class="progress-block">
        <div class="row progress-head">
          <span class="muted">日额度使用进度</span>
          <span class="mono">{{ usedPercent }}%（比值 {{ (usage?.used_ratio ?? 0).toFixed(3) }}）</span>
        </div>
        <el-progress :percentage="usedPercent" :status="usageStatus" :stroke-width="10" />
        <p class="field-hint">
          「剩余额度」为 <span class="mono">-1</span> 时表示不限量；触顶后平台会拒绝新的 AI 调用并走规则引擎兜底。
        </p>
      </div>

      <el-form label-position="top" :disabled="!canWrite" class="quota-form">
        <el-row :gutter="12">
          <el-col :xs="24" :sm="12">
            <el-form-item label="日额度（tokens，0 或 -1 表示不限）">
              <el-input-number
                v-model="form.daily_token_quota"
                :min="-1"
                :max="1000000000"
                :step="100000"
                controls-position="right"
                class="mobile-block"
              />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="每人日额度（tokens，0 或 -1 表示不限）">
              <el-input-number
                v-model="form.per_user_daily_quota"
                :min="-1"
                :max="1000000000"
                :step="10000"
                controls-position="right"
                class="mobile-block"
              />
            </el-form-item>
          </el-col>
        </el-row>
      </el-form>

      <h4 class="section">消费趋势（最近 7 天）</h4>
      <div class="table-scroll">
        <el-table :data="trendData" size="small" empty-text="暂无消费数据">
          <el-table-column prop="date" label="日期" width="130" />
          <el-table-column label="tokens" min-width="120">
            <template #default="{ row }">
              <span class="mono">{{ thousand(row.tokens) }}</span>
            </template>
          </el-table-column>
          <el-table-column label="调用次数" min-width="110">
            <template #default="{ row }">
              <span class="mono">{{ thousand(row.calls) }}</span>
            </template>
          </el-table-column>
        </el-table>
      </div>

      <el-row :gutter="12" class="mt">
        <el-col :xs="24" :md="12">
          <h4 class="section">按来源分布</h4>
          <div class="table-scroll">
            <el-table :data="usage?.by_source || []" size="small" empty-text="暂无数据">
              <el-table-column prop="source" label="来源" min-width="120" />
              <el-table-column label="tokens" min-width="110">
                <template #default="{ row }">
                  <span class="mono">{{ thousand(row.tokens) }}</span>
                </template>
              </el-table-column>
              <el-table-column label="调用次数" min-width="100">
                <template #default="{ row }">
                  <span class="mono">{{ thousand(row.calls) }}</span>
                </template>
              </el-table-column>
            </el-table>
          </div>
        </el-col>
        <el-col :xs="24" :md="12">
          <h4 class="section">Top 用户</h4>
          <div class="table-scroll">
            <el-table :data="usage?.top_users || []" size="small" empty-text="暂无数据">
              <el-table-column label="用户" min-width="140">
                <template #default="{ row }">
                  {{ row.username || `#${row.user_id}` }}
                  <span class="muted mono">#{{ row.user_id }}</span>
                </template>
              </el-table-column>
              <el-table-column label="tokens" min-width="110">
                <template #default="{ row }">
                  <span class="mono">{{ thousand(row.tokens) }}</span>
                </template>
              </el-table-column>
              <el-table-column label="调用次数" min-width="100">
                <template #default="{ row }">
                  <span class="mono">{{ thousand(row.calls) }}</span>
                </template>
              </el-table-column>
            </el-table>
          </div>
        </el-col>
      </el-row>

      <div v-if="canWrite" class="row actions">
        <el-button type="primary" :loading="saving" :icon="'Check'" @click="save">保存额度设置</el-button>
      </div>
      <p class="muted foot">
        额度与「AI Key 设置」共用一次保存请求（<span class="mono">PUT /api/settings/ai</span>）。
      </p>
    </div>
  </div>
</template>

<style scoped>
.mb {
  margin-bottom: 12px;
}

.mb-top {
  margin-top: 12px;
}

.mt {
  margin-top: 12px;
}

.mini-tag {
  margin-right: 4px;
}

.kv-list {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.kv {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  font-size: 12.5px;
  color: var(--c-text-2);
}

.provider {
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  padding: 12px;
  margin-top: 12px;
}

.provider-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 10px;
  flex-wrap: wrap;
  margin-bottom: 8px;
}

.provider-title {
  margin: 0;
  font-size: 13.5px;
  font-weight: 600;
}

.key-row {
  flex-wrap: nowrap;
}

.key-input {
  flex: 1;
  min-width: 0;
}

.progress-block {
  margin-top: 14px;
}

.progress-head {
  justify-content: space-between;
  margin-bottom: 6px;
  font-size: 12.5px;
}

.quota-form {
  margin-top: 12px;
}

.section {
  margin: 14px 0 8px;
  font-size: 13px;
  font-weight: 600;
}

.actions {
  margin-top: 12px;
}

.head-note {
  font-size: 11.5px;
  font-weight: 400;
}

.foot {
  margin: 10px 0 0;
  font-size: 11.5px;
}

.field-hint {
  margin: 2px 0 0;
  font-size: 11.5px;
  line-height: 1.6;
  color: var(--c-text-3);
}

@media (max-width: 767px) {
  .key-row {
    flex-wrap: wrap;
  }

  .key-input {
    width: 100%;
  }
}
</style>
