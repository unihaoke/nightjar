<script setup lang="ts" generic="T">
/**
 * 列表容器：把「筛选条 + 空态 + 错误重试 + 桌面表格 / 移动卡片 + 分页」收成一个外壳。
 *
 * 抽取动机：此前"移动端把表格换成卡片"是在每个页面手写两套模板
 * （桌面 `<el-table>` + 移动 `v-if="app.isMobile"` 的卡片列表，再加两套分页），
 * 成本太高导致只有 2/16 个页面真正适配了移动端。
 * 现在页面只需提供 `#table` 与（可选的）`#card` 两个插槽：
 *   - 提供 `#card`：移动端自动渲染卡片；
 *   - 未提供：移动端仍渲染表格，页面可后续渐进补齐。
 *
 * 与 `useListPage` 配套使用，但本身不依赖它（任何 items/total/page 都能接）。
 */
import { computed } from 'vue'
import { useAppStore } from '@/stores/app'

const props = withDefaults(
  defineProps<{
    /** 当前页数据。 */
    items: T[]
    /** 加载中（渲染遮罩，同时避免空态闪烁）。 */
    loading?: boolean
    /** 最近一次错误文案；有值且无数据时展示可重试的空态。 */
    error?: string
    /** 总条数。 */
    total?: number
    /** 当前页码。 */
    page?: number
    /** 每页条数。 */
    pageSize?: number
    /** 卡片标题（原页面在列表卡片上已有的标题，如「执行历史」）。 */
    title?: string
    /** 空态文案。 */
    emptyText?: string
    /** 卡片列表的 key 字段，缺省取 id。 */
    rowKey?: string
    /** 是否展示分页，默认 true。 */
    showPagination?: boolean
    /** 每页条数候选项。 */
    pageSizes?: number[]
  }>(),
  {
    loading: false,
    error: '',
    total: 0,
    page: 1,
    pageSize: 20,
    title: '',
    emptyText: '没有匹配的数据',
    rowKey: 'id',
    showPagination: true,
    pageSizes: () => [20, 50, 100],
  },
)

const emit = defineEmits<{
  'update:page': [value: number]
  'update:pageSize': [value: number]
  retry: []
}>()

defineSlots<{
  /** 筛选条（渲染在独立卡片里）。 */
  filters?: () => unknown
  /** 自定义空态（未提供时渲染默认 el-empty）。需要给 CTA 时用 EmptyGuide 覆盖。 */
  empty?: () => unknown
  /** 列表上方的操作区（如批量按钮）。 */
  toolbar?: () => unknown
  /** 桌面端表格。 */
  table?: () => unknown
  /** 移动端单条卡片，作用域插槽提供 row。 */
  card?: (props: { row: T; index: number }) => unknown
}>()

const app = useAppStore()

/** 是否移动端视口。 */
const isMobile = computed(() => app.isMobile)

/** 移动端分页精简，桌面端带总数与每页条数选择。 */
const pagerLayout = computed(() => (isMobile.value ? 'prev, pager, next' : 'total, sizes, prev, pager, next'))

/** 无数据但在非首页时仍要给出分页，否则用户翻不回去。 */
const showPager = computed(() => props.showPagination && (props.total > 0 || props.page > 1))

/** 卡片 key：优先业务主键，缺失时退回下标。 */
function keyOf(row: T, index: number): string | number {
  const value = (row as Record<string, unknown> | null)?.[props.rowKey]
  return typeof value === 'number' || typeof value === 'string' ? value : index
}
</script>

<template>
  <div class="list">
    <div v-if="$slots.filters" class="card filters">
      <slot name="filters" />
    </div>

    <div class="card list-body" v-loading="loading">
      <h3 v-if="title" class="card-title">{{ title }}</h3>

      <div v-if="$slots.toolbar" class="list-toolbar">
        <slot name="toolbar" />
      </div>

      <!-- 失败且无数据：给出原因与重试入口，而不是只弹一条 4 秒后消失的 toast -->
      <el-empty v-if="error && items.length === 0" :description="error" :image-size="72">
        <el-button size="small" type="primary" :icon="'Refresh'" @click="emit('retry')">重试</el-button>
      </el-empty>

      <!-- 空态：页面可用 #empty 插槽塞入带 CTA 的引导 -->
      <slot v-else-if="items.length === 0 && !loading && $slots.empty" name="empty" />
      <el-empty v-else-if="items.length === 0 && !loading" :description="emptyText" :image-size="72" />

      <!-- 移动端：卡片列表（未提供 card 插槽时退回表格，保证可渐进迁移） -->
      <template v-else-if="isMobile && $slots.card">
        <div v-for="(row, index) in items" :key="keyOf(row, index)" class="list-card">
          <slot name="card" :row="row" :index="index" />
        </div>
      </template>

      <slot v-else name="table" />
    </div>

    <el-pagination
      v-if="showPager"
      class="pager"
      :current-page="page"
      :page-size="pageSize"
      :total="total"
      :page-sizes="pageSizes"
      :layout="pagerLayout"
      :small="isMobile"
      :pager-count="isMobile ? 5 : 7"
      @update:current-page="emit('update:page', $event)"
      @update:page-size="emit('update:pageSize', $event)"
    />
  </div>
</template>

<style scoped>
.filters {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  align-items: center;
  margin-bottom: var(--sp-3);
  padding: var(--sp-3);
}

.list-body {
  padding: var(--sp-3) var(--sp-4);
}

.list-toolbar {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: var(--sp-3);
}

.list-card {
  border: 1px solid var(--c-border);
  border-radius: var(--r-lg);
  padding: 12px 14px;
}

.list-card + .list-card {
  margin-top: 10px;
}

.pager {
  margin-top: var(--sp-3);
  justify-content: flex-end;
}

/* 移动端：筛选控件一律占满一行。
   用 :deep 是因为插槽内容由父组件编译，带的是父组件的 scopeId。 */
@media (max-width: 767px) {
  .filters :deep(.filter-item) {
    width: 100%;
  }

  .pager {
    justify-content: center;
  }
}
</style>
