<script setup lang="ts">
/** 操作级别标签（L0/L1/L2，设计文档 4.6）。 */
import { computed } from 'vue'

const props = defineProps<{ level?: string; showDesc?: boolean }>()

const meta = computed(() => {
  switch ((props.level || '').toUpperCase()) {
    case 'L0':
      return { type: 'info' as const, text: 'L0 只读', desc: '直接执行' }
    case 'L1':
      return { type: 'warning' as const, text: 'L1 低危', desc: '直接执行并留痕' }
    case 'L2':
      return { type: 'danger' as const, text: 'L2 高危', desc: '需二次确认 + 审批' }
    default:
      return { type: 'info' as const, text: props.level || '-', desc: '' }
  }
})
</script>

<template>
  <el-tag :type="meta.type" size="small" effect="light">
    {{ meta.text }}<span v-if="showDesc && meta.desc"> · {{ meta.desc }}</span>
  </el-tag>
</template>
