<script setup lang="ts">
/**
 * 全局大盘（8.2 系统模块）。
 *
 * 数据来源：/api/system/overview（实例健康、告警趋势、诊断质量与成本、平台自检）。
 * 刷新策略：进入页面即拉取；提供手动刷新；60s 静默轮询兼顾大盘延迟目标，
 * 但页面处于后台标签页时暂停轮询，回到前台立即补刷一次。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { systemApi } from '@/api'
import { toastError } from '@/api/http'
import type { Overview } from '@/api/types'
import EmptyGuide from '@/components/EmptyGuide.vue'
import StatCard from '@/components/StatCard.vue'
import MetricChart from '@/components/MetricChart.vue'
import { useUserStore } from '@/stores/user'
import { formatDuration, formatNumber, formatPercent, mwTypeLabels } from '@/utils/format'

const router = useRouter()
const store = useUserStore()
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

/**
 * 是否处于「全新系统」状态：一个实例都还没有。
 *
 * 这时大盘全是 0 与空图，用户看不出下一步该点哪——必须显式给出接入引导，
 * 并说明「集成中心」与「中间件纳管」两条路径的区别（这是首次使用最常走错的地方）。
 */
const isFresh = computed(() => Boolean(overview.value) && (overview.value?.instances.total ?? 0) === 0)

/** 引导入口按权限收敛：没有资源读权限就不展示跳转按钮（只读角色也能看大盘）。 */
const canAccessResources = computed(() => store.can('middleware:read'))

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

/** 切回前台立即补刷一次；后台标签页不主动轮询。 */
function onVisibilityChange(): void {
  if (document.hidden) {
    return
  }
  void load()
}

onMounted(async () => {
  await load()
  // 大盘延迟目标 ≤30s（设计文档 10），这里用 60s 轮询兼顾成本。
  timer = window.setInterval(() => {
    if (!document.hidden) {
      void load()
    }
  }, 60_000)
  document.addEventListener('visibilitychange', onVisibilityChange)
})

onBeforeUnmount(() => {
  if (timer) {
    window.clearInterval(timer)
  }
  document.removeEventListener('visibilitychange', onVisibilityChange)
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

    <!-- 全新系统：先告诉用户下一步该点哪，而不是留一堆 0 和空图 -->
    <EmptyGuide
      v-if="isFresh"
      class="mb"
      title="还没有纳管任何中间件实例"
      description="推荐用「集成中心」接入：平台会一并完成只读监控账号、Exporter 与 Prometheus 抓取目标；「中间件纳管」只做登记，适合已有 Exporter 且 Prometheus 已经抓得到的实例。"
      :steps="[
        '在集成中心选择组件（Redis / MySQL / PostgreSQL / Kafka / Elasticsearch / Nginx）',
        '填写连接地址与账号，可勾选由平台代建只读监控账号',
        '保存后约 30 秒，指标出现在「统一监控」，即可使用 AI 诊断',
      ]"
      :primary-text="canAccessResources ? '去集成中心接入' : ''"
      primary-to="integrations"
      :secondary-text="canAccessResources ? '手工纳管实例' : ''"
      secondary-to="middlewares"
    />

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
          <MetricChart :series="alertSeries" type="line" :height="260" unit=" 条" />
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
                {{ overview?.platform.monitor_source === 'prometheus' ? 'Prometheus' : '无数据源' }}
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

.mb {
  margin-bottom: 12px;
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
