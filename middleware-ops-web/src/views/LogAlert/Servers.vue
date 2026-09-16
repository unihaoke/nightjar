<script setup lang="ts">
/** 服务器与代码仓库映射：日志采集对象 + 出网白名单开关（6.5）。 */
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { logAlertApi } from '@/api'
import { toastError } from '@/api/http'
import type { CodeRepo, ServerInstance } from '@/api/types'
import { envLabels, envTagType, formatTime } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const store = useUserStore()

const loading = ref(false)
const serverDialog = ref(false)
const repoDialog = ref(false)
const submitting = ref(false)
const servers = ref<ServerInstance[]>([])
const serverTotal = ref(0)
const repos = ref<CodeRepo[]>([])
const repoTotal = ref(0)
const editingServer = ref<ServerInstance | null>(null)
const editingRepo = ref<CodeRepo | null>(null)

const serverQuery = reactive({ keyword: '', environment: '', page: 1, page_size: 20 })
const repoQuery = reactive({ keyword: '', page: 1, page_size: 20 })

const serverForm = reactive({ name: '', ip: '', hostname: '', environment: 'dev', group_name: '', tags: [] as string[] })
const repoForm = reactive({
  service_name: '',
  repo_url: '',
  branch: 'main',
  local_path: '',
  language: '',
  allow_third_party: false,
})

const canManage = computed(() => store.can('server:manage'))

/** 加载服务器。 */
async function loadServers(): Promise<void> {
  try {
    const result = await logAlertApi.servers({ ...serverQuery })
    servers.value = result.list || []
    serverTotal.value = result.total || 0
  } catch (error) {
    toastError(error)
  }
}

/** 加载仓库映射。 */
async function loadRepos(): Promise<void> {
  try {
    const result = await logAlertApi.codeRepos({ ...repoQuery })
    repos.value = result.list || []
    repoTotal.value = result.total || 0
  } catch (error) {
    toastError(error)
  }
}

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
    await loadServers()
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
    await loadServers()
  } catch (error) {
    toastError(error)
  }
}

/** 保存仓库映射。 */
async function saveRepo(): Promise<void> {
  if (!repoForm.service_name.trim()) {
    ElMessage({ type: 'warning', message: '请填写服务名' })
    return
  }
  submitting.value = true
  try {
    await logAlertApi.saveCodeRepo(editingRepo.value?.id, { ...repoForm })
    ElMessage({ type: 'success', message: '已保存' })
    repoDialog.value = false
    await loadRepos()
  } catch (error) {
    toastError(error)
  } finally {
    submitting.value = false
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

/** 打开仓库表单。 */
function openRepo(item?: CodeRepo): void {
  editingRepo.value = item || null
  Object.assign(repoForm, {
    service_name: item?.service_name || '',
    repo_url: item?.repo_url || '',
    branch: item?.branch || 'main',
    local_path: item?.local_path || '',
    language: item?.language || '',
    allow_third_party: item?.allow_third_party || false,
  })
  repoDialog.value = true
}

onMounted(async () => {
  loading.value = true
  await Promise.all([loadServers(), loadRepos()])
  loading.value = false
})
</script>

<template>
  <div class="page" v-loading="loading">
    <div class="page-header">
      <div>
        <h2 class="page-title">服务器与代码仓库</h2>
        <p class="page-subtitle">出网白名单默认关闭：仅显式开启的服务允许将堆栈/代码片段发送至第三方 AI（并强制脱敏）</p>
      </div>
    </div>

    <div class="card">
      <h3 class="card-title">
        服务器实例
        <div class="row">
          <el-input v-model="serverQuery.keyword" size="small" placeholder="搜索名称或 IP" clearable class="search" @keyup.enter="loadServers" />
          <el-button v-if="canManage" size="small" type="primary" :icon="'Plus'" @click="openServer()">新增</el-button>
        </div>
      </h3>
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
      <el-pagination
        v-model:current-page="serverQuery.page"
        :page-size="serverQuery.page_size"
        :total="serverTotal"
        layout="total, prev, pager, next"
        class="pager"
        @current-change="loadServers"
      />
    </div>

    <div class="card">
      <h3 class="card-title">
        服务 → 代码仓库映射
        <div class="row">
          <el-input v-model="repoQuery.keyword" size="small" placeholder="搜索服务或仓库" clearable class="search" @keyup.enter="loadRepos" />
          <el-button v-if="canManage" size="small" type="primary" :icon="'Plus'" @click="openRepo()">新增</el-button>
        </div>
      </h3>
      <div class="table-scroll">
        <el-table :data="repos" size="small">
          <el-table-column prop="service_name" label="服务名" min-width="140" show-overflow-tooltip />
          <el-table-column prop="repo_url" label="仓库地址" min-width="220" show-overflow-tooltip />
          <el-table-column prop="branch" label="分支" width="90" />
          <el-table-column prop="local_path" label="本地路径" min-width="200" show-overflow-tooltip />
          <el-table-column prop="language" label="语言" width="90" />
          <el-table-column label="出网白名单" width="110">
            <template #default="{ row }">
              <el-tag size="small" :type="row.allow_third_party ? 'warning' : 'info'" effect="light">
                {{ row.allow_third_party ? '已开启' : '未开启' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column v-if="canManage" label="操作" width="90" fixed="right">
            <template #default="{ row }">
              <el-button text size="small" @click="openRepo(row)">编辑</el-button>
            </template>
          </el-table-column>
        </el-table>
      </div>
      <el-pagination
        v-model:current-page="repoQuery.page"
        :page-size="repoQuery.page_size"
        :total="repoTotal"
        layout="total, prev, pager, next"
        class="pager"
        @current-change="loadRepos"
      />
    </div>

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

    <el-dialog v-model="repoDialog" :title="editingRepo ? '编辑仓库映射' : '新增仓库映射'" width="560px">
      <el-form label-position="top">
        <el-row :gutter="12">
          <el-col :xs="24" :sm="12">
            <el-form-item label="服务名">
              <el-input v-model="repoForm.service_name" placeholder="与日志上报的 service 一致" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="分支">
              <el-input v-model="repoForm.branch" />
            </el-form-item>
          </el-col>
          <el-col :span="24">
            <el-form-item label="仓库地址">
              <el-input v-model="repoForm.repo_url" placeholder="git@gitlab.internal:group/repo.git" />
            </el-form-item>
          </el-col>
          <el-col :span="24">
            <el-form-item label="本地代码路径（用于简单检索定位）">
              <el-input v-model="repoForm.local_path" placeholder="/data/repos/order-service" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="语言">
              <el-input v-model="repoForm.language" placeholder="java / go / python" />
            </el-form-item>
          </el-col>
          <el-col :span="24">
            <el-form-item label="出网白名单">
              <el-switch v-model="repoForm.allow_third_party" />
              <span class="muted hint">开启后允许将该服务的堆栈与代码片段（脱敏、≤200 行/文件）发送至第三方 AI</span>
            </el-form-item>
          </el-col>
        </el-row>
      </el-form>
      <template #footer>
        <el-button @click="repoDialog = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="saveRepo">保存</el-button>
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

.pager {
  margin-top: 12px;
  justify-content: flex-end;
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
