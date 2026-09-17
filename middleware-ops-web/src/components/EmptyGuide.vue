<script setup lang="ts">
/**
 * 空态引导：把「这里什么都没有」变成「下一步该点哪」。
 *
 * 抽取动机：此前空态一律是一个 `el-empty` + 一句描述（如"尚未纳管实例"），
 * 用户看完不知道该去哪。新装系统第一次登录时尤其致命——大盘全是 0，
 * 而真正的入口（集成中心）埋在侧栏二级菜单里。
 *
 * 用法：给 `primaryTo`（路由名）即可跳转；不传则只 emit，由页面自行处理。
 */
import { useRouter } from 'vue-router'

const props = withDefaults(
  defineProps<{
    /** 主标题。 */
    title: string
    /** 补充说明。 */
    description?: string
    /** 有序步骤（1、2、3…）。 */
    steps?: string[]
    /** 主按钮文案。 */
    primaryText?: string
    /** 主按钮跳转的路由名。 */
    primaryTo?: string
    /** 次按钮文案。 */
    secondaryText?: string
    /** 次按钮跳转的路由名。 */
    secondaryTo?: string
    /** 紧凑模式（用于卡片内部的小空态）。 */
    compact?: boolean
  }>(),
  {
    description: '',
    steps: () => [],
    primaryText: '',
    primaryTo: '',
    secondaryText: '',
    secondaryTo: '',
    compact: false,
  },
)

const emit = defineEmits<{
  primary: []
  secondary: []
}>()

const router = useRouter()

function go(name: string): void {
  if (name) {
    void router.push({ name })
  }
}

function onPrimary(): void {
  emit('primary')
  go(props.primaryTo)
}

function onSecondary(): void {
  emit('secondary')
  go(props.secondaryTo)
}
</script>

<template>
  <div class="guide" :class="{ 'is-compact': compact }">
    <div class="guide-icon" aria-hidden="true">
      <el-icon><Guide /></el-icon>
    </div>
    <div class="guide-body">
      <h4 class="guide-title">{{ title }}</h4>
      <p v-if="description" class="guide-desc">{{ description }}</p>

      <ol v-if="steps.length > 0" class="guide-steps">
        <li v-for="step in steps" :key="step">{{ step }}</li>
      </ol>

      <div v-if="primaryText || secondaryText || $slots.default" class="guide-actions">
        <el-button v-if="primaryText" type="primary" size="small" @click="onPrimary">{{ primaryText }}</el-button>
        <el-button v-if="secondaryText" size="small" @click="onSecondary">{{ secondaryText }}</el-button>
        <slot />
      </div>
    </div>
  </div>
</template>

<style scoped>
.guide {
  display: flex;
  gap: 12px;
  padding: var(--sp-4);
  border: 1px dashed var(--c-accent);
  border-radius: var(--r-lg);
  background: var(--c-accent-soft);
}

.is-compact {
  padding: var(--sp-3);
  gap: 10px;
}

.guide-icon {
  flex: 0 0 30px;
  width: 30px;
  height: 30px;
  display: grid;
  place-items: center;
  border-radius: var(--r-md);
  background: var(--c-accent);
  color: #fff;
  font-size: 16px;
}

.is-compact .guide-icon {
  flex-basis: 26px;
  width: 26px;
  height: 26px;
  font-size: 14px;
}

.guide-body {
  flex: 1;
  min-width: 0;
}

.guide-title {
  margin: 0;
  font-size: 13.5px;
  font-weight: 600;
  color: var(--c-text);
}

.guide-desc {
  margin: 4px 0 0;
  font-size: 12.5px;
  line-height: 1.65;
  color: var(--c-text-2);
}

.guide-steps {
  margin: 8px 0 0;
  padding-left: 18px;
  font-size: 12.5px;
  line-height: 1.8;
  color: var(--c-text-2);
}

.guide-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 10px;
}

.is-compact .guide-actions {
  margin-top: 8px;
}
</style>
