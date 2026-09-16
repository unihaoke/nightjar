<script setup lang="ts">
/**
 * 全局大盘（8.2 系统模块）。
 *
 * 数据来源：/api/system/overview（实例健康、告警趋势、诊断质量与成本、平台自检）。
 * 刷新策略：进入页面即拉取；提供手动刷新；不依赖常驻轮询，避免无谓的后端压力。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { systemApi } from '@/api'
import { toastError } from '@/api/http'
import type { Overview } from '@/api/types'
import StatCard from '@/components/StatCard.vue'
import MetricChart from '@/components/MetricChart.vue'
import { formatDuration, formatNumber, formatPercent, mwTypeLabels } from '@/utils/format'

const router = useRouter()
const loading = ref(false)
const overview = ref<Overview | null>(null)
const refreshedAt = ref<string>('')

/** 拉取大盘数据。 */
async function load(): Promise<void> {
  loading.value = true
  try {
    overview.value = await systemApi.overview()
    refreshedAt.value = new Date().toLocaleTimeString('zh-CN')
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

/** 告警趋势图数据。 */
const alertSeries = computed(() =>
  (overview.value?.alerts.trend || []).map((item) => ({ timestamp: item.label, value: item.total })),
)

/** 中间件类型分布。 */
const typeDistribution = computed(() => {
  const byType = overview.value?.instances.by_type || {}
  return Object.entries(byType)
    .map(([type, count]) => ({ label: mwTypeLabels[type] || type, value: count }))
    .sort((a, b) => b.value - a.value)
})

/** 采纳率（0-1 → 百分比）。 */
const adoptionRate = computed(() => formatPercent((overview.value?.diagnosis.adoption_rate || 0) * 100, 1))

/** 实例健康状态语义。 */
const instanceStatus = computed<'ok' | 'warning' | 'critical'>(() => {
  const data = overview.value?.instances
  if (!data || data.total === 0) {
    return 'warning'
  }
  if (data.offline === 0) {
    return 'ok'
  }
  return data.offline / data.total > 0.2 ? 'critical' : 'warning'
})

/** 活跃告警语义。 */
const alertStatus = computed<'ok' | 'warning' | 'critical'>(() => {
  const data = overview.value?.alerts
  if (!data) {
    return 'ok'
  }
  const critical = data.by_level?.critical || 0
  if (critical > 0) {
    return 'critical'
  }
  return (data.by_level?.warning || 0) > 0 ? 'warning' : 'ok'
})

let timer: number | undefined

onMounted(async () => {
  await load()
  // 大盘延迟目标 ≤30s（设计文档 10），这里用 60s 轮询兼顾成本。
  timer = window.setInterval(() => void load(), 60_000)
})

onBeforeUnmount(() => {
  if (timer) {
    window.clearInterval(timer)
  }
})
</script>

<template>
  <div class="page" v-loading="loading">
    <div class="page-header">
      <div>
        <h2 class="page-title">全局大盘</h2>
        <p class="page-subtitle">
          纳管实例 {{ overview?.instances.total ?? 0 }} 个 · 平台运行 {{ formatDuration(overview?.platform.uptime_sec || 0) }}
          <span v-if="refreshedAt"> · 更新于 {{ refreshedAt }}</span>
        </p>
      </div>
      <div class="row">
        <el-tag v-if="overview" size="small" effect="light" :type="overview.platform.cost_tripped ? 'danger' : 'info'">
          缓存：{{ overview.platform.cache_kind }} · 队列待处理：{{ overview.platform.queue_pending }}
        </el-tag>
        <el-button :icon="'Refresh'" size="small" @click="load">刷新</el-button>
      </div>
    </div>

    <!-- 关键指标 -->
    <el-row :gutter="12">
      <el-col :xs="12" :sm="12" :md="6">
        <StatCard
          label="中间件实例"
          :value="overview?.instances.total ?? 0"
          :status="instanceStatus"
          :hint="`在线 ${overview?.instances.online ?? 0} · 离线 ${overview?.instances.offline ?? 0}`"
        />
      </el-col>
      <el-col :xs="12" :sm="12" :md="6">
        <StatCard
          label="活跃告警（24h）"
          :value="overview?.alerts.active ?? 0"
          :status="alertStatus"
          :hint="`严重 ${overview?.alerts.by_level?.critical ?? 0} · 警告 ${overview?.alerts.by_level?.warning ?? 0}`"
        />
      </el-col>
      <el-col :xs="12" :sm="12" :md="6">
        <StatCard
          label="AI 诊断采纳率"
          :value="adoptionRate"
          :status="(overview?.diagnosis.adoption_rate || 0) >= 0.7 ? 'ok' : 'warning'"
          :hint="`累计诊断 ${overview?.diagnosis.total ?? 0} 次 · 降级 ${overview?.diagnosis.degraded ?? 0} 次`"
        />
      </el-col>
      <el-col :xs="12" :sm="12" :md="6">
        <StatCard
          label="待审批工单"
          :value="overview?.approvals.pending ?? 0"
          :status="(overview?.approvals.pending || 0) > 0 ? 'warning' : 'ok'"
          hint="L2 高危操作，30 分钟未审批自动拒绝"
        />
      </el-col>
    </el-row>

    <!-- 告警趋势 + 类型分布 -->
    <el-row :gutter="12" class="mt">
      <el-col :xs="24" :md="16">
        <div class="card">
          <h3 class="card-title">
            告警趋势（近 24 小时）
            <el-tag size="small" effect="plain">引擎 {{ overview?.platform.engine?.name || '-' }}</el-tag>
          </h3>
          <MetricChart :series="alertSeries" type="bar" :height="260" unit=" 条" />
        </div>
      </el-col>
      <el-col :xs="24" :md="8">
        <div class="card">
          <h3 class="card-title">中间件类型分布</h3>
          <ul v-if="typeDistribution.length > 0" class="dist-list">
            <li v-for="item in typeDistribution" :key="item.label">
              <span>{{ item.label }}</span>
              <div class="dist-bar">
                <span :style="{ width: `${(item.value / (typeDistribution[0]?.value || 1)) * 100}%` }" />
              </div>
              <span class="mono">{{ item.value }}</span>
            </li>
          </ul>
          <el-empty v-else description="尚未纳管实例" :image-size="72" />
        </div>
      </el-col>
    </el-row>

    <!-- 成本治理 / 诊断质量 / 审计概览 -->
    <el-row :gutter="12" class="mt">
      <el-col :xs="24" :md="8">
        <div class="card">
          <h3 class="card-title">诊断质量与成本</h3>
          <div class="kv-list">
            <div class="kv">
              <span>平均诊断耗时</span>
              <span class="mono">{{ formatNumber(Number(overview?.diagnosis.avg_duration_ms || 0), ' ms') }}</span>
            </div>
            <div class="kv">
              <span>累计 token 消耗</span>
              <span class="mono">{{ formatNumber(Number(overview?.diagnosis.total_tokens || 0)) }}</span>
            </div>
            <div class="kv">
              <span>反馈：有用 / 没用</span>
              <span class="mono">
                {{ overview?.diagnosis.feedback?.useful ?? 0 }} / {{ overview?.diagnosis.feedback?.useless ?? 0 }}
              </span>
            </div>
            <div class="kv">
              <span>知识库条目</span>
              <span class="mono">
                已发布 {{ overview?.knowledge?.published ?? 0 }} · 草稿 {{ overview?.knowledge?.draft ?? 0 }}
              </span>
            </div>
            <div class="kv">
              <span>日志告警事件</span>
              <span class="mono">
                待处理 {{ overview?.log_events?.pending ?? 0 }} · 已解决 {{ overview?.log_events?.resolved ?? 0 }}
              </span>
            </div>
          </div>
        </div>
      </el-col>
      <el-col :xs="24" :md="8">
        <div class="card">
          <h3 class="card-title">平台自检</h3>
          <div class="kv-list">
            <div class="kv">
              <span>监控数据源</span>
              <el-tag size="small" :type="overview?.platform.monitor_source === 'prometheus' ? 'success' : 'warning'">
                {{ overview?.platform.monitor_source === 'prometheus' ? 'Prometheus' : '内置模拟器' }}
              </el-tag>
            </div>
            <div class="kv">
              <span>缓存 / 队列</span>
              <span class="mono">{{ overview?.platform.cache_kind }}</span>
            </div>
            <div class="kv">
              <span>AI 引擎</span>
              <el-tag
                size="small"
                :type="overview?.platform.engine?.available ? (overview?.platform.engine?.degraded ? 'warning' : 'success') : 'danger'"
              >
                {{ overview?.platform.engine?.name || '-' }}
              </el-tag>
            </div>
            <div class="kv">
              <span>成本熔断</span>
              <el-tag size="small" :type="overview?.platform.cost_tripped ? 'danger' : 'success'">
                {{ overview?.platform.cost_tripped ? '已熔断' : '正常' }}
              </el-tag>
            </div>
            <div class="kv">
              <span>通知渠道</span>
              <span class="row">
                <el-tag
                  v-for="channel in overview?.platform.notify || []"
                  :key="channel.channel"
                  size="small"
                  :type="channel.enabled ? 'success' : 'info'"
                  effect="plain"
                >
                  {{ channel.channel }}
                </el-tag>
              </span>
            </div>
          </div>
        </div>
      </el-col>
      <el-col :xs="24" :md="8">
        <div class="card">
          <h3 class="card-title">
            近 24 小时操作审计
            <el-button text size="small" @click="router.push({ name: 'audit' })">查看全部</el-button>
          </h3>
          <ul v-if="(overview?.audit.by_action || []).length > 0" class="action-list">
            <li v-for="item in overview?.audit.by_action || []" :key="item.action_type">
              <code>{{ item.action_type }}</code>
              <span class="mono">{{ item.total }}</span>
            </li>
          </ul>
          <el-empty v-else description="暂无审计记录" :image-size="72" />
        </div>
      </el-col>
    </el-row>
  </div>
</template>

<style scoped>
.mt {
  margin-top: 12px;
}

.dist-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.dist-list li {
  display: grid;
  grid-template-columns: 84px 1fr 40px;
  align-items: center;
  gap: 10px;
  font-size: 12.5px;
}

.dist-bar {
  height: 6px;
  border-radius: var(--r-pill);
  background: var(--c-surface-2);
  overflow: hidden;
}

.dist-bar span {
  display: block;
  height: 100%;
  background: var(--c-accent);
  border-radius: var(--r-pill);
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

.action-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.action-list li {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  font-size: 12.5px;
}

.action-list code {
  color: var(--c-text-2);
}
</style>
