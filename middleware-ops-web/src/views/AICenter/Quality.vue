<script setup lang="ts">
/** AI 质量与成本面板：六道护栏参数、质量基线、成本治理状态。 */
import { computed, onMounted, ref } from 'vue'
import { aiApi, systemApi } from '@/api'
import { toastError } from '@/api/http'
import type { SystemInfo } from '@/api/types'
import StatCard from '@/components/StatCard.vue'
import { formatNumber, formatPercent } from '@/utils/format'

const loading = ref(false)
const quality = ref<Record<string, unknown> | null>(null)
const guardrail = ref<Record<string, unknown> | null>(null)
const cost = ref<Record<string, unknown> | null>(null)
const info = ref<SystemInfo | null>(null)

/** 反馈统计。 */
const feedback = computed(() => (quality.value?.feedback as Record<string, number>) || {})

/** 采纳率。 */
const adoptionRate = computed(() => formatPercent(Number(quality.value?.adoption_rate || 0) * 100, 1))

/** 成本使用率。 */
const costUsage = computed(() => {
  const used = Number(cost.value?.user_tokens || 0)
  const quota = Number(cost.value?.user_quota || 0)
  if (!quota) {
    return 0
  }
  return Math.min(Math.round((used / quota) * 100), 100)
})

/** 护栏参数展示行（与设计文档第五章一一对应）。 */
const guardrailRows = computed(() => {
  const g = guardrail.value || {}
  return [
    { key: '① 上下文预算', value: `输入 ${g.input_token_budget ?? '-'} tokens / 输出 ${g.output_token_budget ?? '-'} tokens` },
    { key: '① 采集配额', value: `日志 ${g.log_context_lines ?? '-'} 行 × ${g.max_log_sources ?? '-'} 来源` },
    { key: '② 防死循环', value: `最大 ${g.max_steps ?? '-'} 步 · 同指纹重复 ${g.loop_threshold ?? '-'} 次即终止` },
    { key: '③ 并发与超时', value: `并发 ≤ ${g.max_concurrency ?? '-'} · 工具 10s · 任务 120s` },
    { key: '④ SQL 规则校验', value: `强制 LIMIT ${g.sql_default_limit ?? '-'}（上限 ${g.sql_max_limit ?? '-'}）+ 只读` },
    { key: '⑥ 确定性缓存', value: `${g.cache_ttl_hours ?? '-'} 小时` },
    { key: '⑥ 日预算', value: `平台 ${g.daily_token_quota ?? '-'} · 单用户 ${g.per_user_quota ?? '-'} tokens` },
    { key: '⑤ 参考案例阈值', value: `相似度 ≥ ${g.vector_threshold ?? '-'} 仅作参考展示` },
  ]
})

/** 工具白名单。 */
const tools = ref<string[]>([])
const evalSetSize = ref(0)

