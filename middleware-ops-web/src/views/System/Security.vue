<script setup lang="ts">
/**
 * 合规设置：出网白名单。
 *
 * 白名单决定「哪些服务的日志可以被送去外部 AI 分析」，默认空 = 全禁。
 * 它必须在界面上逐条维护：靠改 .env 再重启容器的话，"放行一个新服务"就变成一次部署动作，
 * 而放行/收回恰恰是会随着接入进度频繁变化的。
 */
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { settingApi } from '@/api'
import { toastError } from '@/api/http'
import type { SecuritySettingsView } from '@/api/types'
import { formatTime } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const store = useUserStore()
const canWrite = computed(() => store.can('system:config:write'))

const loading = ref(false)
const saving = ref(false)
const view = ref<SecuritySettingsView | null>(null)

/**
 * 白名单编辑中的值。
 *
 * 用 ref<string[]> 而不是 reactive 包对象：el-select 的 v-model 直接绑整个数组，
 * 整体替换即可触发重渲染（INC-020 的坑出在"按 key 取子对象再绑字段"的写法上）。
 */
const whitelist = ref<string[]>([])

/** 是否全部放行：`*` 是白名单里的通配项。 */
const allowAll = computed(() => whitelist.value.includes('*'))

/** 当前是否等于"全禁"（列表为空）。 */
const deniesAll = computed(() => whitelist.value.length === 0)

/** 服务名合法性：只允许字母数字、点、下划线、中划线与 `*`。 */
function isValidName(name: string): boolean {
  return name === '*' || /^[A-Za-z0-9._-]+$/.test(name)
}

async function load(): Promise<void> {
  loading.value = true
  try {
    const data = await settingApi.security()
    view.value = data
    whitelist.value = [...(data.outbound_whitelist || [])]
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

async function save(): Promise<void> {
  const invalid = whitelist.value.filter((item) => !isValidName(item.trim()))
  if (invalid.length > 0) {
    toastError(new Error(`服务名不合法：${invalid.join('、')}（只允许字母数字、点、下划线、中划线，或 *）`))
    return
  }
  saving.value = true
  try {
    view.value = await settingApi.saveSecurity({ outbound_whitelist: whitelist.value.map((i) => i.trim()) })
    whitelist.value = [...(view.value.outbound_whitelist || [])]
    ElMessage({ type: 'success', message: '合规设置已保存并即时生效' })
  } catch (error) {
    toastError(error)
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page" v-loading="loading">
    <div class="page-header">
      <div>
        <h2 class="page-title">合规设置</h2>
        <p class="page-subtitle">
          控制日志告警的错误信息能否送出平台做 AI 分析；白名单外的服务一律跳过分析并写明原因
        </p>
      </div>
      <div class="row">
        <el-button :icon="'Refresh'" size="small" @click="load">刷新</el-button>
      </div>
    </div>

    <div class="card">
      <h3 class="card-title">
        <span>出网白名单</span>
        <el-tag size="small" effect="plain" :type="view?.source === 'env' ? 'warning' : 'success'">
          {{ view?.source === 'env' ? '来源：环境变量（env）' : '来源：平台设置（platform）' }}
        </el-tag>
        <el-tag v-if="!canWrite" size="small" type="info" effect="plain">只读：缺少 system:config:write</el-tag>
      </h3>

      <el-alert
        v-if="view?.source === 'env'"
        class="mb-top"
        type="warning"
        :closable="false"
        show-icon
        title="尚未由平台托管"
      >
        <p class="field-hint">
          当前展示的是 .env / config.yaml 里的值（<span class="mono">MWOPS_SECURITY_OUTBOUND_WHITELIST</span>）。
          在下面保存一次即转为平台托管，此后以平台设置为准，环境变量不再参与。
        </p>
      </el-alert>

      <el-alert
        v-if="deniesAll"
        class="mb-top"
        type="info"
        :closable="false"
        show-icon
        title="当前禁止任何外部 AI 分析"
      >
        <p class="field-hint">
          白名单为空时，日志事件的 AI 分析会明确标为「未启用」并写明原因，不会假装排队。
        </p>
      </el-alert>

      <el-form label-position="top" :disabled="!canWrite">
        <el-form-item label="允许外发 AI 分析的服务">
          <el-select
            v-model="whitelist"
            multiple
            filterable
            allow-create
            default-first-option
            class="mobile-block"
            placeholder="输入服务名后回车，如 order-service"
          />
          <p class="field-hint">
            服务名必须与日志里的 <span class="mono">service</span> 字段完全一致（大小写不敏感），
            也就是 Filebeat 的 <span class="mono">fields.service</span>。
            填 <span class="mono">*</span> 表示全部放行——这会允许把任何服务的日志送去外部 AI，请谨慎。
          </p>
        </el-form-item>
      </el-form>

      <div v-if="canWrite" class="row actions">
        <el-button type="primary" :loading="saving" :icon="'Check'" @click="save">保存合规设置</el-button>
      </div>
      <p class="muted foot">
        保存后立即生效：白名单在每次合规判定时实时读取，不需要重启。
        <template v-if="allowAll">
          <br />
          <span class="text-danger">当前为全部放行（*），任何服务的日志都可能被送去外部 AI 分析。</span>
        </template>
        <template v-if="view?.updated_by">
          <br />
          最近由 {{ view.updated_by }}
          <template v-if="view.updated_at">于 {{ formatTime(view.updated_at) }}</template>
          更新
        </template>
      </p>
    </div>
  </div>
</template>
