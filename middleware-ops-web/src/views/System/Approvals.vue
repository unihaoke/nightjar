<script setup lang="ts">
/** 审批管理（6.2）：L2 高危操作工单，prod 强制审批，30 分钟超时自动拒绝。 */
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { alertApi, approvalApi } from '@/api'
import { toastError } from '@/api/http'
import type { Approval } from '@/api/types'
import { useListPage } from '@/composables/useListPage'
import RefreshControl from '@/components/RefreshControl.vue'
import ResponsiveList from '@/components/ResponsiveList.vue'
import { approvalStatusLabels, envLabels, envTagType, formatTime, prettyJSON } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const store = useUserStore()
const route = useRoute()
const router = useRouter()

/** 跨页带入的上下文：focus=修复工单创建的审批单号。 */
const focusTicket = route.query.focus ? String(route.query.focus) : ''
const focused = ref(false)

const detailVisible = ref(false)
const current = ref<Approval | null>(null)

/** 来源上下文：优先取审批单持久化的关联（alert_id / diagnosis_id），其次回退到跨页 URL 参数。 */
const sourceAlertId = computed<number>(
  () => Number(current.value?.alert_id) || Number(route.query.alert_id) || 0,
)
const sourceDiagnosisId = computed<number>(
  () => Number(current.value?.diagnosis_id) || Number(route.query.diagnosis_id) || 0,
)

/** 工单 30 分钟超时自动拒绝，时效性最强，默认开启自动刷新。 */
const POLL_INTERVAL = 60_000

const list = useListPage<Approval>({
  fetch: (params, signal) => approvalApi.list(params, signal),
  defaults: { status: 'pending', mine: false, page: 1, page_size: 20 },
  booleanKeys: ['mine'],
  pollInterval: POLL_INTERVAL,
})
const { query, items, total, loading, error, polling, lastLoadedAt } = list

/** 进入审批页若带 focus（来自修复工单），自动展开对应工单详情。 */
watch(
  items,
  (rows) => {
    if (focused.value || !focusTicket) {
      return
    }
    const hit = rows.find((row) => row.ticket_id === focusTicket)
    if (hit) {
      focused.value = true
      void openDetail(hit)
    }
  },
  { immediate: true },
)

const canDecide = computed(() => store.can('approval:decide'))

/** 查看详情。 */
async function openDetail(row: Approval): Promise<void> {
  detailVisible.value = true
  try {
    current.value = await approvalApi.detail(row.id)
  } catch (error) {
    current.value = row
    toastError(error)
  }
}

/** 审批决策。 */
async function decide(approved: boolean): Promise<void> {
  if (!current.value) {
    return
  }
  let comment = ''
  try {
    const result = await ElMessageBox.prompt(
      approved ? '确认通过该高危操作工单？' : '请填写驳回原因：',
      approved ? '审批通过' : '审批驳回',
      {
        confirmButtonText: approved ? '通过' : '驳回',
        cancelButtonText: '取消',
        inputPlaceholder: approved ? '审批意见（可选）' : '驳回原因（必填）',
        inputValidator: (value) => (approved || value ? true : '驳回必须填写原因'),
        type: approved ? 'info' : 'warning',
      },
    )
    comment = result.value || ''
  } catch {
    return
  }
  try {
    await approvalApi.decide(current.value.id, approved, comment)
    ElMessage({ type: 'success', message: approved ? '工单已通过' : '工单已驳回' })
    detailVisible.value = false
    await list.load()
  } catch (error) {
    toastError(error)
  }
}

/** 剩余有效期。 */
function remaining(expiresAt: string): string {
  const diff = new Date(expiresAt).getTime() - Date.now()
  if (diff <= 0) {
    return '已超时'
  }
  const minutes = Math.floor(diff / 60000)
  return `${minutes} 分钟后超时`
}

