<script setup lang="ts">
/**
 * 中间件纳管列表（4.1）。
 *
 * 列表骨架（加载/错误/分页/筛选同步地址栏/移动端卡片）由
 * `useListPage` + `ResponsiveList` 承担，本页只描述「有哪些筛选条件」与「每行长什么样」。
 */
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { middlewareApi } from '@/api'
import { toastError } from '@/api/http'
import type { MiddlewareInstance } from '@/api/types'
import { useListPage } from '@/composables/useListPage'
import EmptyGuide from '@/components/EmptyGuide.vue'
import ResponsiveList from '@/components/ResponsiveList.vue'
import { useUserStore } from '@/stores/user'
import { envLabels, envTagType, formatTime, mwTypeLabels, mwTypeTagType } from '@/utils/format'
import FormDialog from './FormDialog.vue'

const router = useRouter()
const store = useUserStore()

const list = useListPage<MiddlewareInstance>({
  fetch: (params, signal) => middlewareApi.list(params, signal),
  defaults: { keyword: '', mw_type: '', environment: '', group: '', page: 1, page_size: 20 },
})
const { query, items, total, loading, error } = list

const dialogVisible = ref(false)
const editing = ref<MiddlewareInstance | null>(null)

const typeOptions = [
  { value: 'redis', label: 'Redis' },
  { value: 'kafka', label: 'Kafka' },
  { value: 'mysql', label: 'MySQL' },
  { value: 'pg', label: 'PostgreSQL' },
  { value: 'es', label: 'Elasticsearch' },
  { value: 'nginx', label: 'Nginx' },
  { value: 'rabbitmq', label: 'RabbitMQ' },
]

/** 是否可写。 */
const canWrite = computed(() => store.can('middleware:write'))

/** 是否设置了业务筛选（用于区分"筛选无结果"与"系统里没有实例"）。 */
const hasFilter = computed(() =>
  Boolean(query.keyword || query.mw_type || query.environment || query.group),
)

/** 打开新增/编辑。 */
function openForm(item?: MiddlewareInstance): void {
  editing.value = item || null
  dialogVisible.value = true
}

/** 删除实例（prod 会转为审批工单）。 */
async function handleDelete(item: MiddlewareInstance): Promise<void> {
  const isProd = item.environment === 'prod'
  try {
    const { value } = await ElMessageBox.prompt(
      isProd
        ? `【生产环境】${item.name} 的删除属于 L2 高危操作，将创建审批工单。请填写申请理由：`
        : `确认删除实例 ${item.name}？该操作不可撤销。`,
      isProd ? '提交审批' : '删除确认',
      {
        confirmButtonText: isProd ? '提交审批' : '删除',
        cancelButtonText: '取消',
        inputPlaceholder: '申请理由（生产环境必填）',
        inputValidator: (input) => (isProd && !input ? '生产环境必须填写申请理由' : true),
        type: 'warning',
      },
    )
    const result = await middlewareApi.remove(item.id, value || undefined)
    if (result.ticket_id) {
      ElMessage({ type: 'success', message: `已创建审批工单 ${result.ticket_id}，审批通过后请人工执行` })
    } else {
      ElMessage({ type: 'success', message: '实例已删除' })
    }
    await list.load()
  } catch (err) {
    if (err !== 'cancel') {
      toastError(err)
    }
  }
}

