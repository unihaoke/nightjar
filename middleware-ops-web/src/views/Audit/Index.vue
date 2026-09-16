<script setup lang="ts">
/**
 * 审计日志（4.7 + 6.4）。
 *
 * 只追加语义：平台不提供审计日志的修改/删除接口；
 * 支持哈希链校验与每日快照生成，异常篡改可检出。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { auditApi } from '@/api'
import { toastError } from '@/api/http'
import type { AuditLog, AuditSnapshot } from '@/api/types'
import { formatTime, prettyJSON } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const store = useUserStore()

const loading = ref(false)
const verifying = ref(false)
const snapshotting = ref(false)
const items = ref<AuditLog[]>([])
const total = ref(0)
const snapshots = ref<AuditSnapshot[]>([])
const verifyResult = ref<{ verified: boolean; broken_id: number; message: string } | null>(null)
const detailVisible = ref(false)
const current = ref<AuditLog | null>(null)

const query = reactive({
  keyword: '',
  action_type: '',
  level: '',
  result: '',
  instance_id: 0,
  page: 1,
  page_size: 20,
})

const canSnapshot = computed(() => store.can('audit:snapshot'))

/** 常见操作类型（用于筛选下拉）。 */
const actionOptions = [
  'login',
  'logout',
  'middleware_create',
  'middleware_update',
  'middleware_delete',
  'middleware_health',
  'alert_ack',
  'alert_rule_create',
  'alert_rule_update',
  'knowledge_create',
  'knowledge_update',
  'fix_preview',
  'fix_execute',
  'approval_create',
  'approval_approve',
  'approval_reject',
  'ai_diagnose',
  'ai_diagnose_stream',
  'ai_tool_call',
  'ai_code_analyze',
  'sql_query',
]

