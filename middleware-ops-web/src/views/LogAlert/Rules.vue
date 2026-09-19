<script setup lang="ts">
/**
 * 日志告警规则（4.8.2）：列表 + 新建/编辑弹窗 + 启停 + 字段校验。
 *
 * 与指标告警规则（Alert/Rules.vue）形态刻意对齐：两个页面一套心智模型。
 * 差别在"判定输入"——指标规则比数值（metric > threshold），日志规则比**服务 + 日志消息 + 级别**，
 * 因此字段是 service_name / signature_pattern / min_severity。
 * 注意 signature_pattern 匹配的是 message 原文（与屏蔽项同一套写法），不是错误指纹。
 *
 * 平台**没有任何默认规则**：日志必须命中这里新增的某条规则才会产生告警，
 * 否则不入库、不通知、不分析——页面顶部必须把这条说清，否则使用者会以为平台漏收了日志。
 *
 * 顶部另一块是「屏蔽规则」：把"这类错误我不想收到"（如框架噪音 Request method 'GET' is not supported）
 * 变成可配置条目，命中即丢弃，优先于所有规则生效；可配多条。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { logAlertApi } from '@/api'
import { toastError } from '@/api/http'
import type { LogAlertExclusion, LogAlertRule, LogAlertRuleInput } from '@/api/types'
import { useListPage } from '@/composables/useListPage'
import ResponsiveList from '@/components/ResponsiveList.vue'
import { useUserStore } from '@/stores/user'

const store = useUserStore()

/** 可选通知渠道：与后端渠道枚举一致。留空表示使用平台默认渠道。 */
const CHANNEL_OPTIONS = ['feishu', 'wecom', 'dingtalk', 'email']

/** 最低级别候选：留空表示不限级别。 */
const SEVERITY_OPTIONS = ['INFO', 'WARN', 'ERROR', 'FATAL']

const submitting = ref(false)
const dialogVisible = ref(false)
const editing = ref<LogAlertRule | null>(null)
const formRef = ref<FormInstance>()

const list = useListPage<LogAlertRule>({
  fetch: (params, signal) => logAlertApi.rules(params, signal),
  defaults: { keyword: '', page: 1, page_size: 20 },
})
const { query, items, total, loading, error } = list

const canWrite = computed(() => store.can('logalert:write'))

// ---------------------------------------------------------------------------
// 屏蔽规则：命中的日志不入库、不通知、不分析（优先于所有规则）
//
// 单独存失败原因，而不是只看"列表为空"：接口不通与"确实没配屏蔽项"是两件事，
// 界面必须区分（INC-016：不能把未知显示成结论）。
// ---------------------------------------------------------------------------
const exclusions = ref<LogAlertExclusion[]>([])
const exclLoading = ref(false)
const exclError = ref('')

async function loadExclusions(): Promise<void> {
  exclLoading.value = true
  try {
    const result = await logAlertApi.exclusions({ keyword: '', page: 1, page_size: 200 })
    exclusions.value = result?.list || []
    exclError.value = ''
  } catch (err) {
    exclusions.value = []
    exclError.value = err instanceof Error ? err.message : String(err)
  } finally {
    exclLoading.value = false
  }
}

const exclDialogVisible = ref(false)
const exclEditing = ref<LogAlertExclusion | null>(null)
const exclFormRef = ref<FormInstance>()
const exclSubmitting = ref(false)
const exclForm = reactive({ name: '', service_name: '', pattern: '', enabled: true })

const exclRules: FormRules = {
  pattern: [
    { required: true, message: '请填写要屏蔽的内容', trigger: 'blur' },
    {
      validator: (_rule, value, callback) => {
        // 与规则里的指纹匹配同一套写法：只有 `/.../` 形式才按正则校验。
        const text = String(value ?? '').trim()
        if (!text.startsWith('/') || !text.endsWith('/') || text.length < 2) {
          callback()
          return
        }
        try {
          new RegExp(text.slice(1, -1))
          callback()
        } catch (err) {
          callback(new Error(`正则不合法：${err instanceof Error ? err.message : String(err)}`))
        }
      },
      trigger: 'blur',
    },
  ],
}

function openExclusion(item?: LogAlertExclusion): void {
  exclEditing.value = item || null
  Object.assign(
    exclForm,
    item
      ? { name: item.name || '', service_name: item.service_name || '', pattern: item.pattern || '', enabled: item.enabled }
      : { name: '', service_name: '', pattern: '', enabled: true },
  )
  exclDialogVisible.value = true
  exclFormRef.value?.clearValidate()
}

