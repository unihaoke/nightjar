<script setup lang="ts">
/** 用户与角色管理（6.1 RBAC + 数据权限）。 */
import { computed, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { userApi } from '@/api'
import { toastError } from '@/api/http'
import type { Role, User } from '@/api/types'
import { useListPage } from '@/composables/useListPage'
import ResponsiveList from '@/components/ResponsiveList.vue'
import { formatTime } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const store = useUserStore()

const submitting = ref(false)
const dialogVisible = ref(false)
const roleDialogVisible = ref(false)
const editing = ref<User | null>(null)
const editingRole = ref<Role | null>(null)
const roles = ref<Role[]>([])
const permissionGroups = ref<{ group: string; items: { code: string; name: string }[] }[]>([])

// 用户列表与角色列表一并取：改完角色后需要立刻看到最新权限点。
const list = useListPage<User>({
  fetch: async (params, signal) => {
    const [userList, roleList] = await Promise.all([userApi.list(params, signal), userApi.roles()])
    roles.value = roleList.list || []
    permissionGroups.value = roleList.permissions || []
    return userList
  },
  defaults: { keyword: '', page: 1, page_size: 20 },
})
const { query, items: users, total, loading, error } = list

const form = reactive({
  username: '',
  password: '',
  nickname: '',
  email: '',
  role_code: 'dev',
  env_scope: [] as string[],
  group_scope: [] as string[],
  status: 1,
})

const roleForm = reactive({ name: '', description: '', permissions: [] as string[], levels: [] as string[] })

const canManage = computed(() => store.can('user:manage'))

/** 打开用户表单。 */
function openForm(user?: User): void {
  editing.value = user || null
  Object.assign(form, {
    username: user?.username || '',
    password: '',
    nickname: user?.nickname || '',
    email: user?.email || '',
    role_code: user?.role_code || 'dev',
    env_scope: user?.env_scope || [],
    group_scope: user?.group_scope || [],
    status: user?.status ?? 1,
  })
  dialogVisible.value = true
}

/** 保存用户。 */
async function saveUser(): Promise<void> {
  if (!form.username.trim()) {
    ElMessage({ type: 'warning', message: '请填写用户名' })
    return
  }
  if (!editing.value && !form.password) {
    ElMessage({ type: 'warning', message: '请设置初始密码' })
    return
  }
  submitting.value = true
  try {
    if (editing.value) {
      await userApi.update(editing.value.id, { ...form })
    } else {
      await userApi.create({ ...form })
    }
    ElMessage({ type: 'success', message: '已保存' })
    dialogVisible.value = false
    await list.load()
  } catch (error) {
    toastError(error)
  } finally {
    submitting.value = false
  }
}

/** 删除用户。 */
async function removeUser(user: User): Promise<void> {
  try {
    await ElMessageBox.confirm(`确认删除用户「${user.username}」？`, '删除确认', {
      confirmButtonText: '删除',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await userApi.remove(user.id)
    ElMessage({ type: 'success', message: '已删除' })
    await list.load()
  } catch (error) {
    toastError(error)
  }
}

/** 打开角色编辑。 */
function openRole(role: Role): void {
  editingRole.value = role
  Object.assign(roleForm, {
    name: role.name,
    description: role.description,
    permissions: role.permissions || [],
    levels: role.levels || [],
  })
  roleDialogVisible.value = true
}

/** 保存角色。 */
async function saveRole(): Promise<void> {
  if (!editingRole.value) {
    return
  }
  submitting.value = true
  try {
    await userApi.updateRole(editingRole.value.id, { ...roleForm })
    ElMessage({ type: 'success', message: '角色已更新' })
    roleDialogVisible.value = false
    await list.load()
  } catch (error) {
    toastError(error)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2 class="page-title">用户与角色</h2>
        <p class="page-subtitle">
          数据权限按环境（dev/staging/prod）与分组隔离，权限校验在服务端工具层强制执行
        </p>
      </div>
      <el-button v-if="canManage" type="primary" :icon="'Plus'" @click="openForm()">新增用户</el-button>
    </div>

    <ResponsiveList
      :items="users"
      :loading="loading"
      :error="error"
      :total="total"
      :page="query.page"
      :page-size="query.page_size"
      title="用户列表"
      empty-text="没有匹配的用户"
      @update:page="list.setPage"
      @update:page-size="list.setPageSize"
      @retry="list.load"
    >
      <template #filters>
        <el-input v-model="query.keyword" placeholder="搜索用户名/昵称/邮箱" clearable class="filter-item" @keyup.enter="list.search" />
        <el-button type="primary" :icon="'Search'" @click="list.search">查询</el-button>
        <el-button :icon="'RefreshLeft'" @click="list.reset">重置</el-button>
      </template>

      <!-- 移动端：卡片 -->
      <template #card="{ row }">
        <div class="user-head">
          <span class="user-name">{{ row.nickname || row.username }}</span>
          <el-tag size="small" effect="plain">{{ row.role_code }}</el-tag>
          <el-tag size="small" :type="row.status === 1 ? 'success' : 'danger'" effect="light">
            {{ row.status === 1 ? '启用' : '禁用' }}
          </el-tag>
        </div>
        <p class="user-meta muted">{{ row.username }}<span v-if="row.email"> · {{ row.email }}</span></p>
        <p class="user-meta muted">最近登录 {{ formatTime(row.last_login) }}</p>
        <div v-if="canManage" class="user-actions">
          <el-button size="small" @click="openForm(row)">编辑</el-button>
          <el-button size="small" type="danger" @click="removeUser(row)">删除</el-button>
        </div>
      </template>

      <!-- 桌面端：表格 -->
      <template #table>
        <div class="table-scroll">
        <el-table :data="users" size="default">
          <el-table-column prop="username" label="用户名" min-width="130" />
          <el-table-column prop="nickname" label="昵称" min-width="120" />
          <el-table-column prop="email" label="邮箱" min-width="180" show-overflow-tooltip />
          <el-table-column label="角色" width="110">
            <template #default="{ row }">
              <el-tag size="small" effect="plain">{{ row.role_code }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="数据权限" min-width="200">
            <template #default="{ row }">
              <span v-if="!(row.env_scope || []).length && !(row.group_scope || []).length" class="muted">全部环境 / 全部分组</span>
              <template v-else>
                <el-tag v-for="env in row.env_scope || []" :key="env" size="small" effect="plain" class="mini-tag">{{ env }}</el-tag>
                <el-tag v-for="group in row.group_scope || []" :key="group" size="small" type="info" effect="plain" class="mini-tag">
                  {{ group }}
                </el-tag>
              </template>
            </template>
          </el-table-column>
          <el-table-column label="状态" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.status === 1 ? 'success' : 'danger'" effect="light">
                {{ row.status === 1 ? '启用' : '禁用' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="最近登录" width="170">
            <template #default="{ row }">{{ formatTime(row.last_login) }}</template>
          </el-table-column>
          <el-table-column v-if="canManage" label="操作" width="130" fixed="right">
            <template #default="{ row }">
              <el-button text size="small" @click="openForm(row)">编辑</el-button>
              <el-button text size="small" type="danger" @click="removeUser(row)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
        </div>
      </template>
    </ResponsiveList>

    <div class="card">
      <h3 class="card-title">角色与权限点</h3>
      <div class="table-scroll">
        <el-table :data="roles" size="small">
          <el-table-column prop="code" label="角色码" width="110" />
          <el-table-column prop="name" label="名称" width="100" />
          <el-table-column prop="description" label="职责范围" min-width="240" show-overflow-tooltip />
          <el-table-column label="可直接执行的级别" width="180">
            <template #default="{ row }">
              <el-tag v-for="level in row.levels || []" :key="level" size="small" effect="plain" class="mini-tag">{{ level }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="权限点数量" width="110">
            <template #default="{ row }">{{ (row.permissions || []).length }}</template>
          </el-table-column>
          <el-table-column label="用户数" width="90">
            <template #default="{ row }">{{ row.user_count ?? 0 }}</template>
          </el-table-column>
          <el-table-column label="类型" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.builtin ? 'info' : 'primary'" effect="plain">
                {{ row.builtin ? '内置' : '自定义' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column v-if="canManage" label="操作" width="90" fixed="right">
            <template #default="{ row }">
              <el-button text size="small" :disabled="row.builtin" @click="openRole(row)">编辑</el-button>
            </template>
          </el-table-column>
        </el-table>
      </div>
      <p class="muted note">内置角色权限点随版本升级自动同步，不允许手动修改；需要差异化授权时请创建自定义角色。</p>
    </div>

    <!-- 用户表单 -->
    <el-dialog v-model="dialogVisible" :title="editing ? '编辑用户' : '新增用户'" width="560px">
      <el-form label-position="top">
        <el-row :gutter="12">
          <el-col :xs="24" :sm="12">
            <el-form-item label="用户名">
              <el-input v-model="form.username" :disabled="Boolean(editing)" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item :label="editing ? '重置密码（留空不修改）' : '初始密码'">
              <el-input v-model="form.password" type="password" show-password />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="昵称">
              <el-input v-model="form.nickname" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="邮箱">
              <el-input v-model="form.email" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="角色">
              <el-select v-model="form.role_code" class="mobile-block">
                <el-option v-for="item in roles" :key="item.code" :label="`${item.name}（${item.code}）`" :value="item.code" />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="状态">
              <el-switch v-model="form.status" :active-value="1" :inactive-value="0" active-text="启用" inactive-text="禁用" />
            </el-form-item>
          </el-col>
          <el-col :span="24">
            <el-form-item label="数据权限：环境范围（留空表示全部）">
              <el-select v-model="form.env_scope" multiple clearable class="mobile-block">
                <el-option label="开发" value="dev" />
                <el-option label="预发" value="staging" />
                <el-option label="生产" value="prod" />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :span="24">
            <el-form-item label="数据权限：分组范围（留空表示全部分组）">
              <el-select v-model="form.group_scope" multiple filterable allow-create default-first-option class="mobile-block" />
            </el-form-item>
          </el-col>
        </el-row>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="saveUser">保存</el-button>
      </template>
    </el-dialog>

    <!-- 角色表单 -->
    <el-dialog v-model="roleDialogVisible" title="编辑角色权限" width="640px">
      <el-form label-position="top">
        <el-form-item label="角色名称">
          <el-input v-model="roleForm.name" />
        </el-form-item>
        <el-form-item label="职责说明">
          <el-input v-model="roleForm.description" type="textarea" :rows="2" />
        </el-form-item>
        <el-form-item label="可直接执行的级别（L2 一律走审批，不可授予）">
          <el-checkbox-group v-model="roleForm.levels">
            <el-checkbox value="L0">L0 只读</el-checkbox>
            <el-checkbox value="L1">L1 低危</el-checkbox>
          </el-checkbox-group>
        </el-form-item>
        <el-form-item label="权限点">
          <div class="permission-groups">
            <div v-for="group in permissionGroups" :key="group.group" class="permission-group">
              <p class="permission-title">{{ group.group }}</p>
              <el-checkbox-group v-model="roleForm.permissions">
                <el-checkbox v-for="item in group.items" :key="item.code" :value="item.code">{{ item.name }}</el-checkbox>
              </el-checkbox-group>
            </div>
          </div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="roleDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="saveRole">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.filter-item {
  width: 260px;
}

.mini-tag {
  margin-right: 4px;
}

/* 移动端卡片：用户条目 */
.user-head {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.user-name {
  font-size: 13.5px;
  font-weight: 600;
}

.user-meta {
  margin: 6px 0 0;
  font-size: 12px;
  word-break: break-all;
}

.user-actions {
  display: flex;
  gap: 8px;
  margin-top: 8px;
}

.note {
  margin: 10px 0 0;
  font-size: 12px;
}

.permission-groups {
  display: flex;
  flex-direction: column;
  gap: 10px;
  max-height: 320px;
  overflow: auto;
}

.permission-title {
  margin: 0 0 4px;
  font-size: 12px;
  font-weight: 600;
  color: var(--c-text-3);
}

</style>
