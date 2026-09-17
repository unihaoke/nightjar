<script setup lang="ts">
/**
 * 列表自动刷新开关。
 *
 * 除了开关本身，还要回答用户两个问题：
 *   1. 多久刷一次？（把间隔写进文案，而不是让用户猜）
 *   2. 到底刷没刷？（展示「更新于」，否则开关开了也没感知）
 */
import { computed } from 'vue'
import { formatTime } from '@/utils/format'

const props = withDefaults(
  defineProps<{
    /** 是否开启自动刷新（配合 useListPage 的 polling）。 */
    modelValue: boolean
    /** 轮询间隔（毫秒），用于文案展示。 */
    intervalMs?: number
    /** 最后一次加载成功的时刻（ISO 字符串）。 */
    lastLoadedAt?: string
    /** 是否禁用（如页面不支持轮询）。 */
    disabled?: boolean
  }>(),
  { intervalMs: 0, lastLoadedAt: '', disabled: false },
)

const emit = defineEmits<{
  'update:modelValue': [value: boolean]
  refresh: []
}>()

/** 间隔文案：60000 → 「60 秒」。 */
const intervalText = computed(() => {
  if (!props.intervalMs) {
    return ''
  }
  const seconds = Math.round(props.intervalMs / 1000)
  return seconds >= 60 ? `${Math.round(seconds / 60)} 分钟` : `${seconds} 秒`
})

const label = computed(() => (intervalText.value ? `自动刷新（${intervalText.value}）` : '自动刷新'))
</script>

<template>
  <div class="refresh-control">
    <el-checkbox
      :model-value="modelValue"
      :disabled="disabled || !intervalMs"
      size="small"
      @update:model-value="emit('update:modelValue', $event)"
    >
      {{ label }}
    </el-checkbox>
    <span v-if="lastLoadedAt" class="muted refresh-at">更新于 {{ formatTime(lastLoadedAt, 'HH:mm:ss') }}</span>
    <el-button text size="small" :icon="'Refresh'" @click="emit('refresh')">刷新</el-button>
  </div>
</template>

<style scoped>
.refresh-control {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.refresh-at {
  font-size: 11.5px;
}
</style>
