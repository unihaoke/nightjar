<script setup lang="ts">
/**
 * ECharts 折线图组件。
 *
 * 设计要点：
 *   - 按需引入 ECharts 模块，控制产物体积；
 *   - 图表随容器尺寸自适应（移动端横竖屏切换）；
 *   - 遵守深浅主题：颜色从 CSS 变量读取，主题切换时重建实例。
 */
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import * as echarts from 'echarts/core'
import { LineChart, BarChart } from 'echarts/charts'
import { GridComponent, TooltipComponent, LegendComponent, DataZoomComponent, MarkLineComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import type { MetricSample } from '@/api/types'
import { useUserStore } from '@/stores/user'

echarts.use([
  LineChart,
  BarChart,
  GridComponent,
  TooltipComponent,
  LegendComponent,
  DataZoomComponent,
  MarkLineComponent,
  CanvasRenderer,
])

const props = withDefaults(
  defineProps<{
    /** 采样序列（按时间升序）。 */
    series: MetricSample[]
    /** 图表类型。 */
    type?: 'line' | 'bar'
    /** 单位（展示在 tooltip 与 Y 轴）。 */
    unit?: string
    /** 告警阈值线（warning / critical）。 */
    warning?: number
    critical?: number
    /** 高度（px）。 */
    height?: number
    /** 是否显示加载态。 */
    loading?: boolean
  }>(),
  { type: 'line', unit: '', warning: 0, critical: 0, height: 260, loading: false },
)

const container = ref<HTMLDivElement | null>(null)
const store = useUserStore()
let chart: echarts.ECharts | null = null

const hasData = computed(() => props.series.length > 0)

/** 读取当前主题下的绘图配色。 */
function palette(): { text: string; text3: string; border: string; accent: string; warning: string; danger: string; area: string } {
  const styles = getComputedStyle(document.documentElement)
  const read = (name: string, fallback: string): string => styles.getPropertyValue(name).trim() || fallback
  return {
    text: read('--c-text', '#171a20'),
    text3: read('--c-text-3', '#7b8494'),
    border: read('--c-border', '#e3e6ec'),
    accent: read('--c-accent', '#1d5fd8'),
    warning: read('--c-warning', '#9a6206'),
    danger: read('--c-danger', '#b8232f'),
    area: read('--c-accent-soft', 'rgba(29,95,216,0.12)'),
  }
}

/** 构造 ECharts 配置。 */
function buildOption(): echarts.EChartsCoreOption {
  const c = palette()
  const times = props.series.map((item) => item.timestamp)
  const values = props.series.map((item) => item.value)
  const markLines: Record<string, unknown>[] = []
  if (props.warning) {
    markLines.push({
      yAxis: props.warning,
      lineStyle: { color: c.warning, type: 'dashed', width: 1 },
      label: { formatter: `警告 ${props.warning}`, color: c.warning, fontSize: 10, position: 'insideEndTop' },
    })
  }
  if (props.critical) {
    markLines.push({
      yAxis: props.critical,
      lineStyle: { color: c.danger, type: 'dashed', width: 1 },
      label: { formatter: `严重 ${props.critical}`, color: c.danger, fontSize: 10, position: 'insideEndBottom' },
    })
  }

  return {
    animationDuration: 240,
    grid: { left: 8, right: 12, top: 16, bottom: 8, containLabel: true },
    tooltip: {
      trigger: 'axis',
      backgroundColor: 'rgba(23,26,32,0.92)',
      borderWidth: 0,
      textStyle: { color: '#fff', fontSize: 12 },
      valueFormatter: (value: unknown) => `${value}${props.unit}`,
    },
    xAxis: {
      type: 'category',
      data: times,
      boundaryGap: props.type === 'bar',
      axisLine: { lineStyle: { color: c.border } },
      axisLabel: {
        color: c.text3,
        fontSize: 10,
        hideOverlap: true,
        formatter: (value: string) => (value.length > 11 ? value.slice(5, 16) : value),
      },
      axisTick: { show: false },
    },
    yAxis: {
      type: 'value',
      axisLine: { show: false },
      axisLabel: { color: c.text3, fontSize: 10 },
      splitLine: { lineStyle: { color: c.border, type: 'dotted' } },
    },
    series: [
      {
        type: props.type,
        data: values,
        smooth: props.type === 'line',
        symbol: 'none',
        lineStyle: { width: 1.8, color: c.accent },
        itemStyle: { color: c.accent },
        areaStyle:
          props.type === 'line'
            ? {
                color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
                  { offset: 0, color: c.area },
                  { offset: 1, color: 'rgba(0,0,0,0)' },
                ]),
              }
            : undefined,
        markLine: markLines.length > 0 ? { silent: true, symbol: 'none', data: markLines } : undefined,
      },
    ],
  }
}

/** 渲染或更新图表。 */
function render(): void {
  if (!container.value) {
    return
  }
  if (!chart) {
    chart = echarts.init(container.value)
  }
  chart.setOption(buildOption(), true)
  chart.resize()
}

onMounted(() => {
  if (hasData.value) {
    render()
  }
})

watch(
  () => props.series,
  () => {
    if (hasData.value) {
      render()
    }
  },
  { deep: false },
)

// 主题切换后重建配色。
watch(
  () => store.theme,
  () => {
    if (hasData.value) {
      render()
    }
  },
)

// 容器尺寸变化（含移动端旋转）时自适应。
let observer: ResizeObserver | null = null
onMounted(() => {
  if (typeof ResizeObserver === 'undefined' || !container.value) {
    return
  }
  observer = new ResizeObserver(() => chart?.resize())
  observer.observe(container.value)
})

onBeforeUnmount(() => {
  observer?.disconnect()
  chart?.dispose()
  chart = null
})
</script>

<template>
  <div class="chart-wrap" :style="{ height: `${height}px` }">
    <div v-if="loading" class="chart-state">
      <el-skeleton :rows="4" animated />
    </div>
    <div v-else-if="!hasData" class="chart-state">
      <el-empty description="暂无采样数据" :image-size="72" />
    </div>
    <div v-show="hasData && !loading" ref="container" class="chart-canvas" />
  </div>
</template>

<style scoped>
.chart-wrap {
  width: 100%;
  position: relative;
}

.chart-canvas {
  width: 100%;
  height: 100%;
}

.chart-state {
  height: 100%;
  display: grid;
  place-items: center;
  padding: var(--sp-3);
}
</style>