/** 详细页跳转。 */
function openDetail(item: MiddlewareInstance): void {
  void router.push({ name: 'middleware-detail', params: { id: item.id } })
}
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2 class="page-title">中间件纳管</h2>
        <p class="page-subtitle">一期核心：Redis / Kafka / MySQL / PostgreSQL / Elasticsearch / Nginx</p>
      </div>
      <el-button v-if="canWrite" type="primary" :icon="'Plus'" @click="openForm()">新增实例</el-button>
    </div>

    <ResponsiveList
      :items="items"
      :loading="loading"
      :error="error"
      :total="total"
      :page="query.page"
      :page-size="query.page_size"
      empty-text="没有匹配的实例"
      @update:page="list.setPage"
      @update:page-size="list.setPageSize"
      @retry="list.load"
    >
      <template #filters>
        <el-input v-model="query.keyword" placeholder="按名称或地址搜索" clearable class="filter-item" @keyup.enter="list.search">
          <template #prefix><el-icon><Search /></el-icon></template>
        </el-input>
        <el-select v-model="query.mw_type" placeholder="全部类型" clearable class="filter-item">
          <el-option v-for="item in typeOptions" :key="item.value" :label="item.label" :value="item.value" />
        </el-select>
        <el-select v-model="query.environment" placeholder="全部环境" clearable class="filter-item">
          <el-option label="开发" value="dev" />
          <el-option label="预发" value="staging" />
          <el-option label="生产" value="prod" />
        </el-select>
        <el-input v-model="query.group" placeholder="分组" clearable class="filter-item" @keyup.enter="list.search" />
        <el-button type="primary" :icon="'Search'" @click="list.search">查询</el-button>
        <el-button :icon="'RefreshLeft'" @click="list.reset">重置</el-button>
      </template>

      <!-- 空态：区分「筛选无结果」与「系统里根本没有实例」两种情况 -->
      <template #empty>
        <EmptyGuide
          v-if="total === 0 && !hasFilter"
          compact
          title="还没有纳管任何实例"
          description="如果希望平台自动创建只读监控账号、拉起 Exporter 并接入 Prometheus，请走集成中心；这里只登记实例信息。"
          primary-text="去集成中心接入"
          primary-to="integrations"
          secondary-text="直接登记实例"
          @secondary="openForm()"
        />
        <el-empty v-else description="没有匹配的实例，试试调整筛选条件" :image-size="72" />
      </template>

      <!-- 移动端：卡片列表 -->
      <template #card="{ row }">
        <div class="card-head" @click="openDetail(row)">
          <span class="mw-name">{{ row.name }}</span>
          <el-tag size="small" :type="row.status === 1 ? 'success' : 'danger'" effect="light">
            {{ row.status === 1 ? '在线' : '离线' }}
          </el-tag>
        </div>
        <div class="mw-tags">
          <el-tag size="small" effect="plain" :type="mwTypeTagType[row.mw_type] || 'info'">
            {{ mwTypeLabels[row.mw_type] || row.mw_type }}
          </el-tag>
          <el-tag size="small" effect="plain" :type="envTagType[row.environment]">
            {{ envLabels[row.environment] }}
          </el-tag>
          <el-tag v-if="row.group_name" size="small" effect="plain">{{ row.group_name }}</el-tag>
        </div>
        <p class="mw-endpoint mono">{{ row.host }}:{{ row.port }}</p>
        <p class="mw-message muted">{{ row.last_message || '尚未探测' }}</p>
        <div class="mw-actions">
          <el-button size="small" @click="openDetail(row)">详情</el-button>
          <el-button v-if="canWrite" size="small" @click="openForm(row)">编辑</el-button>
          <el-button v-if="canWrite" size="small" type="danger" plain @click="handleDelete(row)">
            {{ row.environment === 'prod' ? '审批删除' : '删除' }}
          </el-button>
        </div>
      </template>

      <!-- 桌面端：表格 -->
      <template #table>
        <div class="table-scroll">
        <el-table :data="items" size="default" row-key="id" @row-click="openDetail">
          <el-table-column prop="name" label="实例名称" min-width="160" show-overflow-tooltip />
          <el-table-column label="类型" width="120">
            <template #default="{ row }">
              <el-tag size="small" effect="plain" :type="mwTypeTagType[row.mw_type] || 'info'">
                {{ mwTypeLabels[row.mw_type] || row.mw_type }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="地址" min-width="170">
            <template #default="{ row }">
              <span class="mono">{{ row.host }}:{{ row.port }}</span>
            </template>
          </el-table-column>
          <el-table-column label="环境" width="90">
            <template #default="{ row }">
              <el-tag size="small" effect="plain" :type="envTagType[row.environment]">
                {{ envLabels[row.environment] }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="group_name" label="分组" width="110" show-overflow-tooltip />
          <el-table-column label="状态" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.status === 1 ? 'success' : 'danger'" effect="light">
                {{ row.status === 1 ? '在线' : '离线' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="last_message" label="最近探测" min-width="200" show-overflow-tooltip />
          <el-table-column label="探测时间" width="170">
            <template #default="{ row }">{{ formatTime(row.last_check_at) }}</template>
          </el-table-column>
          <el-table-column label="操作" width="200" fixed="right">
            <template #default="{ row }">
              <el-button text size="small" @click.stop="openDetail(row)">详情</el-button>
              <el-button v-if="canWrite" text size="small" @click.stop="openForm(row)">编辑</el-button>
              <el-button v-if="canWrite" text size="small" type="danger" @click.stop="handleDelete(row)">
                {{ row.environment === 'prod' ? '审批删除' : '删除' }}
              </el-button>
            </template>
          </el-table-column>
        </el-table>
        </div>
      </template>
    </ResponsiveList>

    <FormDialog v-model="dialogVisible" :instance="editing" @saved="list.load" />
  </div>
</template>

<style scoped>
/* 筛选控件宽度：插槽内容由本页编译，作用域样式仍然生效。
   容器、分页与移动端适配由 ResponsiveList 负责。 */
.filter-item {
  width: 180px;
}

.card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.mw-name {
  font-size: 14px;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.mw-tags {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
  margin-top: 8px;
}

.mw-endpoint {
  margin: 8px 0 0;
  font-size: 12.5px;
  color: var(--c-text-2);
}

.mw-message {
  margin: 4px 0 0;
  font-size: 12px;
  overflow: hidden;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
}

.mw-actions {
  display: flex;
  gap: 8px;
  margin-top: 10px;
  flex-wrap: wrap;
}
</style>
