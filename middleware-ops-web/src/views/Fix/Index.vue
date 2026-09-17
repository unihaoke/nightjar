<script setup lang="ts">
/**
 * 修复执行（4.6 + 6.2）。
 *
 * 执行链路：预演（影响预览）→ 审批（prod 强制，30min 超时自动拒绝）→ 执行 → 结果回填复核。
 * L2 高危动作在平台内创建审批工单；当前执行器为预演实现，不会对被管中间件产生副作用。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { fixApi, middlewareApi } from '@/api'
import { toastError } from '@/api/http'
import type { FixPreview, FixRecord, MiddlewareInstance } from '@/api/types'
import { useListPage } from '@/composables/useListPage'
import ResponsiveList from '@/components/ResponsiveList.vue'
import LevelTag from '@/components/LevelTag.vue'
import { approvalStatusLabels, envLabels, formatTime, levelLabels, mwTypeLabels, prettyJSON } from '@/utils/format'

const previewing = ref(false)
const executing = ref(false)
const metaLoading = ref(false)
const instances = ref<MiddlewareInstance[]>([])
const preview = ref<FixPreview | null>(null)
const actions = ref<{ action: string; label: string; level: string; desc: string }[]>([])
const levels = ref<{ level: string; label: string; desc: string }[]>([])
const executor = ref('')
const sqlGuard = ref<{ default_limit: number; max_limit: number; allowlist: string[] } | null>(null)

const route = useRoute()
const router = useRouter()

/** 来源上下文（诊断/告警带入；仅用于预选实例与展示，便于回溯链路）。 */
const alertId = ref<number>(Number(route.query.alert_id) || 0)
const diagnosisId = ref<number>(Number(route.query.diagnosis_id) || 0)

/** 执行表单（不是列表筛选，因此不进地址栏）。 */
const query = reactive({
  instance_id: 0,
  action_type: 'view_metrics',
  params: {} as Record<string, unknown>,
  reason: '',
})

// 执行历史：只有分页进地址栏，表单状态不进。
const list = useListPage<FixRecord>({
  fetch: (params, signal) =>
    fixApi.history({ page: Number(params.page), page_size: Number(params.page_size) }, signal),
  defaults: { page: 1, page_size: 20 },
})
const { items: history, total, loading, error } = list

/** SQL 输入（用于只读校验与预览）。 */
const sqlText = ref('')

/** 当前动作定义。 */
const currentAction = computed(() => actions.value.find((item) => item.action === query.action_type) || null)

/** 当前动作级别。 */
const currentLevel = computed(() => currentAction.value?.level || 'L2')

/** 是否为 A2 高危。 */
const requiresApproval = computed(() => currentLevel.value === 'L2')

/** 加载实例清单与动作字典（与执行历史分页无关，只需一次）。 */
async function loadMeta(): Promise<void> {
  metaLoading.value = true
  try {
    const [options, fixOptions] = await Promise.all([
      middlewareApi.list({ page: 1, page_size: 100 }),
      fixApi.options(),
    ])
    instances.value = options.list || []
    actions.value = fixOptions.actions || []
    levels.value = fixOptions.levels || []
    executor.value = fixOptions.executor || ''
    sqlGuard.value = fixOptions.sql_guard || null
    const preselect = Number(route.query.instance_id) || 0
    if (preselect) {
      query.instance_id = preselect
    } else if (!query.instance_id && instances.value.length > 0) {
      query.instance_id = instances.value[0].id
    }
  } catch (error) {
    toastError(error)
  } finally {
    metaLoading.value = false
  }
}

/** 参数构造：根据动作类型生成 params。 */
function buildParams(): Record<string, unknown> {
  const params: Record<string, unknown> = {}
  if (query.action_type === 'run_readonly_sql' || query.action_type === 'sql_write') {
    if (sqlText.value.trim()) {
      params.sql = sqlText.value.trim()
    }
  }
  if (query.action_type === 'clean_redis_key') {
    params.command = 'DEL <key>'
  }
  if (query.action_type === 'restart_service') {
    params.command = 'systemctl restart <service>'
  }
  return params
}