async function submitExclusion(): Promise<void> {
  if (!exclFormRef.value) {
    return
  }
  const valid = await exclFormRef.value.validate().catch(() => false)
  if (!valid) {
    return
  }
  exclSubmitting.value = true
  try {
    const payload = { ...exclForm }
    if (exclEditing.value) {
      await logAlertApi.updateExclusion(exclEditing.value.id, payload)
      ElMessage({ type: 'success', message: '屏蔽项已更新' })
    } else {
      await logAlertApi.createExclusion(payload)
      ElMessage({ type: 'success', message: '屏蔽项已新增，即刻生效' })
    }
    exclDialogVisible.value = false
    await loadExclusions()
  } catch (error) {
    toastError(error)
  } finally {
    exclSubmitting.value = false
  }
}

async function toggleExclusion(item: LogAlertExclusion): Promise<void> {
  try {
    await logAlertApi.updateExclusion(item.id, { enabled: !item.enabled })
    await loadExclusions()
  } catch (error) {
    toastError(error)
  }
}

/** 删除屏蔽项：删除后同类错误会重新开始告警，所以确认文案要说清后果。 */
async function removeExclusion(item: LogAlertExclusion): Promise<void> {
  try {
    await ElMessageBox.confirm(`删除后包含「${item.pattern}」的错误将重新开始告警，确认删除？`, '删除确认', {
      confirmButtonText: '删除',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await logAlertApi.removeExclusion(item.id)
    ElMessage({ type: 'success', message: '屏蔽项已删除' })
    await loadExclusions()
  } catch (error) {
    toastError(error)
  }
}

onMounted(loadExclusions)

/** 列表文案：空数组返回空串——由调用方决定显示"无数据"还是"平台默认"，两者语义不同。 */
function joinList(items: string[] | null | undefined): string {
  return (items || []).join('、')
}

// ---------------------------------------------------------------------------
// 表单
// ---------------------------------------------------------------------------
const form = reactive({
  name: '',
  description: '',
  service_name: '',
  signature_pattern: '',
  min_severity: '',
  dedup_window: 5,
  cooldown: 10,
  notify_channels: [] as string[],
  ai_enabled: true,
  enabled: true,
  priority: 100,
})

const rules: FormRules = {
  name: [{ required: true, message: '请输入规则名称', trigger: 'blur' }],
  dedup_window: [
    {
      validator: (_rule, value, callback) => {
        // 0 是合法值（表示不做窗口合并），所以不能用 required / min 这类内置规则。
        if (value === null || value === undefined || value === '') {
          callback(new Error('请填写去重窗口（分钟）'))
          return
        }
        const num = Number(value)
        if (!Number.isInteger(num) || num < 0) {
          callback(new Error('去重窗口必须是不小于 0 的整数（0 表示不去重）'))
          return
        }
        callback()
      },
      trigger: 'change',
    },
  ],
  cooldown: [
    {
      validator: (_rule, value, callback) => {
        // 同理：0 表示不冷却，是明确配置而不是"没填"。
        if (value === null || value === undefined || value === '') {
          callback(new Error('请填写冷却期（分钟）'))
          return
        }
        const num = Number(value)
        if (!Number.isInteger(num) || num < 0) {
          callback(new Error('冷却期必须是不小于 0 的整数（0 表示不冷却）'))
          return
        }
        callback()
      },
      trigger: 'change',
    },
  ],
  priority: [
    {
      validator: (_rule, value, callback) => {
        const num = Number(value)
        if (value === null || value === undefined || value === '' || !Number.isInteger(num) || num < 1) {
          callback(new Error('优先级必须是不小于 1 的整数（数字小的优先）'))
          return
        }
        callback()
      },
      trigger: 'change',
    },
  ],
  signature_pattern: [
    {
      validator: (_rule, value, callback) => {
        // 只有 `/.../` 形式才按正则处理；普通文本按子串匹配，不做正则校验。
        const text = String(value ?? '').trim()
        if (!text.startsWith('/') || !text.endsWith('/') || text.length < 2) {
          callback()
          return
        }
        try {
          // 前端先试一次：非法正则在后端只会得到一句模糊报错，这里直接指出问题。
          new RegExp(text.slice(1, -1))
          callback()
        } catch (err) {
          callback(new Error(`正则不合法：${err instanceof Error ? err.message : String(err)}`))
        }
      },
      trigger: 'blur',
    },
  ],
}

/** 规则 → 提交入参：只挑接口字段，避免把 id 等只读字段回传。 */
function toPayload(rule: LogAlertRule): LogAlertRuleInput {
  return {
    name: rule.name,
    description: rule.description || '',
    service_name: rule.service_name || '',
    signature_pattern: rule.signature_pattern || '',
    min_severity: rule.min_severity || '',
    dedup_window: rule.dedup_window ?? 5,
    cooldown: rule.cooldown ?? 10,
    notify_channels: rule.notify_channels || [],
    ai_enabled: rule.ai_enabled,
    enabled: rule.enabled,
    priority: rule.priority ?? 100,
  }
}

/** 打开表单：编辑时回填，新建时给默认值（与后端默认一致，便于直接保存）。 */
function openForm(rule?: LogAlertRule): void {
  editing.value = rule || null
  Object.assign(
    form,
    rule
      ? {
          name: rule.name,
          description: rule.description || '',
          service_name: rule.service_name || '',
          signature_pattern: rule.signature_pattern || '',
          min_severity: rule.min_severity || '',
          dedup_window: rule.dedup_window ?? 5,
          cooldown: rule.cooldown ?? 10,
          notify_channels: rule.notify_channels || [],
          ai_enabled: rule.ai_enabled,
          enabled: rule.enabled,
          priority: rule.priority ?? 100,
        }
      : {
          name: '',
          description: '',
          service_name: '',
          signature_pattern: '',
          min_severity: '',
          dedup_window: 5,
          cooldown: 10,
          notify_channels: [],
          ai_enabled: true,
          enabled: true,
          priority: 100,
        },
  )
  dialogVisible.value = true
  formRef.value?.clearValidate()
}

/** 提交。 */
async function submit(): Promise<void> {
  if (!formRef.value) {
    return
  }
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) {
    return
  }
  submitting.value = true
  try {
    const payload: LogAlertRuleInput = { ...form }
    if (editing.value) {
      await logAlertApi.updateRule(editing.value.id, payload)
      ElMessage({ type: 'success', message: '规则已更新' })
    } else {
      await logAlertApi.createRule(payload)
      ElMessage({ type: 'success', message: '规则已创建' })
    }
    dialogVisible.value = false
    await list.load()
  } catch (error) {
    toastError(error)
  } finally {
    submitting.value = false
  }
}

/** 删除。 */
async function remove(rule: LogAlertRule): Promise<void> {
  try {
    await ElMessageBox.confirm(`确认删除规则「${rule.name}」？`, '删除确认', {
      confirmButtonText: '删除',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await logAlertApi.removeRule(rule.id)
    ElMessage({ type: 'success', message: '规则已删除' })
    await list.load()
  } catch (error) {
    toastError(error)
  }
}

/** 切换启用状态：停用后该规则不再参与匹配（未命中任何规则的日志不会产生告警）。 */
async function toggle(rule: LogAlertRule): Promise<void> {
  try {
    await logAlertApi.updateRule(rule.id, { ...toPayload(rule), enabled: !rule.enabled })
    await list.load()
  } catch (error) {
    toastError(error)
  }
}
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2 class="page-title">日志告警规则</h2>
        <p class="page-subtitle">
          按服务 + 错误指纹 + 级别匹配日志事件，命中后按该规则的去重窗口合并计数、冷却期内静默，并可自动做 AI 代码分析
        </p>
      </div>
      <el-button v-if="canWrite" type="primary" :icon="'Plus'" @click="openForm()">新建规则</el-button>
    </div>

    <el-alert
      type="info"
      :closable="false"
      show-icon
      class="page-alert"
      title="平台没有内置默认规则：日志必须命中下方某条规则才会产生告警（未命中的日志不入库、不通知、不分析）"
    />

    <!-- 屏蔽规则：先讲清"哪些错误根本不该告警"，再看规则列表 -->
    <div v-loading="exclLoading" class="card exclusion-card">
      <h3 class="card-title">
        <span>屏蔽规则（命中的日志完全不告警）</span>
        <div class="row">
          <el-button v-if="exclError" size="small" text @click="loadExclusions">重试</el-button>
          <el-button v-if="canWrite" size="small" type="primary" :icon="'Plus'" @click="openExclusion()">
            新增屏蔽
          </el-button>
        </div>
      </h3>
      <p class="field-hint">
        屏蔽的是**日志原文**：填普通文本按子串匹配，填 <span class="mono">/正则/</span> 按正则匹配；
        服务名留空表示对所有服务生效。支持多条，命中第一条即生效——例如屏蔽框架噪音
        <span class="mono">Request method 'GET' is not supported</span>。
      </p>
      <div class="table-scroll">
        <el-table :data="exclusions" size="small">
          <el-table-column label="屏蔽内容" min-width="240">
            <template #default="{ row }"><span class="mono">{{ row.pattern }}</span></template>
          </el-table-column>
          <el-table-column label="服务" min-width="130">
            <template #default="{ row }">
              <span v-if="row.service_name" class="mono">{{ row.service_name }}</span>
              <span v-else class="muted">任意服务</span>
            </template>
          </el-table-column>
          <el-table-column label="备注" min-width="150" show-overflow-tooltip>
            <template #default="{ row }">{{ row.name || '—' }}</template>
          </el-table-column>
          <el-table-column label="状态" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.enabled ? 'success' : 'info'" effect="light">
                {{ row.enabled ? '生效中' : '已停用' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column v-if="canWrite" label="操作" width="150" fixed="right">
            <template #default="{ row }">
              <el-switch size="small" :model-value="row.enabled" @change="toggleExclusion(row)" />
              <el-button text size="small" @click="openExclusion(row)">编辑</el-button>
              <el-button text size="small" type="danger" @click="removeExclusion(row)">删除</el-button>
            </template>
          </el-table-column>
          <template #empty>
            <!-- 读不到与"确实没配"必须分开说（INC-016） -->
            <span v-if="exclError">无数据：无法获取屏蔽规则（{{ exclError }}）</span>
            <span v-else>暂无屏蔽项：所有命中的日志都会按下方规则告警</span>
          </template>
        </el-table>
      </div>
      <p class="field-hint">
        屏蔽优先于所有规则：被屏蔽的日志连事件都不落库（不会出现在日志事件列表，也不会通知与 AI 分析）；
        停用或删除后同类错误会重新开始告警。
      </p>
    </div>

    <ResponsiveList
      :items="items"
      :loading="loading"
      :error="error"
      :total="total"
      :page="query.page"
      :page-size="query.page_size"
      empty-text="没有匹配的规则"
      @update:page="list.setPage"
      @update:page-size="list.setPageSize"
      @retry="list.load"
    >
      <template #filters>
        <el-input
          v-model="query.keyword"
          placeholder="搜索规则名 / 服务 / 指纹"
          clearable
          class="filter-item"
          @keyup.enter="list.search"
        />
        <el-button type="primary" :icon="'Search'" @click="list.search">查询</el-button>
        <el-button :icon="'RefreshLeft'" @click="list.reset">重置</el-button>
      </template>

      <!-- 移动端：卡片 -->
      <template #card="{ row }">
        <div class="rule-head">
          <span class="rule-name">{{ row.name }}</span>
          <el-tag size="small" :type="row.enabled ? 'success' : 'info'" effect="light">
            {{ row.enabled ? '已启用' : '已停用' }}
          </el-tag>
        </div>
        <p class="rule-cond mono">
          {{ row.service_name || '任意服务' }} · {{ row.signature_pattern || '任意消息' }}
        </p>
        <div class="rule-meta muted">
          <span>级别 {{ row.min_severity || '不限' }}</span>
          <span>窗口 {{ row.dedup_window }} 分钟 · 冷却 {{ row.cooldown }} 分钟</span>
          <span>优先级 {{ row.priority }}</span>
        </div>
        <div class="rule-meta muted">
          <span>通知渠道：{{ joinList(row.notify_channels) || '平台默认' }}</span>
          <span>AI 分析：{{ row.ai_enabled ? '开启' : '关闭' }}</span>
        </div>
        <p v-if="row.description" class="rule-desc muted">{{ row.description }}</p>
        <div class="rule-actions">
          <el-switch v-if="canWrite" size="small" :model-value="row.enabled" @change="toggle(row)" />
          <el-button v-if="canWrite" size="small" @click="openForm(row)">编辑</el-button>
          <el-button v-if="canWrite" size="small" type="danger" @click="remove(row)">删除</el-button>
        </div>
      </template>

      <!-- 桌面端：表格 -->
      <template #table>
        <div class="table-scroll">
          <el-table :data="items" size="default">
            <el-table-column prop="name" label="规则名称" min-width="150" show-overflow-tooltip />
            <el-table-column label="服务" min-width="130">
              <template #default="{ row }">
                <span v-if="row.service_name" class="mono">{{ row.service_name }}</span>
                <span v-else class="muted">任意服务</span>
              </template>
            </el-table-column>
            <el-table-column label="消息匹配" min-width="170">
              <template #default="{ row }">
                <span v-if="row.signature_pattern" class="mono">{{ row.signature_pattern }}</span>
                <span v-else class="muted">任意</span>
              </template>
            </el-table-column>
            <el-table-column label="最低级别" width="100">
              <template #default="{ row }">
                <span v-if="row.min_severity">{{ row.min_severity }}</span>
                <span v-else class="muted">不限</span>
              </template>
            </el-table-column>
            <el-table-column label="收敛策略" width="180">
              <template #default="{ row }">窗口 {{ row.dedup_window }} 分钟 · 冷却 {{ row.cooldown }} 分钟</template>
            </el-table-column>
            <el-table-column label="通知渠道" min-width="140">
              <template #default="{ row }">
                <el-tag v-for="ch in row.notify_channels || []" :key="ch" size="small" effect="plain" class="mini-tag">
                  {{ ch }}
                </el-tag>
                <span v-if="!(row.notify_channels || []).length" class="muted">平台默认</span>
              </template>
            </el-table-column>
            <el-table-column label="AI 分析" width="100">
              <template #default="{ row }">
                <el-tag size="small" :type="row.ai_enabled ? 'success' : 'info'" effect="light">
                  {{ row.ai_enabled ? '自动分析' : '不分析' }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column label="优先级" width="80">
              <template #default="{ row }">{{ row.priority }}</template>
            </el-table-column>
            <el-table-column label="启用" width="80">
              <template #default="{ row }">
                <el-switch :model-value="row.enabled" :disabled="!canWrite" @change="toggle(row)" />
              </template>
            </el-table-column>
            <el-table-column label="操作" width="130" fixed="right">
              <template #default="{ row }">
                <el-button v-if="canWrite" text size="small" @click="openForm(row)">编辑</el-button>
                <el-button v-if="canWrite" text size="small" type="danger" @click="remove(row)">删除</el-button>
              </template>
            </el-table-column>
          </el-table>
        </div>
      </template>
    </ResponsiveList>

    <el-dialog v-model="dialogVisible" :title="editing ? '编辑日志告警规则' : '新建日志告警规则'" width="640px">
      <el-form ref="formRef" :model="form" :rules="rules" label-position="top">
        <el-row :gutter="12">
          <el-col :xs="24" :sm="12">
            <el-form-item label="规则名称" prop="name">
              <el-input v-model="form.name" placeholder="如 订单服务空指针告警" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="备注">
              <el-input v-model="form.description" placeholder="补充该规则的处置口径" />
            </el-form-item>
          </el-col>

          <el-col :xs="24" :sm="12">
            <el-form-item label="服务名">
              <el-input v-model="form.service_name" placeholder="留空表示任意服务" />
              <p class="field-hint">只对某服务生效；留空表示任意服务。</p>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="最低级别">
              <el-select v-model="form.min_severity" clearable placeholder="不限级别" class="mobile-block">
                <el-option v-for="item in SEVERITY_OPTIONS" :key="item" :label="item" :value="item" />
              </el-select>
              <p class="field-hint">只处理不低于该级别的事件；留空表示不限级别。</p>
            </el-form-item>
          </el-col>

          <el-col :span="24">
            <el-form-item label="日志消息匹配" prop="signature_pattern">
              <el-input v-model="form.signature_pattern" placeholder="留空表示任意消息" />
              <p class="field-hint">
                匹配的是<b>日志里的原文</b>（上报的 message），不是错误指纹——指纹是哈希，照着日志填文本即可。
                填普通文本=子串匹配，例如 <span class="mono">NullPointerException</span>；填
                <span class="mono">/正则/</span>=按正则匹配，例如
                <span class="mono">/timeout|refused/</span>；留空表示任意消息。
              </p>
            </el-form-item>
          </el-col>

          <el-col :xs="24" :sm="12">
            <el-form-item label="去重窗口（分钟）" prop="dedup_window">
              <el-input-number v-model="form.dedup_window" :min="0" :max="1440" class="mobile-block" />
              <p class="field-hint">窗口内同一指纹只合并计数、不重复通知；0 表示不去重。</p>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="冷却期（分钟）" prop="cooldown">
              <el-input-number v-model="form.cooldown" :min="0" :max="10080" class="mobile-block" />
              <p class="field-hint">冷却期内同指纹不再通知、不再触发 AI（事件仍记录）；0 表示不冷却。</p>
            </el-form-item>
          </el-col>

          <el-col :span="24">
            <el-form-item label="通知渠道">
              <el-checkbox-group v-model="form.notify_channels">
                <el-checkbox v-for="item in CHANNEL_OPTIONS" :key="item" :value="item">{{ item }}</el-checkbox>
              </el-checkbox-group>
              <p class="field-hint">留空表示使用平台「通知渠道」里已启用的渠道。</p>
            </el-form-item>
          </el-col>

          <el-col :xs="24" :sm="12">
            <el-form-item label="优先级" prop="priority">
              <el-input-number v-model="form.priority" :min="1" :max="9999" class="mobile-block" />
              <p class="field-hint">数字小的优先；多条命中时取第一条。</p>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="开关">
              <div class="row switch-row">
                <el-switch v-model="form.ai_enabled" />
                <span class="muted">自动做 AI 代码分析</span>
              </div>
              <div class="row switch-row">
                <el-switch v-model="form.enabled" />
                <span class="muted">启用规则</span>
              </div>
              <p class="field-hint">AI 分析需要该服务已在「服务器与仓库」里配置代码仓库映射。</p>
            </el-form-item>
          </el-col>
        </el-row>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="submit">保存</el-button>
      </template>
    </el-dialog>

    <el-dialog
      v-model="exclDialogVisible"
      :title="exclEditing ? '编辑屏蔽项' : '新增屏蔽项'"
      width="560px"
    >
      <el-form ref="exclFormRef" :model="exclForm" :rules="exclRules" label-position="top">
        <el-form-item label="屏蔽内容（必填）" prop="pattern">
          <el-input
            v-model="exclForm.pattern"
            placeholder="如 Request method 'GET' is not supported"
          />
          <p class="field-hint">
            匹配日志原文：填普通文本=子串匹配，填 <span class="mono">/正则/</span>=按正则匹配，例如
            <span class="mono">/Request method .* is not supported/i</span>。命中的日志不入库、不通知、不分析。
          </p>
        </el-form-item>
        <el-row :gutter="12">
          <el-col :xs="24" :sm="12">
            <el-form-item label="服务名">
              <el-input v-model="exclForm.service_name" placeholder="留空表示任意服务" />
              <p class="field-hint">只对该服务生效；留空表示所有服务。</p>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="备注">
              <el-input v-model="exclForm.name" placeholder="如 框架噪音，不是故障" />
              <p class="field-hint">说明为什么屏蔽，方便其他人看懂。</p>
            </el-form-item>
          </el-col>
        </el-row>
        <el-form-item label="开关">
          <div class="row switch-row">
            <el-switch v-model="exclForm.enabled" />
            <span class="muted">启用该屏蔽项</span>
          </div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="exclDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="exclSubmitting" @click="submitExclusion">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.page-alert {
  margin-bottom: 12px;
}

.exclusion-card {
  margin-bottom: 12px;
}

.card-title .row {
  gap: 8px;
}

.field-hint {
  margin: 2px 0 0;
  font-size: 11.5px;
  line-height: 1.6;
}

.filter-item {
  width: 240px;
}

.mini-tag {
  margin-right: 4px;
}

.switch-row {
  gap: 8px;
  font-size: 12.5px;
}

/* 移动端卡片：规则条目 */
.rule-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.rule-name {
  font-size: 13.5px;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.rule-cond {
  margin: 8px 0 0;
  font-size: 12.5px;
  word-break: break-all;
}

.rule-meta {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
  margin: 4px 0 0;
  font-size: 11.5px;
}

.rule-desc {
  margin: 6px 0 0;
  font-size: 11.5px;
  line-height: 1.6;
}

.rule-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 8px;
}
</style>
