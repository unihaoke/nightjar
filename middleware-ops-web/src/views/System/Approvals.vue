<script setup lang="ts">
/** 审批管理（6.2）：L2 高危操作工单，prod 强制审批，30 分钟超时自动拒绝。 */
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { approvalApi } from '@/api'
import { toastError } from '@/api/http'
import type { Approval } from '@/api/types'
import { approvalStatusLabels, envLabels, envTagType, formatTime, prettyJSON } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const store = useUserStore()

const loading = ref(false)
const items = ref<Approval[]>([])
const total = ref(0)
const detailVisible = ref(false)
const current = ref<Approval | null>(null)

const query = reactive({
  status: 'pending',
  mine: false,
  page: 1,
  page_size: 20,
})

const canDecide = computed(() => store.can('approval:decide'))

/** 加载工单。 */
async function load(): Promise<void> {
  loading.value = true
  try {
    const result = await approvalApi.list({ ...query })
    items.value = result.list || []
    total.value = result.total || 0
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

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
    await load()
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

onMounted(load)
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
      <el-checkbox v-model="query.mine" @change="load">只看我提交的</el-checkbox>
    </div>

    <div class="card filters">
      <el-select v-model="query.status" placeholder="全部状态" clearable class="filter-item" @change="load">
        <el-option label="待审批" value="pending" />
        <el-option label="已通过" value="approved" />
        <el-option label="已驳回" value="rejected" />
        <el-option label="已超时" value="expired" />
        <el-option label="已执行" value="success" />
        <el-option label="执行失败" value="failed" />
      </el-select>
      <el-button type="primary" :icon="'Search'" @click="load">查询</el-button>
    </div>

    <div class="card" v-loading="loading">
      <div v-if="items.length === 0">
        <el-empty description="没有匹配的审批工单" />
      </div>
      <ul v-else class="ticket-list">
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

      <el-pagination
        v-model:current-page="query.page"
        v-model:page-size="query.page_size"
        :total="total"
        layout="total, prev, pager, next"
        class="pager"
        @current-change="load"
      />
    </div>

    <el-drawer v-model="detailVisible" title="工单详情" size="620px" direction="rtl">
      <div v-if="current" class="stack">
        <div class="kv"><span>工单号</span><code>{{ current.ticket_id }}</code></div>
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
.filters {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}

.filter-item {
  width: 180px;
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

.pager {
  margin-top: 12px;
  justify-content: flex-end;
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

@media (max-width: 767px) {
  .filter-item {
    width: 100%;
  }

  .ticket-list li {
    flex-direction: column;
  }
}
</style>