/** 预览（L0）。 */
async function doPreview(): Promise<void> {
  if (!query.instance_id) {
    ElMessage({ type: 'warning', message: '请先选择实例' })
    return
  }
  previewing.value = true
  try {
    preview.value = await fixApi.preview({
      instance_id: query.instance_id,
      action_type: query.action_type,
      params: buildParams(),
      reason: query.reason,
    })
  } catch (error) {
    toastError(error)
    preview.value = null
  } finally {
    previewing.value = false
  }
}

/** 执行（L1 直接执行；L2 创建审批工单）。 */
async function doExecute(dryRun = false): Promise<void> {
  if (!query.instance_id) {
    ElMessage({ type: 'warning', message: '请先选择实例' })
    return
  }
  const required = requiresApproval.value && !dryRun
  try {
    await ElMessageBox.confirm(
      required
        ? `该操作属于 ${levelLabels[currentLevel.value]}，将提交审批工单（30 分钟未审批自动拒绝）。确认继续？`
        : dryRun
          ? '将执行预演，不产生任何副作用。确认继续？'
          : '确认执行该操作？执行结果将写入审计日志。',
      required ? '提交审批' : dryRun ? '预演确认' : '执行确认',
      { confirmButtonText: '确认', cancelButtonText: '取消', type: required ? 'warning' : 'info' },
    )
  } catch {
    return
  }
  executing.value = true
  try {
    const result = await fixApi.execute({
      instance_id: query.instance_id,
      action_type: query.action_type,
      params: buildParams(),
      reason: query.reason,
      dry_run: dryRun,
      alert_id: alertId.value || undefined,
      diagnosis_id: diagnosisId.value || undefined,
    })
    if (result.ticket_id) {
      ElMessage({ type: 'success', message: `${result.message}` })
      // L2 高危：已生成审批工单，带上下文跳审批（闭合 诊断 → 修复 → 审批 链路）。
      if (requiresApproval.value && !dryRun) {
        const q: Record<string, string> = { focus: result.ticket_id }
        if (alertId.value) {
          q.alert_id = String(alertId.value)
        }
        if (diagnosisId.value) {
          q.diagnosis_id = String(diagnosisId.value)
        }
        window.setTimeout(() => void router.push({ name: 'approvals', query: q }), 700)
        return
      }
    } else {
      ElMessage({ type: result.status === 'failed' ? 'error' : 'success', message: result.message })
    }
    await list.load()
  } catch (error) {
    toastError(error)
  } finally {
    executing.value = false
  }
}

/** 校验 SQL 只读安全性。 */
async function validateSql(): Promise<void> {
  if (!sqlText.value.trim()) {
    ElMessage({ type: 'warning', message: '请先输入 SQL' })
    return
  }
  try {
    const result = await fixApi.validateSql(sqlText.value)
    ElMessage({
      type: result.high_risk ? 'warning' : 'success',
      message: result.high_risk
        ? `高危语句：${result.high_risk_reason}`
        : `校验通过，规范化后：${result.normalized_sql}`,
    })
  } catch (error) {
    toastError(error)
  }
}

onMounted(() => {
  void loadMeta()
})
</script>

