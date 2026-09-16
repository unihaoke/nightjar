<script setup lang="ts">
/** 告警中心：告警列表、确认/恢复、AI 诊断入口、离线语义聚类。 */
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { aiApi, alertApi } from '@/api'
import { toastError } from '@/api/http'
import type { Alert } from '@/api/types'
import StatCard from '@/components/StatCard.vue'
import { alertLevelLabels, alertStatusLabels, formatNumber, formatTime, mwTypeLabels } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const router = useRouter()
const store = useUserStore()

const loading = ref(false)
const clustering = ref(false)
const items = ref<Alert[]>([])
const total = ref(0)

const query = reactive({
  level: '',
  status: '',
  mw_type: '',
  instance_id: 0,
  page: 1,
  page_size: 20,
})

const canWrite = computed(() => store.can('alert:write'))

/** 统计。 */
const stats = computed(() => ({
  active: items.value.filter((item) => item.status === 'active').length,
  critical: items.value.filter((item) => item.alert_level === 'critical').length,
  acknowledged: items.value.filter((item) => item.status === 'acknowledged').length,
}))

/** 加载告警。 */
async function load(): Promise<void> {
  loading.value = true
  try {
    const result = await alertApi.list({ ...query })
    items.value = result.list || []
    total.value = result.total || 0
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

/** 确认告警（L1）。 */
async function ack(alert: Alert): Promise<void> {
  try {
    await alertApi.ack(alert.id)
    ElMessage({ type: 'success', message: '告警已确认' })
    await load()
  } catch (error) {
    toastError(error)
  }
}

/** 标记恢复（L1）。 */
async function resolve(alert: Alert): Promise<void> {
  try {
    await alertApi.resolve(alert.id)
    ElMessage({ type: 'success', message: '告警已标记恢复' })
    await load()
  } catch (error) {
    toastError(error)
  }
}

/** 一键 AI 诊断（事件驱动，带告警上下文）。 */
function diagnose(alert: Alert): void {
  void router.push({ name: 'ai-diagnose', query: { instance_id: String(alert.instance_id), alert_id: String(alert.id) } })
}

/** 触发一轮规则评估。 */
async function evaluate(): Promise<void> {
  try {
    const result = await alertApi.evaluate()
    ElMessage({
      type: 'success',
      message: `评估完成：规则 ${result.evaluated} 条，触发 ${result.triggered} 条，合并 ${result.merged} 条，静默 ${result.suppressed} 条`,
    })
    await load()
  } catch (error) {
    toastError(error)
  }
}

/** 离线语义聚类（仅合并展示，不修改状态）。 */
async function cluster(): Promise<void> {
  clustering.value = true
  try {
    const result = await alertApi.cluster()
    ElMessage({ type: 'success', message: `聚类完成：处理 ${result.processed} 条向量，生成 ${result.clusters.length} 个聚类` })
    await load()
  } catch (error) {
    toastError(error)
  } finally {
    clustering.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2 class="page-title">告警中心</h2>
        <p class="page-subtitle">规则级实时收敛（指纹 + 窗口去重 + 冷却期）· 语义聚类为离线辅助，仅合并展示</p>
      </div>
      <div class="row">
        <el-button v-if="canWrite" size="small" @click="evaluate">立即评估规则</el-button>
        <el-button v-if="canWrite" size="small" :loading="clustering" @click="cluster">语义聚类</el-button>
        <el-button size="small" @click="router.push({ name: 'alert-rules' })">规则管理</el-button>
      </div>
    </div>

    <el-row :gutter="12">
      <el-col :xs="12" :sm="8">
        <StatCard label="当前页待处理" :value="stats.active" :status="stats.active > 0 ? 'warning' : 'ok'" />
      </el-col>
      <el-col :xs="12" :sm="8">
        <StatCard label="严重告警" :value="stats.critical" :status="stats.critical > 0 ? 'critical' : 'ok'" />
      </el-col>
      <el-col :xs="24" :sm="8">
        <StatCard label="已确认" :value="stats.acknowledged" status="neutral" hint="确认仅代表已知晓，不代表已恢复" />
      </el-col>
    </el-row>

    <div class="card filters mt">
      <el-select v-model="query.level" placeholder="全部级别" clearable class="filter-item">
        <el-option label="警告" value="warning" />
        <el-option label="严重" value="critical" />
      </el-select>
      <el-select v-model="query.status" placeholder="全部状态" clearable class="filter-item">
        <el-option label="待处理" value="active" />
        <el-option label="已确认" value="acknowledged" />
        <el-option label="已恢复" value="resolved" />
      </el-select>
      <el-select v-model="query.mw_type" placeholder="全部类型" clearable class="filter-item">
        <el-option label="Redis" value="redis" />
        <el-option label="Kafka" value="kafka" />
        <el-option label="MySQL" value="mysql" />
        <el-option label="PostgreSQL" value="pg" />
        <el-option label="Elasticsearch" value="es" />
        <el-option label="Nginx" value="nginx" />
      </el-select>
      <el-button type="primary" :icon="'Search'" @click="load">查询</el-button>
    </div>

    <div class="card" v-loading="loading">
      <div v-if="items.length === 0">
        <el-empty description="没有匹配的告警" />
      </div>
      <ul v-else class="alert-list">
        <li v-for="item in items" :key="item.id" :class="item.alert_level">
          <div class="alert-main">
            <div class="alert-head">
              <el-tag size="small" :type="item.alert_level === 'critical' ? 'danger' : 'warning'" effect="light">
                {{ alertLevelLabels[item.alert_level] || item.alert_level }}
              </el-tag>
              <el-tag size="small" effect="plain">{{ mwTypeLabels[item.mw_type] || item.mw_type }}</el-tag>
              <el-tag size="small" effect="plain">{{ alertStatusLabels[item.status] || item.status }}</el-tag>
              <el-tag v-if="item.cluster_id" size="small" type="info" effect="plain">聚类 {{ item.cluster_id }}</el-tag>
            </div>
            <p class="alert-message">{{ item.alert_message }}</p>
            <div class="alert-meta">
              <span class="muted">实例 #{{ item.instance_id }}</span>
              <span class="muted">当前值 {{ formatNumber(item.metric_value) }}</span>
              <span class="muted">触发 {{ formatTime(item.triggered_at) }}</span>
              <span v-if="item.count > 1" class="muted">窗口内合并 {{ item.count }} 次</span>
              <span v-if="item.diagnosis_id" class="text-success">已关联诊断 #{{ item.diagnosis_id }}</span>
            </div>
          </div>
          <div class="alert-actions">
            <el-button text size="small" @click="diagnose(item)">AI 诊断</el-button>
            <el-button v-if="canWrite && item.status === 'active'" text size="small" @click="ack(item)">确认</el-button>
            <el-button v-if="canWrite && item.status !== 'resolved'" text size="small" @click="resolve(item)">标记恢复</el-button>
          </div>
        </li>
      </ul>

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
  </div>
</template>

<style scoped>
.mt {
  margin-top: 12px;
}

.filters {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}

.filter-item {
  width: 170px;
}

.alert-list {
  list-style: none;
  margin: 0;
  padding: 0;
}

.alert-list li {
  display: flex;
  gap: 12px;
  justify-content: space-between;
  align-items: flex-start;
  padding: 12px 0;
  border-bottom: 1px solid var(--c-border);
  border-left: 2px solid transparent;
  padding-left: 10px;
}

.alert-list li:last-child {
  border-bottom: 0;
}

.alert-list li.critical {
  border-left-color: var(--c-danger);
}

.alert-list li.warning {
  border-left-color: var(--c-warning);
}

.alert-main {
  flex: 1;
  min-width: 0;
}

.alert-head {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}

.alert-message {
  margin: 8px 0 0;
  font-size: 13.5px;
  line-height: 1.6;
}

.alert-meta {
  display: flex;
  gap: 12px;
  flex-wrap: wrap;
  margin-top: 6px;
  font-size: 11.5px;
}

.alert-actions {
  flex: 0 0 auto;
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  gap: 4px;
}

.pager {
  margin-top: 12px;
  justify-content: flex-end;
}

@media (max-width: 767px) {
  .filter-item {
    width: 100%;
  }

  .alert-list li {
    flex-direction: column;
  }

  .alert-actions {
    flex-direction: row;
    align-items: center;
  }
}
</style>