/** 加载数据。 */
async function load(): Promise<void> {
  loading.value = true
  try {
    const [qualityResult, systemResult] = await Promise.all([aiApi.quality(), systemApi.info()])
    quality.value = qualityResult.quality
    guardrail.value = qualityResult.guardrail
    cost.value = qualityResult.cost
    tools.value = qualityResult.tools || []
    evalSetSize.value = qualityResult.eval_set_size || 0
    info.value = systemResult
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page" v-loading="loading">
    <div class="page-header">
      <div>
        <h2 class="page-title">质量与成本</h2>
        <p class="page-subtitle">六道护栏参数、诊断质量基线与成本治理状态（设计文档第五章）</p>
      </div>
      <el-button :icon="'Refresh'" size="small" @click="load">刷新</el-button>
    </div>

    <el-row :gutter="12">
      <el-col :xs="12" :sm="12" :md="6">
        <StatCard
          label="诊断采纳率"
          :value="adoptionRate"
          :status="Number(quality?.adoption_rate || 0) >= 0.7 ? 'ok' : 'warning'"
          :hint="`已评价 ${quality?.rated_count ?? 0} 次`"
        />
      </el-col>
      <el-col :xs="12" :sm="12" :md="6">
        <StatCard
          label="平均诊断耗时"
          :value="formatNumber(Number(quality?.avg_duration_ms || 0), ' ms')"
          :status="Number(quality?.avg_duration_ms || 0) < 30000 ? 'ok' : 'warning'"
          hint="目标 P95 < 30s"
        />
      </el-col>
      <el-col :xs="12" :sm="12" :md="6">
        <StatCard
          label="降级次数"
          :value="Number(quality?.degraded_count ?? 0)"
          :status="Number(quality?.degraded_count || 0) === 0 ? 'ok' : 'warning'"
          hint="第三方失败自动降级本地/规则引擎"
        />
      </el-col>
      <el-col :xs="12" :sm="12" :md="6">
        <StatCard
          label="用户日预算使用率"
          :value="`${costUsage}%`"
          :status="costUsage >= 80 ? 'critical' : costUsage >= 60 ? 'warning' : 'ok'"
          :hint="`${cost?.user_tokens ?? 0} / ${cost?.user_quota ?? 0} tokens`"
        />
      </el-col>
    </el-row>

    <el-row :gutter="12" class="mt">
      <el-col :xs="24" :md="12">
        <div class="card">
          <h3 class="card-title">护栏参数（引擎层统一生效）</h3>
          <div class="kv-list">
            <div v-for="row in guardrailRows" :key="row.key" class="kv">
              <span>{{ row.key }}</span>
              <span class="mono">{{ row.value }}</span>
            </div>
          </div>
        </div>
      </el-col>
      <el-col :xs="24" :md="12">
        <div class="card">
          <h3 class="card-title">只读工具集与评测集</h3>
          <p class="hint">AI 只挂载以下只读工具，执行权永远保留给人（5.5）：</p>
          <div class="chips">
            <el-tag v-for="tool in tools" :key="tool" size="small" effect="plain">{{ tool }}</el-tag>
          </div>
          <el-divider />
          <div class="kv-list">
            <div class="kv">
              <span>内置评测集用例</span>
              <span class="mono">{{ evalSetSize }} 条</span>
            </div>
            <div class="kv">
              <span>评测集要求</span>
              <span class="mono">20-50 条典型故障，引擎/模型切换后回归</span>
            </div>
            <div class="kv">
              <span>反馈分布</span>
              <span class="mono">
                有用 {{ feedback.useful ?? 0 }} · 采纳 {{ feedback.adopted ?? 0 }} · 没用 {{ feedback.useless ?? 0 }}
              </span>
            </div>
            <div class="kv">
              <span>累计 token</span>
              <span class="mono">{{ formatNumber(Number(quality?.total_tokens || 0)) }}</span>
            </div>
            <div class="kv">
              <span>成本熔断</span>
              <el-tag size="small" :type="cost?.tripped ? 'danger' : 'success'">
                {{ cost?.tripped ? '已熔断' : '正常' }}
              </el-tag>
            </div>
          </div>
        </div>
      </el-col>
    </el-row>

    <div class="card mt">
      <h3 class="card-title">能力矩阵（声明与实现一致）</h3>
      <div class="table-scroll">
        <el-table :data="info?.capability_matrix || []" size="small">
          <el-table-column prop="mw_type" label="中间件" width="140" />
          <el-table-column label="纳管" width="80">
            <template #default="{ row }">
              <el-icon :class="row.manage ? 'text-success' : 'muted'"><CircleCheck v-if="row.manage" /><Close v-else /></el-icon>
            </template>
          </el-table-column>
          <el-table-column label="监控指标" width="100">
            <template #default="{ row }">
              <el-icon :class="row.metrics ? 'text-success' : 'muted'"><CircleCheck v-if="row.metrics" /><Close v-else /></el-icon>
            </template>
          </el-table-column>
          <el-table-column label="阈值告警" width="100">
            <template #default="{ row }">
              <el-icon :class="row.alerts ? 'text-success' : 'muted'"><CircleCheck v-if="row.alerts" /><Close v-else /></el-icon>
            </template>
          </el-table-column>
          <el-table-column label="AI 诊断" width="100">
            <template #default="{ row }">
              <el-icon :class="row.ai_diagnose ? 'text-success' : 'muted'"><CircleCheck v-if="row.ai_diagnose" /><Close v-else /></el-icon>
            </template>
          </el-table-column>
          <el-table-column prop="note" label="备注" min-width="200" />
        </el-table>
      </div>
    </div>
  </div>
</template>

<style scoped>
.mt {
  margin-top: 12px;
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

.hint {
  margin: 0 0 8px;
  font-size: 12.5px;
  color: var(--c-text-3);
}

.chips {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}
</style>
