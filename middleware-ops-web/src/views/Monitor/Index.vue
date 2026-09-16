<script setup lang="ts">
/**
 * 统一监控（4.2）。
 *
 * 指标全部来自 Prometheus（或内置模拟器），平台不落自有指标表；
 * 支持单实例多指标趋势与多实例同指标对比。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { metricsApi, middlewareApi } from '@/api'
import { toastError } from '@/api/http'
import type { MetricSnapshot, MiddlewareInstance } from '@/api/types'
import MetricChart from '@/components/MetricChart.vue'
import { envLabels, formatNumber, mwTypeLabels } from '@/utils/format'

const router = useRouter()

const loading = ref(false)
const historyLoading = ref(false)
const compareLoading = ref(false)
const instances = ref<MiddlewareInstance[]>([])
const snapshot = ref<MetricSnapshot | null>(null)
const series = ref<{ timestamp: string; value: number }[]>([])
const compareItems = ref<{ instance_id: number; instance_name: string; value: number; unit: string }[]>([])

const query = reactive({
  instance_id: 0,
  metric: '',
  hours: 6,
})

/** 当前实例的指标目录。 */
const metricOptions = computed(() => {
  if (snapshot.value?.metrics?.length) {
    return snapshot.value.metrics.map((item) => ({ name: item.name, label: item.display_name, unit: item.unit }))
  }
  return []
})

const selectedSpec = computed(() => snapshot.value?.metrics?.find((item) => item.name === query.metric) || null)

/** 加载实例下拉。 */
async function loadInstances(): Promise<void> {
  try {
    const result = await middlewareApi.list({ page: 1, page_size: 100 })
    instances.value = result.list || []
    if (!query.instance_id && instances.value.length > 0) {
      query.instance_id = instances.value[0].id
    }
  } catch (error) {
    toastError(error)
  }
}

