<script setup lang="ts">
/** 实例详情：指标概览 + 告警规则 + 最近告警 + 快捷诊断入口。 */
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { alertApi, middlewareApi, metricsApi } from '@/api'
import { toastError } from '@/api/http'
import type { Alert, AlertRule, DiagnoseResult, Metric, MetricSnapshot, MiddlewareInstance } from '@/api/types'
import MetricChart from '@/components/MetricChart.vue'
import StatCard from '@/components/StatCard.vue'
import { alertLevelLabels, alertStatusLabels, envLabels, envTagType, formatNumber, formatTime, mwTypeLabels } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const route = useRoute()
const router = useRouter()
const store = useUserStore()

const instanceID = computed(() => Number(route.params.id))
const loading = ref(false)
const instance = ref<MiddlewareInstance | null>(null)
const snapshot = ref<MetricSnapshot | null>(null)
const rules = ref<AlertRule[]>([])
const alerts = ref<Alert[]>([])
const catalog = ref<Metric[]>([])

const selectedMetric = ref('')
const history = ref<{ timestamp: string; value: number }[]>([])
const historyLoading = ref(false)

/** 当前选中指标的元信息。 */
const selectedSpec = computed(() => snapshot.value?.metrics.find((item) => item.name === selectedMetric.value) || null)

/** 异常指标优先展示。 */
const metrics = computed(() => {
  const list = snapshot.value?.metrics || []
  const rank: Record<string, number> = { critical: 0, warning: 1, unknown: 2, ok: 3 }
  return [...list].sort((a, b) => (rank[a.status] ?? 9) - (rank[b.status] ?? 9))
})

const statusCount = computed(() => {
  const list = snapshot.value?.metrics || []
  return {
    critical: list.filter((item) => item.status === 'critical').length,
    warning: list.filter((item) => item.status === 'warning').length,
    ok: list.filter((item) => item.status === 'ok').length,
  }
})

