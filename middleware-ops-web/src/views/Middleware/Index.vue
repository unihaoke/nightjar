<script setup lang="ts">
/**
 * 中间件纳管列表（4.1）。
 *
 * 移动端适配：
 *   - <768px 时把表格切换为卡片列表，避免横向滚动找不到操作入口；
 *   - 表格模式使用横向滚动容器承载较多列。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { middlewareApi } from '@/api'
import { toastError } from '@/api/http'
import type { MiddlewareInstance } from '@/api/types'
import { useAppStore } from '@/stores/app'
import { useUserStore } from '@/stores/user'
import { envLabels, envTagType, formatTime, mwTypeLabels, mwTypeTagType } from '@/utils/format'
import FormDialog from './FormDialog.vue'

const router = useRouter()
const app = useAppStore()
const store = useUserStore()

const loading = ref(false)
const items = ref<MiddlewareInstance[]>([])
const total = ref(0)
const dialogVisible = ref(false)
const editing = ref<MiddlewareInstance | null>(null)

const query = reactive({
  keyword: '',
  mw_type: '',
  environment: '',
  group: '',
  page: 1,
  page_size: 20,
})

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

/** 加载列表。 */
async function load(): Promise<void> {
  loading.value = true
  try {
    const result = await middlewareApi.list({ ...query })
    items.value = result.list || []
    total.value = result.total || 0
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

/** 重置筛选。 */
function resetQuery(): void {
  query.keyword = ''
  query.mw_type = ''
  query.environment = ''
  query.group = ''
  query.page = 1
  void load()
}

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
    await load()
  } catch (error) {
    if (error !== 'cancel') {
      toastError(error)
    }
  }
}

/** 详细页跳转。 */
function openDetail(item: MiddlewareInstance): void {
  void router.push({ name: 'middleware-detail', params: { id: item.id } })
}

onMounted(load)
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

    <div class="card filters">
      <el-input v-model="query.keyword" placeholder="按名称或地址搜索" clearable class="filter-item" @keyup.enter="load">
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
      <el-input v-model="query.group" placeholder="分组" clearable class="filter-item" @keyup.enter="load" />
      <el-button type="primary" :icon="'Search'" @click="load">查询</el-button>
      <el-button :icon="'RefreshLeft'" @click="resetQuery">重置</el-button>
    </div>

    <!-- 移动端：卡片列表 -->
    <div v-if="app.isMobile" class="mobile-list" v-loading="loading">
      <el-empty v-if="items.length === 0 && !loading" description="没有匹配的实例" />
      <div v-for="item in items" :key="item.id" class="mw-card" @click="openDetail(item)">
        <div class="mw-card-head">
          <span class="mw-name">{{ item.name }}</span>
          <el-tag size="small" :type="item.status === 1 ? 'success' : 'danger'" effect="light">
            {{ item.status === 1 ? '在线' : '离线' }}
          </el-tag>
        </div>
        <div class="mw-tags">
          <el-tag size="small" effect="plain" :type="mwTypeTagType[item.mw_type] || 'info'">
            {{ mwTypeLabels[item.mw_type] || item.mw_type }}
          </el-tag>
          <el-tag size="small" effect="plain" :type="envTagType[item.environment]">
            {{ envLabels[item.environment] }}
          </el-tag>
          <el-tag v-if="item.group_name" size="small" effect="plain">{{ item.group_name }}</el-tag>
        </div>
        <p class="mw-endpoint mono">{{ item.host }}:{{ item.port }}</p>
        <p class="mw-message muted">{{ item.last_message || '尚未探测' }}</p>
        <div class="mw-actions" @click.stop>
          <el-button size="small" @click="openDetail(item)">详情</el-button>
          <el-button v-if="canWrite" size="small" @click="openForm(item)">编辑</el-button>
          <el-button v-if="canWrite" size="small" type="danger" plain @click="handleDelete(item)">
            {{ item.environment === 'prod' ? '审批删除' : '删除' }}
          </el-button>
        </div>
      </div>
      <el-pagination
        v-if="total > query.page_size"
        v-model:current-page="query.page"
        :page-size="query.page_size"
        :total="total"
        layout="prev, pager, next"
        small
        class="pager"
        @current-change="load"
      />
    </div>

    <!-- 桌面端：表格 -->
    <div v-else class="card">
      <div class="table-scroll">
        <el-table :data="items" v-loading="loading" size="default" row-key="id" @row-click="openDetail">
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
      <el-pagination
        v-model:current-page="query.page"
        v-model:page-size="query.page_size"
        :total="total"
        :page-sizes="[20, 50, 100]"
        layout="total, sizes, prev, pager, next"
        class="pager"
        @current-change="load"
        @size-change="load"
      />
    </div>

    <FormDialog v-model="dialogVisible" :instance="editing" @saved="load" />
  </div>
</template>

<style scoped>
.filters {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  align-items: center;
  margin-bottom: 12px;
}

.filter-item {
  width: 180px;
}

.pager {
  margin-top: 12px;
  justify-content: flex-end;
}

.mobile-list {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.mw-card {
  background: var(--c-surface);
  border: 1px solid var(--c-border);
  border-radius: var(--r-lg);
  padding: 12px 14px;
}

.mw-card-head {
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

@media (max-width: 767px) {
  .filter-item {
    width: 100%;
  }
}
</style>
