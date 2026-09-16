<script setup lang="ts">
/**
 * AI 诊断报告结构化展示（对齐设计文档 5.6 质量护栏的输出约束）。
 *
 * 展示规则：
 *   - 置信度低于阈值或无硬证据时，显式标注「推测 / 低置信度」，不做美化掩盖；
 *   - 每条结论都展示证据引用；无证据项以警示色标注；
 *   - 修复建议展示操作级别（L0/L1/L2），L2 明确提示需审批；
 *   - 参考案例单独分区，并注明「不作为最终诊断」（5.7）。
 */
import { computed } from 'vue'
import type { DiagnosisReport, KnowledgeReference } from '@/api/types'
import { formatPercent, horizonLabels } from '@/utils/format'
import LevelTag from './LevelTag.vue'

const props = defineProps<{
  report: DiagnosisReport
  references?: KnowledgeReference[] | null
  warnings?: string[] | null
  /** 被截断 / 缺失的上下文维度（5.2 / 5.4）。 */
  truncated?: string[] | null
  missing?: string[] | null
  meta?: {
    engine_name?: string
    engine_status?: string
    cache_hit?: boolean
    data_source?: string
    scope?: string
    prompt_tokens?: number
    duration_ms?: number
    cost_tokens?: number
  } | null
}>()

const emit = defineEmits<{ (e: 'feedback', value: 'useful' | 'useless' | 'adopted'): void }>()

/** 置信度百分比。 */
const confidencePercent = computed(() => Math.round((props.report?.confidence || 0) * 100))

/** 置信度语义色。 */
const confidenceStatus = computed(() => {
  const value = props.report?.confidence || 0
  if (value >= 0.75) {
    return 'ok' as const
  }
  if (value >= 0.5) {
    return 'warning' as const
  }
  return 'critical' as const
})

/** 证据覆盖率百分比。 */
const groundedPercent = computed(() => Math.round((props.report?.grounded_ratio || 0) * 100))

/** 是否为降级结论（规则引擎/半自动）。 */
const isFallback = computed(() => props.report && props.report.ai_available === false)

/** 按时间维度分组建议。 */
const suggestionGroups = computed(() => {
  const order: ('immediate' | 'short_term' | 'long_term')[] = ['immediate', 'short_term', 'long_term']
  return order
    .map((horizon) => ({
      horizon,
      label: horizonLabels[horizon],
      items: (props.report?.suggestions || []).filter((item) => item.horizon === horizon),
    }))
    .filter((group) => group.items.length > 0)
})

/** 证据来源展示名。 */
function sourceLabel(source: string): string {
  const map: Record<string, string> = {
    metric: '指标',
    log: '日志',
    config: '配置',
    code: '代码',
    knowledge: '知识库',
    user_input: '用户输入',
    unknown: '未标注',
  }
  return map[source] || source
}
</script>