/** 回填来源告警（闭环：审批通过后标记来源告警已解决）。 */
async function backfill(): Promise<void> {
  if (!sourceAlertId.value) {
    return
  }
  try {
    await alertApi.resolve(sourceAlertId.value)
    ElMessage({ type: 'success', message: `已回填：来源告警 #${sourceAlertId.value} 标记为已解决` })
  } catch (error) {
    toastError(error)
  }
}
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2 class="page-title">审批管理</h2>
        <p class="page-subtitle">
          L2 高危操作链路：预演 → 审批（生产环境强制）→ 执行 → 结果回填复核；申请人与审批人不得为同一人
        </p>
      </div>
      <el-checkbox v-model="query.mine" @change="list.search">只看我提交的</el-checkbox>
    </div>

    <div class="row toolbar-row">
      <RefreshControl
        v-model="polling"
        :interval-ms="POLL_INTERVAL"
        :last-loaded-at="lastLoadedAt"
        @refresh="list.load()"
      />
    </div>

    <ResponsiveList
      :items="items"
      :loading="loading"
      :error="error"
      :total="total"
      :page="query.page"
      :page-size="query.page_size"
      empty-text="没有匹配的审批工单"
      @update:page="list.setPage"
      @update:page-size="list.setPageSize"
      @retry="list.load"
    >
      <template #filters>
        <el-select v-model="query.status" placeholder="全部状态" clearable class="filter-item" @change="list.search">
          <el-option label="待审批" value="pending" />
          <el-option label="已通过" value="approved" />
          <el-option label="已驳回" value="rejected" />
          <el-option label="已超时" value="expired" />
          <el-option label="已执行" value="success" />
          <el-option label="执行失败" value="failed" />
        </el-select>
        <el-button type="primary" :icon="'Search'" @click="list.search">查询</el-button>
        <el-button :icon="'RefreshLeft'" @click="list.reset">重置</el-button>
      </template>

      <!-- 移动端：卡片 -->
      <template #card="{ row }">
        <div class="ticket-head">
          <code class="ticket-id">{{ row.ticket_id }}</code>
          <el-tag size="small" effect="light" :type="row.status === 'pending' ? 'warning' : row.status === 'approved' || row.status === 'success' ? 'success' : row.status === 'rejected' || row.status === 'failed' ? 'danger' : 'info'">
            {{ approvalStatusLabels[row.status] || row.status }}
          </el-tag>
          <el-tag v-if="row.status === 'pending'" size="small" type="danger" effect="plain">
            {{ remaining(row.expires_at) }}
          </el-tag>
        </div>
        <p class="ticket-action">{{ row.action_type }}</p>
        <p class="ticket-reason muted">理由：{{ row.reason || '（未填写）' }}</p>
        <div class="ticket-meta">
          <span class="muted">申请人 #{{ row.applicant_id }}</span>
          <span class="muted">实例 #{{ row.instance_id }}</span>
          <span class="muted">{{ formatTime(row.created_at) }}</span>
        </div>
        <div class="ticket-actions is-card">
          <el-button size="small" @click="openDetail(row)">详情</el-button>
        </div>
      </template>

      <!-- 桌面端：列表 -->
      <template #table>
        <ul class="ticket-list">
        <li v-for="item in items" :key="item.id" :class="item.status">
          <div class="ticket-main">
            <div class="ticket-head">
              <code class="ticket-id">{{ item.ticket_id }}</code>
              <el-tag size="small" effect="plain" :type="envTagType[item.environment]">
                {{ envLabels[item.environment] || item.environment }}
              </el-tag>
              <el-tag size="small" effect="light" :type="item.status === 'pending' ? 'warning' : item.status === 'approved' || item.status === 'success' ? 'success' : item.status === 'rejected' || item.status === 'failed' ? 'danger' : 'info'">
                {{ approvalStatusLabels[item.status] || item.status }}
              </el-tag>
              <el-tag v-if="item.status === 'pending'" size="small" type="danger" effect="plain">
                {{ remaining(item.expires_at) }}
              </el-tag>
            </div>
            <p class="ticket-action">{{ item.action_type }}</p>
            <p class="ticket-reason muted">理由：{{ item.reason || '（未填写）' }}</p>
            <div class="ticket-meta">
              <span class="muted">申请人 #{{ item.applicant_id }}</span>
              <span class="muted">实例 #{{ item.instance_id }}</span>
              <span class="muted">提交 {{ formatTime(item.created_at) }}</span>
              <span v-if="item.approver_id" class="muted">审批人 #{{ item.approver_id }}</span>
              <span v-if="item.comment" class="muted">意见：{{ item.comment }}</span>
            </div>
          </div>
          <div class="ticket-actions">
            <el-button text size="small" @click="openDetail(item)">详情</el-button>
          </div>
        </li>
        </ul>
      </template>
    </ResponsiveList>

    <el-drawer v-model="detailVisible" title="工单详情" size="620px" direction="rtl">
      <div v-if="current" class="stack">
        <div class="kv"><span>工单号</span><code>{{ current.ticket_id }}</code></div>
        <div v-if="sourceAlertId || sourceDiagnosisId" class="kv">
          <span>来源</span>
          <span>
            <template v-if="sourceAlertId">告警 #{{ sourceAlertId }}</template>
            <template v-if="sourceAlertId && sourceDiagnosisId"> · </template>
            <template v-if="sourceDiagnosisId">诊断 #{{ sourceDiagnosisId }}</template>
          </span>
        </div>
        <div class="kv"><span>状态</span><span>{{ approvalStatusLabels[current.status] || current.status }}</span></div>
        <div class="kv"><span>环境</span><span>{{ envLabels[current.environment] || current.environment }}</span></div>
        <div class="kv"><span>动作</span><span class="mono">{{ current.action_type }}</span></div>
        <div class="kv"><span>实例</span><span>#{{ current.instance_id }}</span></div>
        <div class="kv"><span>申请理由</span><span>{{ current.reason || '（未填写）' }}</span></div>
        <div class="kv"><span>到期时间</span><span>{{ formatTime(current.expires_at) }}</span></div>
        <div v-if="current.decided_at" class="kv"><span>决策时间</span><span>{{ formatTime(current.decided_at) }}</span></div>
        <div v-if="current.comment" class="kv"><span>审批意见</span><span>{{ current.comment }}</span></div>

        <el-divider />
        <h4 class="section">影响预览</h4>
        <pre class="code-block">{{ prettyJSON(current.preview) }}</pre>
        <h4 class="section">动作参数</h4>
        <pre class="code-block">{{ prettyJSON(current.action_detail) }}</pre>
        <template v-if="current.exec_result">
          <h4 class="section">执行结果（结果复核）</h4>
          <pre class="code-block">{{ prettyJSON(current.exec_result) }}</pre>
        </template>

        <div v-if="canDecide && current.status === 'pending'" class="row actions">
          <el-button type="primary" @click="decide(true)">通过</el-button>
          <el-button type="danger" plain @click="decide(false)">驳回</el-button>
        </div>
        <div v-else-if="sourceAlertId && (current.status === 'approved' || current.status === 'success')" class="row actions">
          <el-button type="success" plain :icon="'CircleCheck'" @click="backfill">
            回填：标记来源告警 #{{ sourceAlertId }} 已解决
          </el-button>
        </div>
        <el-alert
          v-else-if="sourceAlertId && current.status === 'pending'"
          type="info"
          :closable="false"
          show-icon
          title="审批通过后，可在此回填来源告警为已解决，闭合处置链路"
        />
        <el-alert
          v-else-if="current.status === 'approved'"
          type="success"
          :closable="false"
          show-icon
          title="审批已通过"
          description="高危操作不支持在 IM 或自动流程中一键执行，请按预演方案人工执行，并在平台回填执行结果。"
        />
      </div>
    </el-drawer>
  </div>
