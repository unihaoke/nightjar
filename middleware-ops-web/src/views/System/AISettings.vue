<script setup lang="ts">
/**
 * AI 设置：AI Key（密钥/协议/模型）、额度与 token 消费。
 *
 * 密钥安全约定：接口只返回「是否已配置」与掩码，页面永不回显明文；
 * 输入框留空 = 不修改，点「清除」= 保存后清空（clear_api_key）。
 * 当前生效来源仍是 .env 时提示「保存一次即改为平台管理」。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { settingApi } from '@/api'
import { toastError } from '@/api/http'
import type { AISettingsInput, AISettingsView, AITestResult, AIUsageView } from '@/api/types'
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
/** 仍是 .env 生效：保存一次即转为平台管理。 */
const envManaged = computed(() => view.value?.source === 'env')
const providersActive = computed(() => view.value?.providers_active || [])

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

/** 趋势表只展示最近 14 天（接口默认返回 30 天，表格不引图表库）。 */
const recentSeries = computed(() => (usage.value?.series || []).slice(-14))

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

    <!-- 生效来源：仍是 .env 时给出明确预期 -->
    <el-alert
      v-if="envManaged"
      type="warning"
      :closable="false"
      show-icon
      class="mb"
      title="当前仍是 .env 里的配置，保存一次即改为平台管理"
    >
      <p class="field-hint">
        平台已读取到的密钥不会被回显；保存后配置改由平台数据库托管，
        <span class="mono">.env</span> 里的同名字段不再覆盖页面设置。
      </p>
    </el-alert>

    <!-- 当前生效概览 -->
    <div class="card">
      <h3 class="card-title">
        <span>当前生效</span>
        <el-tag size="small" effect="plain" :type="envManaged ? 'warning' : 'success'">
          {{ envManaged ? '来源：环境变量（env）' : '来源：平台设置（platform）' }}
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
      </div>

      <!-- 测试连接结果：就地展示 ok / 失败原因 + 耗时 -->
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

      <h4 class="section">消费趋势（最近 14 天）</h4>
      <div class="table-scroll">
        <el-table :data="recentSeries" size="small" empty-text="暂无消费数据">
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