<template>
  <div class="report">
    <!-- 质量提示：推测 / 低置信度 / 降级，必须置顶可见 -->
    <el-alert
      v-if="report.speculative"
      type="warning"
      :closable="false"
      show-icon
      title="该结论缺少硬证据，已标注为「推测」"
      description="请结合下方证据引用与待确认项人工复核后再执行任何操作。"
      class="report-alert"
    />
    <el-alert
      v-else-if="report.low_confidence"
      type="info"
      :closable="false"
      show-icon
      :title="`置信度 ${confidencePercent}%，低于阈值，建议人工复核`"
      class="report-alert"
    />
    <el-alert
      v-if="isFallback"
      type="warning"
      :closable="false"
      show-icon
      title="AI 引擎不可用，本结论由规则引擎生成（半自动）"
      :description="report.engine_note || '降级链已生效：第三方 → 本地 LLM → 规则引擎 + 知识库检索（设计文档 5.4）'"
      class="report-alert"
    />
    <el-alert
      v-for="(warning, index) in warnings || []"
      :key="`warn-${index}`"
      type="warning"
      :closable="false"
      show-icon
      :title="warning"
      class="report-alert"
    />

    <!-- 结论头部 -->
    <div class="report-head">
      <div class="head-main">
        <span class="head-label">根因结论</span>
        <p class="head-text">{{ report.root_cause || '（模型未给出根因）' }}</p>
      </div>
      <div class="head-metrics">
        <div class="metric-block" :class="`is-${confidenceStatus}`">
          <span class="metric-value">{{ confidencePercent }}%</span>
          <span class="metric-label">置信度</span>
        </div>
        <div class="metric-block">
          <span class="metric-value">{{ groundedPercent }}%</span>
          <span class="metric-label">证据覆盖率</span>
        </div>
      </div>
    </div>

    <div class="report-section">
      <h4 class="section-title">影响范围</h4>
      <p class="section-body">{{ report.impact_scope || '未说明' }}</p>
    </div>

    <!-- 证据引用 -->
    <div class="report-section">
      <h4 class="section-title">
        证据引用
        <span class="muted">（{{ (report.evidence || []).length }} 条）</span>
      </h4>
      <div v-if="(report.evidence || []).length === 0" class="empty-inline">
        <el-icon><WarningFilled /></el-icon>
        <span>模型未引用任何证据，结论可靠性无法验证</span>
      </div>
      <ul v-else class="evidence-list">
        <li v-for="(item, index) in report.evidence" :key="`ev-${index}`" :class="{ speculative: item.speculative }">
          <el-tag size="small" effect="plain" :type="item.speculative ? 'warning' : 'info'">
            {{ sourceLabel(item.source) }}
          </el-tag>
          <code v-if="item.ref" class="evidence-ref">{{ item.ref }}</code>
          <span class="evidence-detail">{{ item.detail }}</span>
          <el-tag v-if="item.speculative" size="small" type="warning" effect="dark">推测</el-tag>
        </li>
      </ul>
    </div>

    <!-- 修复建议 -->
    <div class="report-section">
      <h4 class="section-title">修复建议</h4>
      <div v-if="suggestionGroups.length === 0" class="muted">模型未给出修复建议</div>
      <div v-for="group in suggestionGroups" :key="group.horizon" class="suggestion-group">
        <p class="suggestion-horizon">{{ group.label }}</p>
        <div v-for="(item, index) in group.items" :key="`sg-${group.horizon}-${index}`" class="suggestion-item">
          <div class="suggestion-main">
            <span class="suggestion-action">{{ item.action }}</span>
            <div class="suggestion-tags">
              <el-tag size="small" effect="plain">风险：{{ item.risk || '未标注' }}</el-tag>
              <LevelTag :level="item.level" />
            </div>
          </div>
          <p v-if="item.level === 'L2'" class="suggestion-note">
            该建议为 L2 高危操作，需二次确认并提交审批（生产环境强制审批）。
          </p>
        </div>
      </div>
    </div>

    <!-- 待确认项 -->
    <div v-if="(report.pending_confirm || []).length > 0" class="report-section">
      <h4 class="section-title">待确认项</h4>
      <ul class="todo-list">
        <li v-for="(item, index) in report.pending_confirm" :key="`todo-${index}`">{{ item }}</li>
      </ul>
    </div>

    <!-- 缺失/截断维度（上下文预算与工具超时透明化） -->
    <div v-if="(missing || []).length > 0 || (truncated || []).length > 0" class="report-section">
      <h4 class="section-title">上下文完整性</h4>
      <div class="chips">
        <el-tag v-for="item in missing || []" :key="`miss-${item}`" type="warning" size="small" effect="light">
          缺失：{{ item }}
        </el-tag>
        <el-tag v-for="item in truncated || []" :key="`trunc-${item}`" type="info" size="small" effect="light">
          已截断：{{ item }}
        </el-tag>
      </div>
    </div>

    <!-- 参考案例（明确不作为结论） -->
    <div v-if="(references || []).length > 0" class="report-section reference-section">
      <h4 class="section-title">历史相似案例（仅作参考，不作为最终诊断）</h4>
      <div v-for="item in references || []" :key="`ref-${item.id}`" class="reference-item">
        <div class="reference-head">
          <span class="reference-title">{{ item.title }}</span>
          <el-tag size="small" effect="plain">相似度 {{ formatPercent(item.similarity * 100, 0) }}</el-tag>
        </div>
        <p class="reference-excerpt">{{ item.excerpt }}</p>
      </div>
    </div>

    <!-- 执行元信息 -->
    <div v-if="meta" class="report-meta">
      <span>引擎：{{ meta.engine_name || '-' }}</span>
      <span>状态：{{ meta.engine_status || '-' }}</span>
      <span>指标来源：{{ meta.data_source || '-' }}</span>
      <span v-if="meta.prompt_tokens">输入 tokens：{{ meta.prompt_tokens }}</span>
      <span v-if="meta.cost_tokens">消耗 tokens：{{ meta.cost_tokens }}</span>
      <span v-if="meta.duration_ms">耗时：{{ meta.duration_ms }} ms</span>
      <span v-if="meta.cache_hit" class="text-success">命中确定性缓存（未消耗 token）</span>
      <span v-if="meta.scope">权限范围：{{ meta.scope }}</span>
    </div>

    <!-- 反馈（质量基线，5.6） -->
    <div class="report-actions">
      <span class="muted">这条诊断对你有帮助吗？</span>
      <el-button size="small" @click="emit('feedback', 'useful')">
        <el-icon><Select /></el-icon>有用
      </el-button>
      <el-button size="small" @click="emit('feedback', 'useless')">
        <el-icon><CloseBold /></el-icon>没用
      </el-button>
      <el-button size="small" type="primary" plain @click="emit('feedback', 'adopted')">
        <el-icon><CircleCheck /></el-icon>采纳结论
      </el-button>
    </div>
  </div>