/** 加载审计日志。 */
async function load(): Promise<void> {
  loading.value = true
  try {
    const result = await auditApi.logs({ ...query })
    items.value = result.list || []
    total.value = result.total || 0
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

/** 加载快照列表。 */
async function loadSnapshots(): Promise<void> {
  try {
    const result = await auditApi.snapshots()
    snapshots.value = result.list || []
  } catch {
    snapshots.value = []
  }
}

/** 校验哈希链。 */
async function verify(): Promise<void> {
  verifying.value = true
  try {
    verifyResult.value = await auditApi.verify({})
    ElMessage({ type: verifyResult.value.verified ? 'success' : 'error', message: verifyResult.value.message })
  } catch (error) {
    toastError(error)
  } finally {
    verifying.value = false
  }
}

/** 生成当日快照。 */
async function createSnapshot(): Promise<void> {
  snapshotting.value = true
  try {
    await auditApi.snapshot()
    ElMessage({ type: 'success', message: '审计快照已生成并校验' })
    await loadSnapshots()
  } catch (error) {
    toastError(error)
  } finally {
    snapshotting.value = false
  }
}

/** 查看详情。 */
async function openDetail(row: AuditLog): Promise<void> {
  detailVisible.value = true
  try {
    current.value = await auditApi.detail(row.id)
  } catch (error) {
    current.value = row
    toastError(error)
  }
}

onMounted(async () => {
  await Promise.all([load(), loadSnapshots()])
})
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2 class="page-title">审计日志</h2>
        <p class="page-subtitle">
          只追加 + 删除需双人复核；每日生成哈希链快照并周期校验，覆盖登录、增删改、SQL 查询、修复执行、AI 工具调用与审批动作
        </p>
      </div>
      <div class="row">
        <el-button size="small" :loading="verifying" :icon="'CircleCheck'" @click="verify">校验哈希链</el-button>
        <el-button v-if="canSnapshot" size="small" type="primary" :loading="snapshotting" @click="createSnapshot">
          生成每日快照
        </el-button>
      </div>
    </div>

    <el-alert
      v-if="verifyResult"
      :type="verifyResult.verified ? 'success' : 'error'"
      :closable="false"
      show-icon
      :title="verifyResult.message"
      :description="verifyResult.verified ? '哈希链连续，未检出篡改' : `首个断链位置：日志 #${verifyResult.broken_id}`"
      class="mb"
    />

    <div class="card filters">
      <el-input v-model="query.keyword" placeholder="搜索操作者或路由" clearable class="filter-item" @keyup.enter="load" />
      <el-select v-model="query.action_type" placeholder="全部操作类型" clearable filterable class="filter-item">
        <el-option v-for="item in actionOptions" :key="item" :label="item" :value="item" />
      </el-select>
      <el-select v-model="query.level" placeholder="全部级别" clearable class="filter-item">
        <el-option label="L0" value="L0" />
        <el-option label="L1" value="L1" />
        <el-option label="L2" value="L2" />
      </el-select>
      <el-select v-model="query.result" placeholder="全部结果" clearable class="filter-item">
        <el-option label="成功" value="success" />
        <el-option label="失败" value="failed" />
      </el-select>
      <el-button type="primary" :icon="'Search'" @click="load">查询</el-button>
    </div>

    <div class="card" v-loading="loading">
      <div class="table-scroll">
        <el-table :data="items" size="default" @row-click="openDetail">
          <el-table-column label="时间" width="170">
            <template #default="{ row }">{{ formatTime(row.created_at) }}</template>
          </el-table-column>
          <el-table-column label="操作者" width="120">
            <template #default="{ row }">{{ row.username || `#${row.user_id}` }}</template>
          </el-table-column>
          <el-table-column prop="action_type" label="操作类型" min-width="170" show-overflow-tooltip />
          <el-table-column label="级别" width="70">
            <template #default="{ row }">
              <el-tag size="small" :type="row.level === 'L2' ? 'danger' : row.level === 'L1' ? 'warning' : 'info'" effect="plain">
                {{ row.level }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="对象" width="100">
            <template #default="{ row }">
              <span class="mono">{{ row.instance_id ? `#${row.instance_id}` : '-' }}</span>
            </template>
          </el-table-column>
          <el-table-column label="结果" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.result === 'success' ? 'success' : 'danger'" effect="light">{{ row.result }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="ip_address" label="来源 IP" width="140" />
          <el-table-column prop="route" label="路由" min-width="180" show-overflow-tooltip />
          <el-table-column label="哈希链" width="130">
            <template #default="{ row }">
              <code>{{ (row.hash_self || '').slice(0, 10) }}</code>
            </template>
          </el-table-column>
        </el-table>
      </div>
      <el-pagination
        v-model:current-page="query.page"
        v-model:page-size="query.page_size"
        :total="total"
        :page-sizes="[20, 50, 100]"
        layout="total, sizes, prev, pager, next"
        class="pager"
        @current-change="load"
        @size-change="load"
      />
    </div>

    <div class="card">
      <h3 class="card-title">审计快照</h3>
      <div v-if="snapshots.length === 0">
        <el-empty description="暂无快照（每日 00:10 自动生成）" :image-size="72" />
      </div>
      <div v-else class="table-scroll">
        <el-table :data="snapshots" size="small">
          <el-table-column prop="snapshot_date" label="日期" width="120" />
          <el-table-column prop="log_count" label="日志条数" width="110" />
          <el-table-column label="链尾哈希" min-width="220">
            <template #default="{ row }">
              <code>{{ (row.chain_hash || '').slice(0, 24) }}</code>
            </template>
          </el-table-column>
          <el-table-column label="校验结果" width="110">
            <template #default="{ row }">
              <el-tag size="small" :type="row.verified ? 'success' : 'danger'" effect="light">
                {{ row.verified ? '通过' : '异常' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="file_path" label="落盘路径" min-width="240" show-overflow-tooltip />
          <el-table-column label="校验时间" width="170">
            <template #default="{ row }">{{ formatTime(row.verified_at) }}</template>
          </el-table-column>
        </el-table>
      </div>
    </div>

    <el-drawer v-model="detailVisible" title="审计详情" size="600px" direction="rtl">
      <div v-if="current" class="stack">
        <div class="kv"><span>操作类型</span><span class="mono">{{ current.action_type }}</span></div>
        <div class="kv"><span>操作者</span><span>{{ current.username }}（#{{ current.user_id }}）</span></div>
        <div class="kv"><span>操作级别</span><span>{{ current.level }}</span></div>
        <div class="kv"><span>结果</span><span>{{ current.result }}</span></div>
        <div class="kv"><span>来源 IP</span><span class="mono">{{ current.ip_address }}</span></div>
        <div class="kv"><span>User-Agent</span><span class="mono">{{ current.user_agent }}</span></div>
        <div class="kv"><span>路由</span><span class="mono">{{ current.route }}</span></div>
        <div class="kv"><span>时间</span><span>{{ formatTime(current.created_at) }}</span></div>
        <el-divider />
        <div class="kv"><span>hash_prev</span><code>{{ current.hash_prev || '（链首）' }}</code></div>
        <div class="kv"><span>hash_self</span><code>{{ current.hash_self }}</code></div>
        <el-divider />
        <h4 class="section">操作详情</h4>
        <pre class="code-block">{{ prettyJSON(current.action_detail) }}</pre>
      </div>
    </el-drawer>
  </div>
</template>

<style scoped>
.mb {
  margin-bottom: 12px;
}

.filters {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}

.filter-item {
  width: 190px;
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
  flex: 0 0 96px;
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
  max-height: 320px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
}

@media (max-width: 767px) {
  .filter-item {
    width: 100%;
  }
}
</style>
