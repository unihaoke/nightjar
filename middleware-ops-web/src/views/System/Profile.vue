<script setup lang="ts">
/** 个人设置：账号信息、数据权限、修改密码。 */
import { computed, reactive, ref } from 'vue'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { authApi } from '@/api'
import { toastError } from '@/api/http'
import { envLabels } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const store = useUserStore()

const formRef = ref<FormInstance>()
const submitting = ref(false)
const form = reactive({ oldPassword: '', newPassword: '', confirmPassword: '' })

const rules: FormRules = {
  oldPassword: [{ required: true, message: '请输入原密码', trigger: 'blur' }],
  newPassword: [
    { required: true, message: '请输入新密码', trigger: 'blur' },
    { min: 8, message: '新密码至少 8 位', trigger: 'blur' },
  ],
  confirmPassword: [
    { required: true, message: '请再次输入新密码', trigger: 'blur' },
    {
      validator: (_rule, value, callback) => {
        if (value !== form.newPassword) {
          callback(new Error('两次输入的密码不一致'))
          return
        }
        callback()
      },
      trigger: 'blur',
    },
  ],
}

/** 数据权限描述。 */
const scopeText = computed(() => {
  const scope = store.dataScope
  if (scope.allow_all) {
    return '全部环境 / 全部分组'
  }
  const envs = (scope.environments || []).map((item) => envLabels[item] || item)
  const groups = scope.groups || []
  return `${envs.length ? envs.join('、') : '全部环境'} / ${groups.length ? groups.join('、') : '全部分组'}`
})

/** 提交修改密码。 */
async function submit(): Promise<void> {
  if (!formRef.value) {
    return
  }
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) {
    return
  }
  submitting.value = true
  try {
    const result = await authApi.changePassword(form.oldPassword, form.newPassword)
    ElMessage({ type: 'success', message: result.message })
    form.oldPassword = ''
    form.newPassword = ''
    form.confirmPassword = ''
    formRef.value.clearValidate()
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
        <h2 class="page-title">个人设置</h2>
        <p class="page-subtitle">账号信息、数据权限范围与密码管理</p>
      </div>
    </div>

    <el-row :gutter="12">
      <el-col :xs="24" :md="12">
        <div class="card">
          <h3 class="card-title">账号信息</h3>
          <div class="kv-list">
            <div class="kv"><span>用户名</span><span class="mono">{{ store.user?.username }}</span></div>
            <div class="kv"><span>昵称</span><span>{{ store.user?.nickname || '-' }}</span></div>
            <div class="kv"><span>邮箱</span><span>{{ store.user?.email || '-' }}</span></div>
            <div class="kv"><span>角色</span><el-tag size="small" effect="plain">{{ store.user?.role_code }}</el-tag></div>
            <div class="kv"><span>数据权限</span><span>{{ scopeText }}</span></div>
            <div class="kv">
              <span>可直接执行级别</span>
              <span class="row">
                <el-tag v-for="level in store.levels" :key="level" size="small" effect="plain">{{ level }}</el-tag>
                <span class="muted">（L2 一律走审批）</span>
              </span>
            </div>
          </div>
          <el-divider />
          <h3 class="card-title">已授予权限点（{{ store.permissions.length }} 项）</h3>
          <div class="chips">
            <el-tag v-for="perm in store.permissions" :key="perm" size="small" effect="plain">{{ perm }}</el-tag>
            <el-empty v-if="store.permissions.length === 0" description="无权限点" :image-size="60" />
          </div>
        </div>
      </el-col>

      <el-col :xs="24" :md="12">
        <div class="card">
          <h3 class="card-title">修改密码</h3>
          <el-form ref="formRef" :model="form" :rules="rules" label-position="top">
            <el-form-item label="原密码" prop="oldPassword">
              <el-input v-model="form.oldPassword" type="password" show-password autocomplete="current-password" />
            </el-form-item>
            <el-form-item label="新密码（至少 8 位）" prop="newPassword">
              <el-input v-model="form.newPassword" type="password" show-password autocomplete="new-password" />
            </el-form-item>
            <el-form-item label="确认新密码" prop="confirmPassword">
              <el-input v-model="form.confirmPassword" type="password" show-password autocomplete="new-password" />
            </el-form-item>
            <el-button type="primary" :loading="submitting" @click="submit">更新密码</el-button>
          </el-form>
          <el-alert
            type="info"
            :closable="false"
            show-icon
            title="密码使用 bcrypt 加盐哈希存储"
            description="修改后请使用新密码重新登录；平台不会保存明文密码。"
            class="mt"
          />
        </div>
      </el-col>
    </el-row>
  </div>
</template>

<style scoped>
.kv-list {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.kv {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  font-size: 12.5px;
  color: var(--c-text-2);
}

.chips {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}

.mt {
  margin-top: 12px;
}
</style>
