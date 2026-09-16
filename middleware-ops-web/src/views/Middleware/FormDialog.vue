<script setup lang="ts">
/** 中间件实例表单对话框（新增/编辑/连接测试）。 */
import { computed, reactive, ref, watch } from 'vue'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { middlewareApi } from '@/api'
import { toastError } from '@/api/http'
import type { MiddlewareInput, MiddlewareInstance, MWType, TestResult } from '@/api/types'

const props = defineProps<{
  modelValue: boolean
  /** 编辑对象；为空表示新增。 */
  instance?: MiddlewareInstance | null
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: boolean): void
  (e: 'saved'): void
}>()

const visible = computed({
  get: () => props.modelValue,
  set: (value: boolean) => emit('update:modelValue', value),
})

const formRef = ref<FormInstance>()
const submitting = ref(false)
const testing = ref(false)
const testResult = ref<TestResult | null>(null)
const types = ref<{ value: string; label: string; port: number; phase: number }[]>([])
const groups = ref<string[]>([])
/** Prometheus 中实际存在的 job 名：直接给候选，避免把容器名当 job 名填。 */
const promJobs = ref<string[]>([])
const environments = ref<{ value: string; label: string }[]>([
  { value: 'dev', label: '开发' },
  { value: 'staging', label: '预发' },
  { value: 'prod', label: '生产' },
])

const isEdit = computed(() => Boolean(props.instance?.id))

const form = reactive<MiddlewareInput & { password: string }>({
  name: '',
  mw_type: 'redis',
  host: '',
  port: 6379,
  username: '',
  password: '',
  environment: 'dev',
  group_name: '',
  tags: [],
  prom_job: '',
  prom_instance: '',
})

const rules: FormRules = {
  name: [{ required: true, message: '请输入实例名称', trigger: 'blur' }],
  mw_type: [{ required: true, message: '请选择中间件类型', trigger: 'change' }],
  host: [{ required: true, message: '请输入连接地址', trigger: 'blur' }],
  port: [{ required: true, message: '请输入端口', trigger: 'blur' }],
}

/** 重置表单。 */
function reset(): void {
  const item = props.instance
  form.name = item?.name || ''
  form.mw_type = (item?.mw_type as MWType) || 'redis'
  form.host = item?.host || ''
  form.port = item?.port || 6379
  form.username = item?.username || ''
  form.password = ''
  form.environment = item?.environment || 'dev'
  form.group_name = item?.group_name || ''
  form.tags = item?.tags || []
  form.prom_job = item?.prom_job || ''
  form.prom_instance = item?.prom_instance || ''
  testResult.value = null
  formRef.value?.clearValidate()
}

/** 类型切换时带入默认端口。 */
function onTypeChange(value: string): void {
  const matched = types.value.find((item) => item.value === value)
  if (matched) {
    form.port = matched.port
  }
}

/** 连接测试（未保存的表单参数也可测试）。 */
async function handleTest(): Promise<void> {
  if (!form.host || !form.port) {
    ElMessage({ type: 'warning', message: '请先填写连接地址与端口' })
    return
  }
  testing.value = true
  try {
    testResult.value = await middlewareApi.testEndpoint({ ...form })
  } catch (error) {
    toastError(error)
  } finally {
    testing.value = false
  }
}

/** 已保存实例的即时健康探测。 */
async function handleHealth(): Promise<void> {
  if (!props.instance?.id) {
    return
  }
  testing.value = true
  try {
    testResult.value = await middlewareApi.health(props.instance.id)
    emit('saved')
  } catch (error) {
    toastError(error)
  } finally {
    testing.value = false
  }
}

/** 提交。 */
async function handleSubmit(): Promise<void> {
  if (!formRef.value) {
    return
  }
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) {
    return
  }
  submitting.value = true
  try {
    const payload: MiddlewareInput = {
      name: form.name,
      mw_type: form.mw_type,
      host: form.host,
      port: Number(form.port),
      username: form.username,
      environment: form.environment,
      group_name: form.group_name,
      tags: form.tags,
      prom_job: form.prom_job,
      prom_instance: form.prom_instance,
    }
    // 编辑时回传原有 config：后端在缺省时也会保留，但显式回传能避免误清空
    // （集成中心创建的实例，其 config.integration 丢失后会从集成列表里消失）。
    if (isEdit.value && props.instance?.config) {
      payload.config = props.instance.config
    }
    if (form.password) {
      payload.password = form.password
    }
    if (isEdit.value && props.instance) {
      await middlewareApi.update(props.instance.id, payload)
      ElMessage({ type: 'success', message: '实例已更新' })
    } else {
      await middlewareApi.create(payload)
      ElMessage({ type: 'success', message: '实例已创建' })
    }
    emit('saved')
    visible.value = false
  } catch (error) {
    toastError(error)
  } finally {
    submitting.value = false
  }
}

watch(
  () => props.modelValue,
  async (open) => {
    if (!open) {
      return
    }
    reset()
    try {
      const options = await middlewareApi.options()
      types.value = options.types
      groups.value = options.groups
      promJobs.value = options.prom_jobs || []
      if (options.environments?.length) {
        environments.value = options.environments.map((value) => ({
          value,
          label: value === 'dev' ? '开发' : value === 'staging' ? '预发' : value === 'prod' ? '生产' : value,
        }))
      }
    } catch {
      // 选项拉取失败不阻塞表单。
    }
  },
)
</script>

