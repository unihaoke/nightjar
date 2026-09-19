<script setup lang="ts">
/** 服务器：日志采集对象与纳管目标（代码仓库映射已移除，AI 分析改由外部 AI 服务完成）。 */
import { computed, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { logAlertApi } from '@/api'
import { toastError } from '@/api/http'
import type { ServerInstance } from '@/api/types'
import { useListPage } from '@/composables/useListPage'
import ResponsiveList from '@/components/ResponsiveList.vue'
import { envLabels, envTagType, formatTime } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const store = useUserStore()

const serverDialog = ref(false)
const submitting = ref(false)
const editingServer = ref<ServerInstance | null>(null)

const serverList = useListPage<ServerInstance>({
  fetch: (params, signal) => logAlertApi.servers(params, signal),
  defaults: { keyword: '', environment: '', page: 1, page_size: 20 },
})
const { query: serverQuery, items: servers, total: serverTotal, loading: serverLoading, error: serverError } = serverList

const serverForm = reactive({ name: '', ip: '', hostname: '', environment: 'dev', group_name: '', tags: [] as string[] })

const canManage = computed(() => store.can('server:manage'))

/** 保存服务器。 */
async function saveServer(): Promise<void> {
  if (!serverForm.name.trim()) {
    ElMessage({ type: 'warning', message: '请填写服务器名称' })
    return
  }
  submitting.value = true
  try {
    if (editingServer.value) {
      await logAlertApi.updateServer(editingServer.value.id, { ...serverForm })
    } else {
      await logAlertApi.createServer({ ...serverForm })
    }
    ElMessage({ type: 'success', message: '已保存' })
    serverDialog.value = false
    await serverList.load()
  } catch (error) {
    toastError(error)
  } finally {
    submitting.value = false
  }
}

/** 删除服务器。 */
async function removeServer(item: ServerInstance): Promise<void> {
  try {
    await ElMessageBox.confirm(`确认删除服务器「${item.name}」？`, '删除确认', {
      confirmButtonText: '删除',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await logAlertApi.removeServer(item.id)
    ElMessage({ type: 'success', message: '已删除' })
    await serverList.load()
  } catch (error) {
    toastError(error)
  }
}

/** 打开服务器表单。 */
function openServer(item?: ServerInstance): void {
  editingServer.value = item || null
  Object.assign(serverForm, {
    name: item?.name || '',
    ip: item?.ip || '',
    hostname: item?.hostname || '',
    environment: item?.environment || 'dev',
    group_name: item?.group_name || '',
    tags: item?.tags || [],
  })
  serverDialog.value = true
}
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2 class="page-title">服务器</h2>
        <p class="page-subtitle">日志采集对象与纳管目标；AI 分析由外部 AI 服务完成，出网白名单仍按服务控制（默认关闭）</p>
      </div>
    </div>

    <ResponsiveList
      :items="servers"
      :loading="serverLoading"
      :error="serverError"
      :total="serverTotal"
      :page="serverQuery.page"
      :page-size="serverQuery.page_size"
      title="服务器实例"
      empty-text="尚未登记服务器"
      @update:page="serverList.setPage"
      @update:page-size="serverList.setPageSize"
      @retry="serverList.load"
    >
      <template #toolbar>
        <el-input v-model="serverQuery.keyword" size="small" placeholder="搜索名称或 IP" clearable class="search" @keyup.enter="serverList.search" />
        <el-button v-if="canManage" size="small" type="primary" :icon="'Plus'" @click="openServer()">新增</el-button>
      </template>

      <template #card="{ row }">
        <div class="entity-head">
          <span class="entity-name">{{ row.name }}</span>
          <el-tag size="small" effect="plain" :type="envTagType[row.environment]">
            {{ envLabels[row.environment] || row.environment }}
          </el-tag>
          <el-tag size="small" :type="row.status === 1 ? 'success' : 'info'" effect="light">
            {{ row.status === 1 ? '在线' : '离线' }}
          </el-tag>
        </div>
        <p class="entity-meta mono">{{ row.ip }}<span v-if="row.hostname"> · {{ row.hostname }}</span></p>
        <p class="entity-meta muted">
          <span v-if="row.group_name">分组 {{ row.group_name }} · </span>心跳 {{ formatTime(row.last_seen_at) }}
        </p>
        <div v-if="canManage" class="entity-actions">
          <el-button size="small" @click="openServer(row)">编辑</el-button>
          <el-button size="small" type="danger" @click="removeServer(row)">删除</el-button>
        </div>
      </template>

      <template #table>
        <div class="table-scroll">
        <el-table :data="servers" size="small">
          <el-table-column prop="name" label="名称" min-width="140" show-overflow-tooltip />
          <el-table-column prop="ip" label="IP" width="140" />
          <el-table-column prop="hostname" label="主机名" min-width="140" show-overflow-tooltip />
          <el-table-column label="环境" width="90">
            <template #default="{ row }">
              <el-tag size="small" effect="plain" :type="envTagType[row.environment]">{{ envLabels[row.environment] || row.environment }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="group_name" label="分组" width="110" />
          <el-table-column label="状态" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.status === 1 ? 'success' : 'info'" effect="light">
                {{ row.status === 1 ? '在线' : '离线' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="最近心跳" width="170">
            <template #default="{ row }">{{ formatTime(row.last_seen_at) }}</template>
          </el-table-column>
          <el-table-column v-if="canManage" label="操作" width="130" fixed="right">
            <template #default="{ row }">
              <el-button text size="small" @click="openServer(row)">编辑</el-button>
              <el-button text size="small" type="danger" @click="removeServer(row)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
        </div>
      </template>
    </ResponsiveList>

    <el-dialog v-model="serverDialog" :title="editingServer ? '编辑服务器' : '新增服务器'" width="520px">
      <el-form label-position="top">
        <el-row :gutter="12">
          <el-col :xs="24" :sm="12">
            <el-form-item label="名称">
              <el-input v-model="serverForm.name" placeholder="如 order-app-01" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="IP">
              <el-input v-model="serverForm.ip" placeholder="10.0.0.10" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="主机名">
              <el-input v-model="serverForm.hostname" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="环境">
              <el-select v-model="serverForm.environment" class="mobile-block">
                <el-option label="开发" value="dev" />
                <el-option label="预发" value="staging" />
                <el-option label="生产" value="prod" />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="分组">
              <el-input v-model="serverForm.group_name" />
            </el-form-item>
          </el-col>
          <el-col :span="24">
            <el-form-item label="标签">
              <el-select v-model="serverForm.tags" multiple filterable allow-create default-first-option class="mobile-block" />
            </el-form-item>
          </el-col>
        </el-row>
      </el-form>
      <template #footer>
        <el-button @click="serverDialog = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="saveServer">保存</el-button>
      </template>
    </el-dialog>

  </div>
</template>

<style scoped>
.card + .card {
  margin-top: 12px;
}

.search {
  width: 200px;
}

/* 移动端卡片：服务器 / 仓库条目 */
.entity-head {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.entity-name {
  font-size: 13.5px;
  font-weight: 600;
}

.entity-meta {
  margin: 6px 0 0;
  font-size: 12px;
  word-break: break-all;
}

.entity-actions {
  display: flex;
  gap: 8px;
  margin-top: 8px;
}

.hint {
  margin-left: 8px;
  font-size: 11.5px;
}

@media (max-width: 767px) {
  .search {
    width: 100%;
  }
}
</style>