</template>

<style scoped>
.report {
  display: flex;
  flex-direction: column;
  gap: var(--sp-4);
}

.report-alert + .report-alert {
  margin-top: -8px;
}

.report-head {
  display: flex;
  gap: var(--sp-4);
  flex-wrap: wrap;
  align-items: flex-start;
  justify-content: space-between;
}

.head-main {
  flex: 1 1 260px;
  min-width: 0;
}

.head-label {
  font-size: 12px;
  color: var(--c-text-3);
}

.head-text {
  margin: 4px 0 0;
  font-size: 15px;
  line-height: 1.6;
  font-weight: 500;
}

.head-metrics {
  display: flex;
  gap: var(--sp-4);
}

.metric-block {
  display: flex;
  flex-direction: column;
  align-items: flex-end;
}

.metric-value {
  font-size: 20px;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}

.metric-block.is-ok .metric-value {
  color: var(--c-success);
}

.metric-block.is-warning .metric-value {
  color: var(--c-warning);
}

.metric-block.is-critical .metric-value {
  color: var(--c-danger);
}

.metric-label {
  font-size: 11.5px;
  color: var(--c-text-3);
}

.report-section {
  border-top: 1px solid var(--c-border);
  padding-top: var(--sp-3);
}

.section-title {
  margin: 0 0 var(--sp-2);
  font-size: 13px;
  font-weight: 600;
  display: flex;
  align-items: center;
  gap: 6px;
}

.section-body {
  margin: 0;
  color: var(--c-text-2);
  font-size: 13.5px;
}

.empty-inline {
  display: flex;
  align-items: center;
  gap: 6px;
  color: var(--c-warning);
  font-size: 13px;
}

.evidence-list,
.todo-list {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.evidence-list li {
  display: flex;
  align-items: baseline;
  gap: 8px;
  flex-wrap: wrap;
  font-size: 13px;
  padding: 8px 10px;
  background: var(--c-surface-2);
  border-radius: var(--r-md);
}

.evidence-list li.speculative {
  border-left: 2px solid var(--c-warning);
}

.evidence-ref {
  color: var(--c-accent);
}

.evidence-detail {
  color: var(--c-text-2);
  flex: 1 1 auto;
  min-width: 0;
}

.todo-list li {
  font-size: 13px;
  color: var(--c-text-2);
  padding-left: 14px;
  position: relative;
}

.todo-list li::before {
  content: '';
  position: absolute;
  left: 2px;
  top: 8px;
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: var(--c-warning);
}

.suggestion-group + .suggestion-group {
  margin-top: var(--sp-3);
}

.suggestion-horizon {
  margin: 0 0 6px;
  font-size: 12px;
  font-weight: 600;
  color: var(--c-text-3);
  letter-spacing: 0.04em;
}

.suggestion-item {
  padding: 10px 12px;
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
}

.suggestion-item + .suggestion-item {
  margin-top: 8px;
}

.suggestion-main {
  display: flex;
  gap: 8px;
  align-items: flex-start;
  justify-content: space-between;
  flex-wrap: wrap;
}

.suggestion-action {
  flex: 1 1 220px;
  font-size: 13.5px;
  line-height: 1.55;
}

.suggestion-tags {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}

.suggestion-note {
  margin: 8px 0 0;
  font-size: 12px;
  color: var(--c-danger);
}

.chips {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}

.reference-section .reference-item {
  padding: 10px 12px;
  border: 1px dashed var(--c-border-strong);
  border-radius: var(--r-md);
}

.reference-item + .reference-item {
  margin-top: 8px;
}

.reference-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.reference-title {
  font-size: 13px;
  font-weight: 500;
}

.reference-excerpt {
  margin: 6px 0 0;
  font-size: 12.5px;
  color: var(--c-text-3);
  line-height: 1.6;
}

.report-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 14px;
  font-size: 11.5px;
  color: var(--c-text-3);
  border-top: 1px solid var(--c-border);
  padding-top: var(--sp-3);
}

.report-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  border-top: 1px solid var(--c-border);
  padding-top: var(--sp-3);
  font-size: 12.5px;
}
</style>