<template>
  <el-dialog v-model="visible" :title="isEdit ? '编辑中间件实例' : '新增中间件实例'" width="560px" :close-on-click-modal="false">
    <el-form ref="formRef" :model="form" :rules="rules" label-position="top">
      <el-row :gutter="12">
        <el-col :xs="24" :sm="12">
          <el-form-item label="实例名称" prop="name">
            <el-input v-model="form.name" placeholder="如 prod-redis-order" />
          </el-form-item>
        </el-col>
        <el-col :xs="24" :sm="12">
          <el-form-item label="中间件类型" prop="mw_type">
            <el-select v-model="form.mw_type" class="mobile-block" @change="onTypeChange">
              <el-option
                v-for="item in types"
                :key="item.value"
                :label="item.phase === 2 ? `${item.label}（已纳管，监控告警为二期）` : item.label"
                :value="item.value"
              />
            </el-select>
          </el-form-item>
        </el-col>
        <el-col :xs="24" :sm="16">
          <el-form-item label="连接地址" prop="host">
            <el-input v-model="form.host" placeholder="IP 或域名" />
          </el-form-item>
        </el-col>
        <el-col :xs="24" :sm="8">
          <el-form-item label="端口" prop="port">
            <el-input v-model.number="form.port" type="number" />
          </el-form-item>
        </el-col>
        <el-col :xs="24" :sm="12">
          <el-form-item label="监控账号（只读）">
            <el-input v-model="form.username" placeholder="建议使用最小权限只读账号" />
          </el-form-item>
        </el-col>
        <el-col :xs="24" :sm="12">
          <el-form-item :label="isEdit ? '密码（留空表示不修改）' : '密码'">
            <el-input v-model="form.password" type="password" show-password placeholder="AES-256 加密存储" />
          </el-form-item>
        </el-col>
        <el-col :xs="24" :sm="12">
          <el-form-item label="环境">
            <el-select v-model="form.environment" class="mobile-block">
              <el-option v-for="item in environments" :key="item.value" :label="item.label" :value="item.value" />
            </el-select>
          </el-form-item>
        </el-col>
        <el-col :xs="24" :sm="12">
          <el-form-item label="分组">
            <el-select v-model="form.group_name" class="mobile-block" filterable allow-create clearable placeholder="如 payment">
              <el-option v-for="item in groups" :key="item" :label="item" :value="item" />
            </el-select>
          </el-form-item>
        </el-col>
        <el-col :xs="24" :sm="12">
          <el-form-item label="Prometheus job（可选）">
            <el-select
              v-model="form.prom_job"
              class="mobile-block"
              filterable
              allow-create
              clearable
              default-first-option
              :placeholder="promJobs.length ? '从 Prometheus 现有 job 中选择' : '默认 middleware-exporter-<类型>'"
            >
              <el-option v-for="job in promJobs" :key="job" :label="job" :value="job" />
            </el-select>
          </el-form-item>
          <p class="field-hint">
            这里填的是 <span class="mono">prometheus.yml</span> 里的 job_name（如
            <span class="mono">middleware-exporter-redis</span>），<b>不是容器名</b>（如
            <span class="mono">jd-redis-exporter</span>）。留空则按前缀自动匹配。
          </p>
        </el-col>
        <el-col :xs="24" :sm="12">
          <el-form-item label="Prometheus instance（可选）">
            <el-input v-model="form.prom_instance" placeholder="如 10.0.0.1:6379；与实例名二选一" />
          </el-form-item>
          <p class="field-hint">
            填了它就用 <span class="mono">instance</span> 标签匹配，<b>实例名称不再参与</b>。
            jd 这类自建 Exporter 只上报 <span class="mono">instance_name</span>，请留空。
          </p>
        </el-col>
        <el-col :span="24">
          <el-form-item label="标签">
            <el-select v-model="form.tags" multiple filterable allow-create default-first-option class="mobile-block" placeholder="回车添加标签" />
          </el-form-item>
        </el-col>
      </el-row>

      <el-alert
        v-if="form.environment === 'prod'"
        type="warning"
        :closable="false"
        show-icon
        title="生产环境实例：删除操作将强制走审批流程（L2）"
      />

      <div v-if="testResult" class="test-result" :class="testResult.success ? 'ok' : 'fail'">
        <el-icon><CircleCheckFilled v-if="testResult.success" /><CircleCloseFilled v-else /></el-icon>
        <span>{{ testResult.message }}</span>
        <span v-if="testResult.latency_ms" class="mono">{{ testResult.latency_ms }} ms</span>
      </div>
    </el-form>

    <template #footer>
      <div class="dialog-footer">
        <el-button :loading="testing" @click="handleTest">连接测试</el-button>
        <el-button v-if="isEdit" :loading="testing" @click="handleHealth">立即健康探测</el-button>
        <div class="spacer" />
        <el-button @click="visible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="handleSubmit">保存</el-button>
      </div>
    </template>
  </el-dialog>
</template>

<style scoped>
.field-hint {
  margin: 2px 0 0;
  font-size: 11.5px;
  line-height: 1.6;
  color: var(--c-text-muted);
}

.test-result {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 10px;
  border-radius: var(--r-md);
  font-size: 12.5px;
  margin-top: var(--sp-2);
}

.test-result.ok {
  background: var(--c-success-soft);
  color: var(--c-success);
}

.test-result.fail {
  background: var(--c-danger-soft);
  color: var(--c-danger);
}

.dialog-footer {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
</style>
