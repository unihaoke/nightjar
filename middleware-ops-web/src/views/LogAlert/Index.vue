<script setup lang="ts">
/**
 * 日志告警中心（4.8）：事件列表 + 堆栈详情 + 三点式 AI 代码分析。
 *
 * 采集入口（HTTP Hook / Agent）说明：应用 POST 到 /api/hooks/logs，
 * 平台按「错误指纹 + 5 分钟窗口」去重聚合，冷却期内不重复通知。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { aiApi, logAlertApi } from '@/api'
import { toastError } from '@/api/http'
import type { CodeAnalysis, LogEvent } from '@/api/types'
import { formatTime } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const store = useUserStore()

const loading = ref(false)
const analyzing = ref(false)
const items = ref<LogEvent[]>([])
const total = ref(0)
const detailVisible = ref(false)
const current = ref<LogEvent | null>(null)
const analysis = ref<CodeAnalysis | null>(null)
const analyzeResult = ref<Record<string, unknown> | null>(null)

const query = reactive({
  keyword: '',
  service: '',
  status: '',
  alert_type: '',
  page: 1,
  page_size: 20,
})

const canWrite = computed(() => store.can('logalert:write'))

/** 加载事件列表。 */
async function load(): Promise<void> {
  loading.value = true
  try {
    const result = await logAlertApi.events({ ...query })
    items.value = result.list || []
    total.value = result.total || 0
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

/** 查看详情。 */
async function openDetail(event: LogEvent): Promise<void> {
  detailVisible.value = true
  analyzeResult.value = null
  try {
    const detail = await logAlertApi.event(event.id)
    current.value = detail.event
    analysis.value = detail.analysis
  } catch (error) {
    toastError(error)
  }
}

/** 触发 AI 代码分析。 */
async function analyze(): Promise<void> {
  if (!current.value) {
    return
  }
  analyzing.value = true
  try {
    const result = await aiApi.codeAnalyze({ event_id: current.value.id })
    analyzeResult.value = result.report
    ElMessage({ type: 'success', message: '代码分析完成' })
    const detail = await logAlertApi.event(current.value.id)
    analysis.value = detail.analysis
    await load()
  } catch (error) {
    toastError(error)
  } finally {
    analyzing.value = false
  }
}

/** 更新事件状态。 */
async function updateStatus(status: string): Promise<void> {
  if (!current.value) {
    return
  }
  try {
    await logAlertApi.updateStatus(current.value.id, status)
    ElMessage({ type: 'success', message: '状态已更新' })
    current.value.status = status
    await load()
  } catch (error) {
    toastError(error)
  }
}

/** 复制 Hook 接入示例。 */
async function copyHookExample(): Promise<void> {
  const example = `curl -X POST http://<平台地址>/api/hooks/logs \\
  -H "Content-Type: application/json" \\
  -H "X-Hook-Token: <MWOPS_HOOK_TOKEN>" \\
  -d '{"service":"order-service","level":"ERROR","message":"Order 10086 处理失败","stacktrace":"java.lang.NullPointerException\\n\\tat com.demo.OrderService.process(OrderService.java:42)"}'`
  try {
    await navigator.clipboard.writeText(example)
    ElMessage({ type: 'success', message: '接入示例已复制' })
  } catch {
    ElMessage({ type: 'info', message: example })
  }
}

onMounted(load)
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2 class="page-title">日志告警</h2>
        <p class="page-subtitle">
          应用通过 HTTP Hook（零侵入）或轻量 Agent 上报 ERROR/堆栈，平台按错误指纹聚合去重
        </p>
      </div>
      <el-button size="small" :icon="'CopyDocument'" @click="copyHookExample">复制接入示例</el-button>
    </div>

    <div class="card filters">
      <el-input v-model="query.keyword" placeholder="搜索堆栈或指纹" clearable class="filter-item" @keyup.enter="load" />
      <el-input v-model="query.service" placeholder="服务名" clearable class="filter-item" @keyup.enter="load" />
      <el-select v-model="query.status" placeholder="全部状态" clearable class="filter-item">
        <el-option label="待处理" value="pending" />
        <el-option label="分析中" value="analyzing" />
        <el-option label="已解决" value="resolved" />
        <el-option label="已忽略" value="ignored" />
      </el-select>
      <el-select v-model="query.alert_type" placeholder="全部类型" clearable class="filter-item">
        <el-option label="异常报错" value="error" />
        <el-option label="异常堆栈" value="stack" />
        <el-option label="GC 日志" value="gc" />
      </el-select>
      <el-button type="primary" :icon="'Search'" @click="load">查询</el-button>
    </div>

    <div class="card" v-loading="loading">
      <div class="table-scroll">
        <el-table :data="items" size="default" @row-click="openDetail">
          <el-table-column prop="service_name" label="服务" min-width="140" show-overflow-tooltip />
          <el-table-column label="指纹" width="130">
            <template #default="{ row }">
              <code>{{ (row.error_signature || '').slice(0, 8) }}</code>
            </template>
          </el-table-column>
          <el-table-column prop="alert_type" label="类型" width="90" />
          <el-table-column label="次数" width="80">
            <template #default="{ row }">
              <el-tag size="small" :type="row.error_count > 10 ? 'danger' : row.error_count > 3 ? 'warning' : 'info'" effect="light">
                {{ row.error_count }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="严重度" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.severity === 'critical' ? 'danger' : 'warning'" effect="plain">
                {{ row.severity }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="raw_stacktrace" label="堆栈摘要" min-width="220" show-overflow-tooltip />
          <el-table-column label="最近出现" width="170">
            <template #default="{ row }">{{ formatTime(row.last_seen_at) }}</template>
          </el-table-column>
          <el-table-column label="状态" width="100">
            <template #default="{ row }">
              <el-tag size="small" effect="light" :type="row.status === 'resolved' ? 'success' : row.status === 'ignored' ? 'info' : 'warning'">
                {{ row.status }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="已分析" width="90">
            <template #default="{ row }">
              <el-icon :class="row.analyzed ? 'text-success' : 'muted'"><CircleCheck v-if="row.analyzed" /><Clock v-else /></el-icon>
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

    <el-drawer v-model="detailVisible" title="日志事件详情" size="720px" direction="rtl">
      <div v-if="current" class="stack">
        <div class="row">
          <el-tag size="small" effect="plain">{{ current.service_name }}</el-tag>
          <el-tag size="small" effect="plain">{{ current.alert_type }}</el-tag>
          <el-tag size="small" type="warning" effect="light">出现 {{ current.error_count }} 次</el-tag>
          <el-tag size="small" effect="light">{{ current.status }}</el-tag>
        </div>

        <div class="field">
          <span class="field-label">错误指纹</span>
          <code>{{ current.error_signature }}</code>
        </div>
        <div class="field">
          <span class="field-label">时间范围</span>
          <span>{{ formatTime(current.first_seen_at) }} → {{ formatTime(current.last_seen_at) }}</span>
        </div>

        <h4 class="section">堆栈原文</h4>
        <pre class="code-block">{{ current.raw_stacktrace || '（无堆栈）' }}</pre>

        <div v-if="current.context_lines" class="field">
          <span class="field-label">上下文日志</span>
          <pre class="code-block">{{ current.context_lines }}</pre>
        </div>

        <div class="row actions">
          <el-button type="primary" :loading="analyzing" :icon="'MagicStick'" @click="analyze">AI 代码分析</el-button>
          <el-button v-if="canWrite" @click="updateStatus('resolved')">标记已解决</el-button>
          <el-button v-if="canWrite" @click="updateStatus('ignored')">忽略</el-button>
        </div>

        <!-- 三点式代码分析结果 -->
        <template v-if="analysis || analyzeResult">
          <h4 class="section">代码分析报告（三点式）</h4>
          <div class="analysis-card">
            <div class="analysis-row">
              <span class="field-label">问题位置</span>
              <span class="mono">
                {{ analysis?.located_file || (analyzeResult?.located_file as string) || '未能定位' }}
                <template v-if="analysis?.located_line || analyzeResult?.located_line">:{{ analysis?.located_line || analyzeResult?.located_line }}</template>
              </span>
            </div>
            <div class="analysis-row">
              <span class="field-label">根因分析</span>
              <span>{{ analysis?.root_cause || analyzeResult?.root_cause || '-' }}</span>
            </div>
            <div class="analysis-row">
              <span class="field-label">应急方案</span>
              <span>{{ analysis?.emergency_plan || analyzeResult?.emergency_plan || '-' }}</span>
            </div>
            <div class="analysis-row">
              <span class="field-label">修复建议</span>
              <span>{{ analysis?.fix_suggestion || analyzeResult?.fix_suggestion || '-' }}</span>
            </div>
            <div class="analysis-row">
              <span class="field-label">影响范围</span>
              <span>{{ analysis?.impact_scope || analyzeResult?.impact_scope || '-' }}</span>
            </div>
            <div class="analysis-row">
              <span class="field-label">出网合规</span>
              <el-tag size="small" :type="analysis?.outbound_ok ? 'success' : 'info'" effect="light">
                {{ analysis?.outbound_ok ? '已通过白名单 + 脱敏' : '本地分析（未出网）' }}
              </el-tag>
            </div>
          </div>
          <pre v-if="analysis?.code_snippet" class="code-block">{{ analysis.code_snippet }}</pre>
        </template>
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

.pager {
  margin-top: 12px;
  justify-content: flex-end;
}

.stack {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 12.5px;
}

.field-label {
  font-size: 11.5px;
  color: var(--c-text-3);
}

.section {
  margin: 8px 0 0;
  font-size: 13px;
  font-weight: 600;
}

.code-block {
  margin: 0;
  padding: 10px 12px;
  background: var(--c-surface-2);
  border-radius: var(--r-md);
  font-size: 12px;
  line-height: 1.6;
  max-height: 280px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
}

.actions {
  margin-top: 4px;
}

.analysis-card {
  display: flex;
  flex-direction: column;
  gap: 10px;
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  padding: 12px;
}

.analysis-row {
  display: flex;
  gap: 10px;
  font-size: 12.5px;
}

.analysis-row .field-label {
  flex: 0 0 64px;
  padding-top: 1px;
}

@media (max-width: 767px) {
  .filter-item {
    width: 100%;
  }
}
</style>