<template>
  <div class="page" v-loading="metaLoading">
    <el-alert
      v-if="alertId || diagnosisId"
      type="info"
      :closable="false"
      show-icon
      class="source-banner"
    >
      <template #title>
        本次修复关联来源：
        <el-button v-if="alertId" link type="primary" size="small" @click="router.push({ name: 'alerts' })">
          告警 #{{ alertId }}
        </el-button>
        <template v-if="alertId && diagnosisId"> · </template>
        <span v-if="diagnosisId">诊断 #{{ diagnosisId }}</span>
      </template>
    </el-alert>

    <div class="page-header">
      <div>
        <h2 class="page-title">修复执行</h2>
        <p class="page-subtitle">
          操作分级：L0 直接执行 · L1 直接执行并留痕 · L2 二次确认 + 审批（生产环境强制）
        </p>
      </div>
      <el-tag size="small" effect="light" type="info">执行器：{{ executor || '-' }}</el-tag>
    </div>

    <el-row :gutter="12">
      <el-col :xs="24" :md="14">
        <div class="card">
          <h3 class="card-title">执行工单</h3>
          <el-form label-position="top">
            <el-row :gutter="12">
              <el-col :xs="24" :sm="12">
                <el-form-item label="目标实例">
                  <el-select v-model="query.instance_id" filterable class="mobile-block">
                    <el-option
                      v-for="item in instances"
                      :key="item.id"
                      :label="`${item.name}（${mwTypeLabels[item.mw_type] || item.mw_type} · ${envLabels[item.environment]}）`"
                      :value="item.id"
                    />
                  </el-select>
                </el-form-item>
              </el-col>
              <el-col :xs="24" :sm="12">
                <el-form-item label="操作类型">
                  <el-select v-model="query.action_type" class="mobile-block" @change="preview = null">
                    <el-option v-for="item in actions" :key="item.action" :label="`${item.label}（${item.level}）`" :value="item.action" />
                  </el-select>
                </el-form-item>
              </el-col>
            </el-row>

            <div v-if="currentAction" class="action-desc">
              <LevelTag :level="currentLevel" show-desc />
              <span class="muted">{{ currentAction.desc }}</span>
            </div>

            <el-form-item
              v-if="query.action_type === 'run_readonly_sql' || query.action_type === 'sql_write'"
              label="SQL 语句（强制只读校验 + 强制 LIMIT）"
            >
              <el-input v-model="sqlText" type="textarea" :rows="4" placeholder="SELECT * FROM orders WHERE status = 1" />
              <div class="row sql-actions">
                <el-button size="small" @click="validateSql">只读安全校验</el-button>
                <span v-if="sqlGuard" class="muted hint">
                  默认 LIMIT {{ sqlGuard.default_limit }}，上限 {{ sqlGuard.max_limit }}；禁止多语句与注释
                </span>
              </div>
            </el-form-item>

            <el-form-item :label="requiresApproval ? '申请理由（生产环境必填）' : '备注（可选）'">
              <el-input v-model="query.reason" type="textarea" :rows="2" placeholder="说明本次操作的背景与影响评估" />
            </el-form-item>

            <div class="row">
              <el-button :loading="previewing" :icon="'View'" @click="doPreview">生成影响预览</el-button>
              <el-button :loading="executing" @click="doExecute(true)">仅预演</el-button>
              <el-button type="primary" :loading="executing" :icon="requiresApproval ? 'Stamp' : 'Tools'" @click="doExecute(false)">
                {{ requiresApproval ? '提交审批' : '执行' }}
              </el-button>
            </div>
          </el-form>

          <!-- 影响预览 -->
          <div v-if="preview" class="preview">
            <h4 class="section">影响预览</h4>
            <div class="preview-grid">
              <div class="kv">
                <span>操作级别</span>
                <LevelTag :level="preview.level" />
              </div>
              <div class="kv">
                <span>目标环境</span>
                <el-tag size="small" effect="plain">{{ envLabels[preview.environment] || preview.environment }}</el-tag>
              </div>
              <div class="kv">
                <span>是否需要审批</span>
                <el-tag size="small" :type="preview.requires_approval ? 'warning' : 'success'">
                  {{ preview.requires_approval ? '需要（30 分钟超时自动拒绝）' : '不需要' }}
                </el-tag>
              </div>
              <div class="kv">
                <span>高危判定</span>
                <el-tag size="small" :type="preview.high_risk ? 'danger' : 'info'">
                  {{ preview.high_risk ? preview.high_risk_reason : '未识别到高危特征' }}
                </el-tag>
              </div>
            </div>
            <el-alert
              v-for="(warning, index) in preview.warnings || []"
              :key="index"
              type="warning"
              :closable="false"
              show-icon
              :title="warning"
              class="mt-sm"
            />
            <pre v-if="preview.normalized_sql" class="code-block">{{ preview.normalized_sql }}</pre>
            <pre class="code-block">{{ prettyJSON(preview.impact) }}</pre>
          </div>
        </div>
      </el-col>

      <el-col :xs="24" :md="10">
        <div class="card">
          <h3 class="card-title">分级说明</h3>
          <div v-for="item in levels" :key="item.level" class="level-row">
            <LevelTag :level="item.level" />
            <span class="muted">{{ item.desc }}</span>
          </div>
          <el-divider />
          <h3 class="card-title">边界约定</h3>
          <ul class="notice-list">
            <li>IM 卡片只做通知 + 确认/驳回 + 查看详情，<strong>不做一键执行</strong>。</li>
            <li>所有执行统一回 Web 端，可叠加二次确认，避免 IM 上下文身份不可信。</li>
            <li>SQL 查询强制只读账号 + 强制 LIMIT + 敏感列脱敏 + 独立审计。</li>
          </ul>
        </div>
      </el-col>
    </el-row>

    <ResponsiveList
      :items="history"
      :loading="loading"
      :error="error"
      :total="total"
      :page="list.query.page"
      :page-size="list.query.page_size"
      title="执行历史"
      empty-text="暂无执行记录"
      @update:page="list.setPage"
      @update:page-size="list.setPageSize"
      @retry="list.load"
    >
      <template #card="{ row }">
        <div class="history-head">
          <LevelTag :level="row.level" />
          <span class="mono">{{ row.action_type }}</span>
          <el-tag size="small" :type="row.status === 'success' ? 'success' : row.status === 'dry_run' ? 'info' : 'danger'" effect="light">
            {{ approvalStatusLabels[row.status] || row.status }}
          </el-tag>
        </div>
        <p class="history-meta muted">
          {{ formatTime(row.created_at) }} · 实例 #{{ row.instance_id }} · {{ row.duration_ms }} ms
        </p>
        <p class="history-command mono">{{ row.command || '-' }}</p>
        <p v-if="row.ticket_id" class="history-meta muted">工单 {{ row.ticket_id }}</p>
      </template>

      <template #table>
        <div class="table-scroll">
        <el-table :data="history" size="small">
          <el-table-column label="时间" width="170">
            <template #default="{ row }">{{ formatTime(row.created_at) }}</template>
          </el-table-column>
          <el-table-column prop="action_type" label="动作" min-width="150" show-overflow-tooltip />
          <el-table-column label="级别" width="110">
            <template #default="{ row }">
              <LevelTag :level="row.level" />
            </template>
          </el-table-column>
          <el-table-column label="实例" width="90">
            <template #default="{ row }">#{{ row.instance_id }}</template>
          </el-table-column>
          <el-table-column label="结果" width="110">
            <template #default="{ row }">
              <el-tag size="small" :type="row.status === 'success' ? 'success' : row.status === 'dry_run' ? 'info' : 'danger'" effect="light">
                {{ approvalStatusLabels[row.status] || row.status }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="command" label="命令/语句" min-width="200" show-overflow-tooltip />
          <el-table-column label="工单" width="150">
            <template #default="{ row }">
              <span class="mono">{{ row.ticket_id || '-' }}</span>
            </template>
          </el-table-column>
          <el-table-column label="耗时" width="90">
            <template #default="{ row }">{{ row.duration_ms }} ms</template>
          </el-table-column>
        </el-table>
        </div>
      </template>
    </ResponsiveList>
  </div>
</template>

<style scoped>
.card + .card {
  margin-top: 12px;
}

.source-banner {
  margin-bottom: 12px;
}

.source-banner :deep(.el-alert__content) {
  font-size: 12.5px;
}

.action-desc {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  font-size: 12.5px;
  margin-bottom: 12px;
}

.sql-actions {
  margin-top: 8px;
}

.hint {
  font-size: 11.5px;
}

.preview {
  margin-top: 12px;
  border-top: 1px solid var(--c-border);
  padding-top: 12px;
}

.section {
  margin: 0 0 10px;
  font-size: 13px;
  font-weight: 600;
}

.preview-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
  gap: 10px;
}

.kv {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  font-size: 12.5px;
  padding: 8px 10px;
  background: var(--c-surface-2);
  border-radius: var(--r-md);
}

.mt-sm {
  margin-top: 8px;
}

.code-block {
  margin: 8px 0 0;
  padding: 10px 12px;
  background: var(--c-surface-2);
  border-radius: var(--r-md);
  font-size: 12px;
  line-height: 1.6;
  max-height: 240px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
}

.level-row {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 12.5px;
  padding: 6px 0;
}

.notice-list {
  margin: 0;
  padding-left: 18px;
  font-size: 12.5px;
  color: var(--c-text-2);
  line-height: 1.8;
}

/* 移动端卡片：执行历史条目 */
.history-head {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.history-meta {
  margin: 8px 0 0;
  font-size: 12px;
}

.history-command {
  margin: 4px 0 0;
  font-size: 12px;
  word-break: break-all;
}
</style>