/** 加载快照与历史。 */
async function loadSnapshot(): Promise<void> {
  if (!query.instance_id) {
    return
  }
  loading.value = true
  try {
    snapshot.value = await metricsApi.snapshot(query.instance_id)
    if (!query.metric && metricOptions.value.length > 0) {
      query.metric = metricOptions.value[0].name
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
  if (!query.instance_id || !query.metric) {
    return
  }
  historyLoading.value = true
  try {
    const result = await metricsApi.history(query.instance_id, query.metric, { hours: query.hours })
    series.value = result?.series || []
  } catch (error) {
    toastError(error)
    series.value = []
  } finally {
    historyLoading.value = false
  }
}

/** 多实例对比。 */
async function compare(): Promise<void> {
  if (!query.metric) {
    return
  }
  compareLoading.value = true
  try {
    const result = await metricsApi.compare([], query.metric)
    compareItems.value = (result.items || []).slice(0, 10)
  } catch (error) {
    toastError(error)
  } finally {
    compareLoading.value = false
  }
}

/** 打开实例详情。 */
function openDetail(id: number): void {
  void router.push({ name: 'middleware-detail', params: { id } })
}

onMounted(async () => {
  await loadInstances()
  await loadSnapshot()
})
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2 class="page-title">统一监控</h2>
        <p class="page-subtitle">指标由 Prometheus + 官方 Exporter 采集，平台通过 PromQL 查询，不建自有指标表</p>
      </div>
      <div class="row">
        <el-tag v-if="snapshot" size="small" effect="light" :type="snapshot.source === 'prometheus' ? 'success' : 'warning'">
          {{ snapshot.source === 'prometheus' ? 'Prometheus' : '内置模拟器' }}
        </el-tag>
        <el-button :icon="'Refresh'" size="small" @click="loadSnapshot">刷新</el-button>
      </div>
    </div>

    <div class="card filters">
      <el-select v-model="query.instance_id" placeholder="选择实例" filterable class="filter-item" @change="loadSnapshot">
        <el-option
          v-for="item in instances"
          :key="item.id"
          :label="`${item.name}（${mwTypeLabels[item.mw_type] || item.mw_type} · ${envLabels[item.environment]}）`"
          :value="item.id"
        />
      </el-select>
      <el-select v-model="query.metric" placeholder="选择指标" class="filter-item" @change="loadHistory">
        <el-option v-for="item in metricOptions" :key="item.name" :label="item.label" :value="item.name" />
      </el-select>
      <el-radio-group v-model="query.hours" size="small" @change="loadHistory">
        <el-radio-button :value="1">1h</el-radio-button>
        <el-radio-button :value="6">6h</el-radio-button>
        <el-radio-button :value="24">24h</el-radio-button>
      </el-radio-group>
      <el-button :loading="compareLoading" @click="compare">多实例对比</el-button>
    </div>

    <div v-if="snapshot?.note" class="card note-card">
      <el-icon><InfoFilled /></el-icon>
      <span>{{ snapshot.note }}</span>
    </div>

    <el-row :gutter="12">
      <el-col :xs="24" :md="16">
        <div class="card" v-loading="historyLoading">
          <h3 class="card-title">
            {{ selectedSpec?.display_name || '指标趋势' }}
            <span class="muted">近 {{ query.hours }} 小时</span>
          </h3>
          <MetricChart
            :series="series"
            :unit="selectedSpec?.unit || ''"
            :warning="Number(selectedSpec?.warning_threshold || 0)"
            :critical="Number(selectedSpec?.critical_threshold || 0)"
            :height="300"
          />
          <p v-if="selectedSpec?.expr" class="expr muted mono">{{ selectedSpec.expr }}</p>
        </div>
      </el-col>
      <el-col :xs="24" :md="8">
        <div class="card compare-card" v-loading="compareLoading">
          <h3 class="card-title">多实例对比（Top 10）</h3>
          <ul v-if="compareItems.length > 0" class="compare-list">
            <li v-for="item in compareItems" :key="item.instance_id" @click="openDetail(item.instance_id)">
              <span class="compare-name">{{ item.instance_name }}</span>
              <span class="mono">{{ formatNumber(item.value, item.unit) }}</span>
            </li>
          </ul>
          <el-empty v-else description="点击上方「多实例对比」获取" :image-size="70" />
        </div>
      </el-col>
    </el-row>

    <div class="card" v-loading="loading">
      <h3 class="card-title">当前指标快照</h3>
      <div class="metrics-grid">
        <div v-for="item in snapshot?.metrics || []" :key="item.name" class="metric-cell" :class="`is-${item.status}`">
          <div class="metric-head">
            <span class="metric-name">{{ item.display_name }}</span>
            <el-tag size="small" :type="item.status === 'ok' ? 'success' : item.status === 'warning' ? 'warning' : item.status === 'critical' ? 'danger' : 'info'">
              {{ item.status }}
            </el-tag>
          </div>
          <div class="metric-number">{{ formatNumber(item.latest, item.unit) }}</div>
          <p class="metric-threshold muted">{{ item.category }}</p>
        </div>
        <el-empty v-if="!(snapshot?.metrics || []).length" description="暂无指标数据" :image-size="72" />
      </div>
    </div>
  </div>
</template>

<style scoped>
.filters {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  align-items: center;
  margin-bottom: 12px;
}

.filter-item {
  width: 260px;
}

.note-card {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
  font-size: 12.5px;
  color: var(--c-text-2);
}

.card + .card {
  margin-top: 12px;
}

.compare-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.compare-list li {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  padding: 6px 8px;
  border-radius: var(--r-md);
  font-size: 12.5px;
  cursor: pointer;
}

.compare-list li:hover {
  background: var(--c-surface-2);
}

.compare-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.metrics-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(180px, 1fr));
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
  gap: 6px;
}

.metric-name {
  font-size: 12.5px;
  color: var(--c-text-2);
}

.metric-number {
  margin-top: 4px;
  font-size: 18px;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}

.metric-threshold {
  margin: 4px 0 0;
  font-size: 11px;
}

.expr {
  margin: 8px 0 0;
  font-size: 11px;
  word-break: break-all;
}

@media (max-width: 767px) {
  .filter-item {
    width: 100%;
  }
}
</style>