/** 加载实例与关联数据。 */
async function load(): Promise<void> {
  if (!instanceID.value) {
    return
  }
  loading.value = true
  try {
    const detail = await middlewareApi.detail(instanceID.value)
    instance.value = detail.instance
    rules.value = detail.rules || []
    alerts.value = detail.alerts || []
    catalog.value = detail.metrics_catalog?.metrics || []
    if (!selectedMetric.value && catalog.value.length > 0) {
      selectedMetric.value = catalog.value[0].name
    }
    try {
      snapshot.value = await metricsApi.snapshot(instanceID.value)
    } catch (error) {
      snapshot.value = null
      ElMessage({ type: 'warning', message: `指标采集失败：${(error as Error).message}` })
    }
    await loadHistory()
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

/** 加载历史序列。 */
async function loadHistory(): Promise<void> {
  if (!instanceID.value || !selectedMetric.value) {
    return
  }
  historyLoading.value = true
  try {
    const result = await metricsApi.history(instanceID.value, selectedMetric.value, { hours: 6 })
    history.value = result?.series || []
  } catch (error) {
    toastError(error)
    history.value = []
  } finally {
    historyLoading.value = false
  }
}

/** 立即健康探测。 */
async function handleHealth(): Promise<void> {
  try {
    const result = await middlewareApi.health(instanceID.value)
    ElMessage({ type: result.success ? 'success' : 'warning', message: result.message })
    await load()
  } catch (error) {
    toastError(error)
  }
}

/** 跳转 AI 诊断并带入实例。 */
function goDiagnose(): void {
  void router.push({ name: 'ai-diagnose', query: { instance_id: String(instanceID.value) } })
}

const diagnoseVisible = ref(false)
const diagnosing = ref(false)
const diagnose = ref<DiagnoseResult | null>(null)

/** 接入自检：把选择器、job 抓取状态与排查建议一次摊开。 */
async function handleDiagnose(): Promise<void> {
  diagnosing.value = true
  diagnoseVisible.value = true
  try {
    diagnose.value = await metricsApi.diagnose(instanceID.value)
  } catch (error) {
    diagnose.value = null
    toastError(error)
  } finally {
    diagnosing.value = false
  }
}

/** 确认告警。 */
async function ackAlert(alert: Alert): Promise<void> {
  try {
    await alertApi.ack(alert.id)
    ElMessage({ type: 'success', message: '告警已确认' })
    await load()
  } catch (error) {
    toastError(error)
  }
}

/** 启用/停用规则。 */
async function toggleRule(rule: AlertRule): Promise<void> {
  try {
    await alertApi.updateRule(rule.id, { ...rule, enabled: !rule.enabled })
    ElMessage({ type: 'success', message: rule.enabled ? '规则已停用' : '规则已启用' })
    await load()
  } catch (error) {
    toastError(error)
  }
}

onMounted(load)
</script>

<template>
  <div class="page" v-loading="loading">
    <div class="page-header">
      <div>
        <h2 class="page-title">
          {{ instance?.name || '实例详情' }}
          <el-tag v-if="instance" size="small" effect="plain" :type="envTagType[instance.environment]">
            {{ envLabels[instance.environment] }}
          </el-tag>
          <el-tag v-if="instance" size="small" effect="plain">
            {{ mwTypeLabels[instance.mw_type] || instance.mw_type }}
          </el-tag>
        </h2>
        <p class="page-subtitle mono">
          {{ instance?.host }}:{{ instance?.port }}
          <span v-if="instance?.username"> · 账号 {{ instance.username }}</span>
          <span v-if="instance?.group_name"> · 分组 {{ instance.group_name }}</span>
        </p>
      </div>
      <div class="row">
        <el-button :icon="'Back'" size="small" @click="router.back()">返回</el-button>
        <el-button :icon="'Refresh'" size="small" @click="load">刷新</el-button>
        <el-button type="primary" size="small" :icon="'MagicStick'" @click="goDiagnose">AI 诊断</el-button>
        <el-button size="small" @click="handleHealth">健康探测</el-button>
        <el-button size="small" :loading="diagnosing" @click="handleDiagnose">接入自检</el-button>
      </div>
    </div>

    <el-alert
      v-if="instance?.last_message"
      :type="instance.status === 1 ? 'success' : 'error'"
      :closable="false"
      show-icon
      :title="instance.last_message"
      :description="`最近探测：${formatTime(instance.last_check_at)}`"
      class="mb"
    />

    <el-row :gutter="12">
      <el-col :xs="12" :sm="8" :md="6">
        <StatCard label="严重指标" :value="statusCount.critical" status="critical" hint="需立即处理" />
      </el-col>
      <el-col :xs="12" :sm="8" :md="6">
        <StatCard label="警告指标" :value="statusCount.warning" status="warning" hint="建议关注" />
      </el-col>
      <el-col :xs="12" :sm="8" :md="6">
        <StatCard label="正常指标" :value="statusCount.ok" status="ok" :hint="`共 ${metrics.length} 项`" />
      </el-col>
      <el-col :xs="12" :sm="8" :md="6">
        <StatCard
          label="活跃告警"
          :value="alerts.filter((item) => item.status === 'active').length"
          :status="alerts.some((item) => item.status === 'active' && item.alert_level === 'critical') ? 'critical' : 'warning'"
          :hint="`规则 ${rules.length} 条`"
        />
      </el-col>
    </el-row>

    <!-- 指标趋势 -->
    <div class="card mt">
      <h3 class="card-title">
        指标趋势（近 6 小时）
        <div class="row">
          <el-select v-model="selectedMetric" size="small" class="metric-select" @change="loadHistory">
            <el-option v-for="item in catalog" :key="item.name" :label="item.display_name" :value="item.name" />
          </el-select>
        </div>
      </h3>
      <MetricChart
        :series="history"
        :unit="selectedSpec?.unit || ''"
        :warning="Number(selectedSpec?.warning_threshold || 0)"
        :critical="Number(selectedSpec?.critical_threshold || 0)"
        :loading="historyLoading"
        :height="280"
      />
      <p v-if="snapshot?.note" class="muted note">{{ snapshot.note }}</p>
    </div>

    <!-- 当前指标 -->
    <div class="card">
      <h3 class="card-title">
        当前指标
        <el-tag size="small" effect="plain">来源：{{ snapshot?.source === 'prometheus' ? 'Prometheus' : '内置模拟器' }}</el-tag>
      </h3>
      <div class="metrics-grid">
        <div v-for="item in metrics" :key="item.name" class="metric-cell" :class="`is-${item.status}`">
          <div class="metric-head">
            <span class="metric-name">{{ item.display_name }}</span>
            <el-tag size="small" :type="item.status === 'ok' ? 'success' : item.status === 'warning' ? 'warning' : item.status === 'critical' ? 'danger' : 'info'">
              {{ item.status }}
            </el-tag>
          </div>
          <div class="metric-value-row">
            <span class="metric-number">{{ formatNumber(item.latest, item.unit) }}</span>          </div>
          <p class="metric-threshold muted">
            警告 {{ item.warning_threshold || '-' }} · 严重 {{ item.critical_threshold || '-' }}
          </p>
        </div>
        <el-empty v-if="metrics.length === 0" description="该类型暂无指标画像" :image-size="72" />
      </div>
    </div>

    <!-- 告警规则 -->
    <div class="card">
      <h3 class="card-title">
        告警规则
        <el-button text size="small" @click="router.push({ name: 'alert-rules', query: { instance_id: String(instanceID) } })">
          管理规则
        </el-button>
      </h3>
      <div class="table-scroll">
        <el-table :data="rules" size="small">
          <el-table-column prop="name" label="规则" min-width="150" show-overflow-tooltip />
          <el-table-column prop="metric_name" label="指标" min-width="160" show-overflow-tooltip />
          <el-table-column label="条件" width="140">
            <template #default="{ row }">
              <span class="mono">{{ row.metric_name }} {{ row.operator }} {{ row.threshold }}</span>
            </template>
          </el-table-column>
          <el-table-column label="级别" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.level === 'critical' ? 'danger' : 'warning'" effect="light">
                {{ alertLevelLabels[row.level] || row.level }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="收敛" width="130">
            <template #default="{ row }">窗口 {{ row.time_window }}m / 冷却 {{ row.cooldown }}m</template>
          </el-table-column>
          <el-table-column label="状态" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.enabled ? 'success' : 'info'" effect="plain">
                {{ row.enabled ? '启用' : '停用' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="90">
            <template #default="{ row }">
              <el-button text size="small" @click="toggleRule(row)">{{ row.enabled ? '停用' : '启用' }}</el-button>
            </template>
          </el-table-column>
        </el-table>
      </div>
    </div>

    <!-- 最近告警 -->
    <div class="card">
      <h3 class="card-title">最近告警</h3>
      <div v-if="alerts.length === 0">
        <el-empty description="该实例暂无告警" :image-size="72" />
      </div>
      <ul v-else class="alert-list">
        <li v-for="item in alerts" :key="item.id" :class="item.alert_level">
          <div class="alert-main">
            <el-tag size="small" :type="item.alert_level === 'critical' ? 'danger' : 'warning'" effect="light">
              {{ alertLevelLabels[item.alert_level] || item.alert_level }}
            </el-tag>
            <span class="alert-message">{{ item.alert_message }}</span>
          </div>
          <div class="alert-meta">
            <span class="muted">{{ formatTime(item.triggered_at) }}</span>
            <span v-if="item.count > 1" class="muted">合并 {{ item.count }} 次</span>
            <el-tag size="small" effect="plain">{{ alertStatusLabels[item.status] || item.status }}</el-tag>
            <el-button v-if="item.status === 'active' && store.can('alert:write')" text size="small" @click="ackAlert(item)">
              确认
            </el-button>
          </div>
        </li>
      </ul>
    </div>

    <!-- 接入自检：纳管后"看不到监控/日志"时先看这里 -->
    <el-dialog v-model="diagnoseVisible" title="接入自检" width="720px">
      <div v-if="diagnose" class="diagnose">
        <el-descriptions :column="2" border size="small">
          <el-descriptions-item label="实例">{{ diagnose.instance_name }}（{{ diagnose.mw_type }}）</el-descriptions-item>
          <el-descriptions-item label="连接地址">{{ diagnose.host }}:{{ diagnose.port }}</el-descriptions-item>
          <el-descriptions-item label="数据源">
            <el-tag size="small" :type="diagnose.monitor_kind === 'prometheus' ? 'success' : 'warning'">
              {{ diagnose.monitor_kind === 'prometheus' ? 'Prometheus' : '内置模拟器' }}
            </el-tag>
            <el-tag class="ml" size="small" :type="diagnose.prometheus_healthy ? 'success' : 'danger'">
              {{ diagnose.prometheus_healthy ? '健康' : '不可达' }}
            </el-tag>
          </el-descriptions-item>
          <el-descriptions-item label="命中指标">
            {{ diagnose.matched }} / {{ diagnose.total }}
          </el-descriptions-item>
          <el-descriptions-item label="PromQL 选择器" :span="2">
            <span class="mono">{{ diagnose.selector || '-' }}</span>
          </el-descriptions-item>
          <el-descriptions-item label="job 抓取状态" :span="2">
            <span v-if="diagnose.job_up === null">Prometheus 中不存在该 job（抓取配置未生效）</span>
            <span v-else-if="diagnose.job_up === 1">up = 1（Exporter 已被抓取）</span>
            <span v-else>up = 0（target 抓取失败）</span>
          </el-descriptions-item>
        </el-descriptions>

        <el-alert v-if="diagnose.note" class="mt" type="info" :closable="false" show-icon :title="diagnose.note" />

        <h4 class="diag-title">建议</h4>
        <ul class="diag-list">
          <li v-for="(hint, index) in diagnose.hints" :key="index">{{ hint }}</li>
          <li v-if="diagnose.hints.length === 0">指标链路正常，无需处理。</li>
        </ul>

        <h4 class="diag-title">日志链路（与中间件实例无关）</h4>
        <ul class="diag-list">
          <li v-for="(item, index) in diagnose.log_checklist" :key="`log-${index}`">{{ item }}</li>
        </ul>

        <h4 class="diag-title">逐条指标</h4>
        <div class="table-scroll">
          <el-table :data="diagnose.metrics" size="small" max-height="240">
            <el-table-column prop="display_name" label="指标" width="120" />
            <el-table-column label="命中" width="80">
              <template #default="{ row }">
                <el-tag size="small" :type="row.matched ? 'success' : 'info'">{{ row.matched ? '是' : '否' }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column prop="expr" label="PromQL" min-width="320" show-overflow-tooltip />
          </el-table>
        </div>
      </div>
      <div v-else v-loading="diagnosing" class="diag-loading" />
    </el-dialog>
  </div>
</template>

<style scoped>
.mb {
  margin-bottom: 12px;
}

.mt {
  margin-top: 12px;
}

.card + .card {
  margin-top: 12px;
}

.metric-select {
  width: 200px;
}

.note {
  margin: 8px 0 0;
  font-size: 11.5px;
}

.ml {
  margin-left: 6px;
}

.diag-loading {
  min-height: 160px;
}

.diag-title {
  margin: 16px 0 6px;
  font-size: 13px;
  font-weight: 600;
}

.diag-list {
  margin: 0;
  padding-left: 18px;
  font-size: 12.5px;
  line-height: 1.8;
}

.diagnose .mt {
  margin-top: 12px;
}

.metrics-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(190px, 1fr));
  gap: 10px;
}

.metric-cell {
  border: 1px solid var(--c-border);
  border-left: 2px solid var(--c-border-strong);
  border-radius: var(--r-md);
  padding: 10px 12px;
}

.metric-cell.is-ok {
  border-left-color: var(--c-success);
}

.metric-cell.is-warning {
  border-left-color: var(--c-warning);
}

.metric-cell.is-critical {
  border-left-color: var(--c-danger);
}

.metric-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.metric-name {
  font-size: 12.5px;
  color: var(--c-text-2);
}

.metric-value-row {
  margin-top: 4px;
}

.metric-number {
  font-size: 18px;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}

.metric-threshold {
  margin: 4px 0 0;
  font-size: 11px;
}

.alert-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.alert-list li {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  flex-wrap: wrap;
  padding: 10px 12px;
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
}

.alert-list li.critical {
  border-left: 2px solid var(--c-danger);
}

.alert-list li.warning {
  border-left: 2px solid var(--c-warning);
}

.alert-main {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: 1 1 260px;
  min-width: 0;
}

.alert-message {
  font-size: 13px;
  overflow: hidden;
  text-overflow: ellipsis;
}

.alert-meta {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 12px;
}

@media (max-width: 767px) {
  .metric-select {
    width: 160px;
  }
}
</style>
