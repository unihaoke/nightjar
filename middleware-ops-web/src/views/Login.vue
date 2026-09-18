<script setup lang="ts">
/** 登录页：极简表单，移动端单列自适应。 */
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { useUserStore } from '@/stores/user'
import { toastError } from '@/api/http'
import { systemApi } from '@/api'
import type { SystemInfo } from '@/api/types'

const store = useUserStore()
const router = useRouter()
const route = useRoute()

const formRef = ref<FormInstance>()
const loading = ref(false)
const systemInfo = ref<SystemInfo | null>(null)
const form = reactive({ username: '', password: '' })

const rules: FormRules = {
  username: [{ required: true, message: '请输入用户名', trigger: 'blur' }],
  password: [{ required: true, message: '请输入密码', trigger: 'blur' }],
}

/** 引擎与基础设施概览（未登录也可看：/api/system/info 需登录，失败时静默降级）。 */
const infraHint = computed(() => {
  if (!systemInfo.value) {
    return ''
  }
  const { infrastructure, ai_engine } = systemInfo.value
  const source = infrastructure.prometheus.source === 'prometheus' ? 'Prometheus' : '无数据源'
  return `AI 引擎：${ai_engine.strategy} · 指标来源：${source} · 缓存：${infrastructure.redis.cache_kind}`
})

async function handleSubmit(): Promise<void> {
  if (!formRef.value) {
    return
  }
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) {
    return
  }
  loading.value = true
  try {
    await store.login(form.username, form.password)
    ElMessage({ type: 'success', message: '登录成功' })
    const redirect = (route.query.redirect as string) || '/dashboard'
    await router.replace(redirect)
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  // 登录页尝试读取系统信息以展示部署形态；无权限或未登录时忽略错误。
  try {
    systemInfo.value = await systemApi.info()
  } catch {
    systemInfo.value = null
  }
})
</script>

<template>
  <div class="login-page">
    <div class="login-panel">
      <div class="login-brand">
        <div class="brand-mark" aria-hidden="true">MW</div>
        <div>
          <h1 class="brand-title">中间件智能问题解决平台</h1>
          <p class="brand-sub">统一纳管 · 监控告警 · AI 诊断 · 分级执行</p>
        </div>
      </div>

      <el-form ref="formRef" :model="form" :rules="rules" label-position="top" size="large" @submit.prevent="handleSubmit">
        <el-form-item label="用户名" prop="username">
          <el-input v-model="form.username" placeholder="请输入用户名" autocomplete="username" :prefix-icon="'User'" />
        </el-form-item>
        <el-form-item label="密码" prop="password">
          <el-input
            v-model="form.password"
            type="password"
            placeholder="请输入密码"
            autocomplete="current-password"
            show-password
            :prefix-icon="'Lock'"
            @keyup.enter="handleSubmit"
          />
        </el-form-item>
        <el-button type="primary" class="login-btn" :loading="loading" @click="handleSubmit">登录</el-button>
      </el-form>

      <p v-if="infraHint" class="login-hint">{{ infraHint }}</p>
      <p class="login-foot">首次部署请使用配置文件中的 bootstrap 管理员账号登录，并立即修改密码。</p>
    </div>
  </div>
</template>

<style scoped>
.login-page {
  min-height: 100dvh;
  display: grid;
  place-items: center;
  padding: var(--sp-4);
  background: var(--c-bg);
}

.login-panel {
  width: 100%;
  max-width: 400px;
  background: var(--c-surface);
  border: 1px solid var(--c-border);
  border-radius: var(--r-lg);
  box-shadow: var(--shadow-2);
  padding: var(--sp-5);
}

.login-brand {
  display: flex;
  gap: 12px;
  align-items: center;
  margin-bottom: var(--sp-5);
}

.brand-mark {
  width: 38px;
  height: 38px;
  flex: 0 0 38px;
  border-radius: var(--r-md);
  background: var(--c-accent);
  color: #fff;
  display: grid;
  place-items: center;
  font-weight: 700;
  font-size: 13px;
}

.brand-title {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
  letter-spacing: -0.01em;
}

.brand-sub {
  margin: 2px 0 0;
  font-size: 12px;
  color: var(--c-text-3);
}

.login-btn {
  width: 100%;
  margin-top: var(--sp-2);
}

.login-hint {
  margin: var(--sp-4) 0 0;
  font-size: 12px;
  color: var(--c-text-3);
  line-height: 1.6;
}

.login-foot {
  margin: var(--sp-2) 0 0;
  font-size: 11.5px;
  color: var(--c-text-3);
  line-height: 1.6;
  border-top: 1px solid var(--c-border);
  padding-top: var(--sp-3);
}
</style>
