<script setup lang="ts">
/**
 * 指标卡片：用于大盘与监控页的关键数值展示。
 *
 * 视觉规则：不使用通用「三色卡」套路 —— 卡片仅承载数值与语义状态，
 * 状态用左侧 2px 状态条 + 文本标签表达，避免大面积着色。
 */
withDefaults(
  defineProps<{
    label: string
    value: string | number
    /** 辅助说明（如环比、阈值）。 */
    hint?: string
    /** 语义状态，仅影响左侧状态条与数值颜色。 */
    status?: 'neutral' | 'ok' | 'warning' | 'critical'
    /** 数值单位。 */
    unit?: string
  }>(),
  { status: 'neutral', hint: '', unit: '' },
)
</script>

<template>
  <div class="stat-card" :class="`is-${status}`">
    <div class="stat-head">
      <span class="stat-label">{{ label }}</span>
      <slot name="extra" />
    </div>
    <div class="stat-value">
      <span class="stat-number">{{ value }}</span>
      <span v-if="unit" class="stat-unit">{{ unit }}</span>
    </div>
    <p v-if="hint" class="stat-hint">{{ hint }}</p>
  </div>
</template>

<style scoped>
.stat-card {
  position: relative;
  background: var(--c-surface);
  border: 1px solid var(--c-border);
  border-radius: var(--r-lg);
  padding: 12px 14px 12px 16px;
  height: 100%;
  overflow: hidden;
}

.stat-card::before {
  content: '';
  position: absolute;
  left: 0;
  top: 0;
  bottom: 0;
  width: 2px;
  background: var(--c-border-strong);
}

.stat-card.is-ok::before {
  background: var(--c-success);
}

.stat-card.is-warning::before {
  background: var(--c-warning);
}

.stat-card.is-critical::before {
  background: var(--c-danger);
}

.stat-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.stat-label {
  font-size: 12.5px;
  color: var(--c-text-3);
}

.stat-value {
  display: flex;
  align-items: baseline;
  gap: 4px;
  margin-top: 2px;
}

.stat-number {
  font-size: 24px;
  font-weight: 600;
  letter-spacing: -0.02em;
  font-variant-numeric: tabular-nums;
  line-height: 1.2;
}

.is-ok .stat-number {
  color: var(--c-success);
}

.is-warning .stat-number {
  color: var(--c-warning);
}

.is-critical .stat-number {
  color: var(--c-danger);
}

.stat-unit {
  font-size: 12px;
  color: var(--c-text-3);
}

.stat-hint {
  margin: 4px 0 0;
  font-size: 11.5px;
  color: var(--c-text-3);
}
</style>