</template>

<style scoped>
.filter-item {
  width: 180px;
}

.toolbar-row {
  margin-bottom: 12px;
}

.ticket-list {
  list-style: none;
  margin: 0;
  padding: 0;
}

.ticket-list li {
  display: flex;
  gap: 12px;
  justify-content: space-between;
  align-items: flex-start;
  padding: 12px 0 12px 10px;
  border-bottom: 1px solid var(--c-border);
  border-left: 2px solid var(--c-border-strong);
}

.ticket-list li:last-child {
  border-bottom: 0;
}

.ticket-list li.pending {
  border-left-color: var(--c-warning);
}

.ticket-list li.approved,
.ticket-list li.success {
  border-left-color: var(--c-success);
}

.ticket-list li.rejected,
.ticket-list li.failed {
  border-left-color: var(--c-danger);
}

.ticket-main {
  flex: 1;
  min-width: 0;
}

.ticket-head {
  display: flex;
  gap: 8px;
  align-items: center;
  flex-wrap: wrap;
}

.ticket-id {
  color: var(--c-accent);
}

.ticket-action {
  margin: 8px 0 0;
  font-size: 13.5px;
  font-weight: 500;
}

.ticket-reason {
  margin: 4px 0 0;
  font-size: 12.5px;
}

.ticket-meta {
  display: flex;
  gap: 12px;
  flex-wrap: wrap;
  margin-top: 6px;
  font-size: 11.5px;
}

.ticket-actions {
  flex: 0 0 auto;
}

.ticket-actions.is-card {
  margin-top: 8px;
}

.stack {
  display: flex;
  flex-direction: column;
  gap: 10px;
  font-size: 12.5px;
}

.kv {
  display: flex;
  gap: 10px;
  justify-content: space-between;
  align-items: flex-start;
}

.kv > span:first-child {
  color: var(--c-text-3);
  flex: 0 0 88px;
}

.section {
  margin: 0;
  font-size: 13px;
}

.code-block {
  margin: 0;
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

.actions {
  margin-top: 8px;
}
</style>
