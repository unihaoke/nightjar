<script setup lang="ts">
/** 告警规则管理：CRUD、启用停用、通知渠道自检。 */
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { alertApi, metricsApi } from '@/api'
import { toastError } from '@/api/http'
import type { AlertRule, MiddlewareInstance } from '@/api/types'
import { alertLevelLabels, mwTypeLabels } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const route = useRoute()
const store = useUserStore()

const loading = ref(false)
const submitting = ref(false)
const dialogVisible = ref(false)
const editing = ref<AlertRule | null>(null)
const formRef = ref<FormInstance>()
const items = ref<AlertRule[]>([])
const total = ref(0)
const instances = ref<{ id: number; name: string; mw_type: string; environment: string }[]>([])
const metricOptions = ref<{ name: string; display_name: string }[]>([])
const notifyChannels = ref<{ channel: string; enabled: boolean }[]>([])

const query = reactive({
  instance_id: Number(route.query.instance_id || 0),
  page: 1,
  page_size: 20,
})

const canWrite = computed(() => store.can('alert:write'))

const form = reactive({
  name: '',
  instance_id: 0,
  mw_type: '',
  metric_name: '',
  operator: '>',
  threshold: 0,
  level: 'warning',
  time_window: 5,
  cooldown: 10,
  notify_channels: ['feishu', 'wecom'] as string[],
  enabled: true,
  ai_enabled: true,
  description: '',
})

const rules: FormRules = {
  name: [{ required: true, message: '请输入规则名称', trigger: 'blur' }],
  instance_id: [{ required: true, message: '请选择实例', trigger: 'change' }],
  metric_name: [{ required: true, message: '请选择或输入指标名', trigger: 'change' }],
  operator: [{ required: true, message: '请选择操作符', trigger: 'change' }],
}

/** 加载规则列表。 */
async function load(): Promise<void> {
  loading.value = true
  try {
    const result = await alertApi.rules({ ...query })
    items.value = result.list || []
    total.value = result.total || 0
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

/** 加载选项。 */
async function loadOptions(): Promise<void> {
  try {
    const options = await alertApi.options()
    instances.value = options.instances || []
    notifyChannels.value = options.notify_status || []
  } catch {
    // 选项失败不阻塞页面。
  }
}

/** 选择实例后拉取该类型的指标目录。 */
async function onInstanceChange(id: number): Promise<void> {
  const target = instances.value.find((item) => item.id === id)
  if (!target) {
    return
  }
  form.mw_type = target.mw_type
  try {
    const catalog = await metricsApi.catalog(target.mw_type)
    metricOptions.value = catalog.metrics || []
  } catch {
    metricOptions.value = []
  }
}

/** 打开表单。 */
function openForm(rule?: AlertRule): void {
  editing.value = rule || null
  dialogVisible.value = true
  if (rule) {
    Object.assign(form, {
      name: rule.name,
      instance_id: rule.instance_id,
      mw_type: rule.mw_type,
      metric_name: rule.metric_name,
      operator: rule.operator,
      threshold: rule.threshold,
      level: rule.level,
      time_window: rule.time_window,
      cooldown: rule.cooldown,
      notify_channels: rule.notify_channels || [],
      enabled: rule.enabled,
      ai_enabled: rule.ai_enabled,
      description: rule.description,
    })
    void onInstanceChange(rule.instance_id)
  } else {
    Object.assign(form, {
      name: '',
      instance_id: query.instance_id || 0,
      mw_type: '',
      metric_name: '',
      operator: '>',
      threshold: 0,
      level: 'warning',
      time_window: 5,
      cooldown: 10,
      notify_channels: ['feishu', 'wecom'],
      enabled: true,
      ai_enabled: true,
      description: '',
    })
    if (form.instance_id) {
      void onInstanceChange(form.instance_id)
    }
  }
  formRef.value?.clearValidate()
}

/** 提交。 */
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
    if (editing.value) {
      await alertApi.updateRule(editing.value.id, { ...form })
      ElMessage({ type: 'success', message: '规则已更新' })
    } else {
      await alertApi.createRule({ ...form })
      ElMessage({ type: 'success', message: '规则已创建' })
    }
    dialogVisible.value = false
    await load()
  } catch (error) {
    toastError(error)
  } finally {
    submitting.value = false
  }
}

/** 删除。 */
async function remove(rule: AlertRule): Promise<void> {
  try {
    await ElMessageBox.confirm(`确认删除规则「${rule.name}」？`, '删除确认', {
      confirmButtonText: '删除',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await alertApi.removeRule(rule.id)
    ElMessage({ type: 'success', message: '规则已删除' })
    await load()
  } catch (error) {
    toastError(error)
  }
}

/** 切换启用状态。 */
async function toggle(rule: AlertRule): Promise<void> {
  try {
    await alertApi.updateRule(rule.id, { ...rule, enabled: !rule.enabled })
    await load()
  } catch (error) {
    toastError(error)
  }
}

/** 通知渠道自检。 */
async function testNotify(channel: string): Promise<void> {
  try {
    await alertApi.notifyTest(channel)
    ElMessage({ type: 'success', message: `测试消息已发送（${channel}），请检查渠道` })
  } catch (error) {
    toastError(error)
  }
}

onMounted(async () => {
  await loadOptions()
  await load()
})
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2 class="page-title">告警规则</h2>
        <p class="page-subtitle">
          错误指纹 + 时间窗口去重 + 冷却期静默；IM 卡片仅支持通知与确认，不支持一键执行
        </p>
      </div>
      <el-button v-if="canWrite" type="primary" :icon="'Plus'" @click="openForm()">新建规则</el-button>
    </div>

    <div class="card notify-card">
      <span class="muted">通知渠道：</span>
      <el-tag
        v-for="item in notifyChannels"
        :key="item.channel"
        size="small"
        :type="item.enabled ? 'success' : 'info'"
        effect="plain"
        class="channel-tag"
        @click="item.enabled && testNotify(item.channel)"
      >
        {{ item.channel }}{{ item.enabled ? '（点击自检）' : '（未配置）' }}
      </el-tag>
    </div>

    <div class="card filters">
      <el-select v-model="query.instance_id" placeholder="全部实例" clearable filterable class="filter-item" @change="load">
        <el-option
          v-for="item in instances"
          :key="item.id"
          :label="`${item.name}（${mwTypeLabels[item.mw_type] || item.mw_type}）`"
          :value="item.id"
        />
      </el-select>
      <el-button type="primary" :icon="'Search'" @click="load">查询</el-button>
    </div>

    <div class="card" v-loading="loading">
      <div class="table-scroll">
        <el-table :data="items" size="default">
          <el-table-column prop="name" label="规则名称" min-width="150" show-overflow-tooltip />
          <el-table-column label="实例" width="90">
            <template #default="{ row }">#{{ row.instance_id }}</template>
          </el-table-column>
          <el-table-column label="条件" min-width="200">
            <template #default="{ row }">
              <span class="mono">{{ row.metric_name }} {{ row.operator }} {{ row.threshold }}</span>
            </template>
          </el-table-column>
          <el-table-column label="级别" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.level === 'critical' ? 'danger' : 'warning'" effect="light">
                {{ alertLevelLabels[row.level] || row.level }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="收敛策略" width="150">
            <template #default="{ row }">窗口 {{ row.time_window }}m · 冷却 {{ row.cooldown }}m</template>
          </el-table-column>
          <el-table-column label="通知渠道" min-width="140">
            <template #default="{ row }">
              <el-tag v-for="ch in row.notify_channels || []" :key="ch" size="small" effect="plain" class="mini-tag">{{ ch }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="启用" width="80">
            <template #default="{ row }">
              <el-switch :model-value="row.enabled" :disabled="!canWrite" @change="toggle(row)" />
            </template>
          </el-table-column>
          <el-table-column label="操作" width="130" fixed="right">
            <template #default="{ row }">
              <el-button v-if="canWrite" text size="small" @click="openForm(row)">编辑</el-button>
              <el-button v-if="canWrite" text size="small" type="danger" @click="remove(row)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
      </div>
      <el-pagination
        v-model:current-page="query.page"
        v-model:page-size="query.page_size"
        :total="total"
        layout="total, prev, pager, next"
        class="pager"
        @current-change="load"
      />
    </div>

    <el-dialog v-model="dialogVisible" :title="editing ? '编辑告警规则' : '新建告警规则'" width="600px">
      <el-form ref="formRef" :model="form" :rules="rules" label-position="top">
        <el-form-item label="规则名称" prop="name">
          <el-input v-model="form.name" placeholder="如 Redis 内存使用率过高" />
        </el-form-item>
        <el-row :gutter="12">
          <el-col :xs="24" :sm="12">
            <el-form-item label="监控实例" prop="instance_id">
              <el-select v-model="form.instance_id" filterable class="mobile-block" @change="onInstanceChange">
                <el-option
                  v-for="item in instances"
                  :key="item.id"
                  :label="`${item.name}（${mwTypeLabels[item.mw_type] || item.mw_type}）`"
                  :value="item.id"
                />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="指标" prop="metric_name">
              <el-select v-model="form.metric_name" filterable allow-create class="mobile-block" placeholder="选择或输入指标名">
                <el-option v-for="item in metricOptions" :key="item.name" :label="`${item.display_name}（${item.name}）`" :value="item.name" />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :xs="12" :sm="8">
            <el-form-item label="操作符" prop="operator">
              <el-select v-model="form.operator" class="mobile-block">
                <el-option v-for="op in ['>', '>=', '<', '<=', '==', '!=']" :key="op" :label="op" :value="op" />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :xs="12" :sm="8">
            <el-form-item label="阈值">
              <el-input v-model.number="form.threshold" type="number" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="8">
            <el-form-item label="级别">
              <el-select v-model="form.level" class="mobile-block">
                <el-option label="警告" value="warning" />
                <el-option label="严重" value="critical" />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :xs="12" :sm="12">
            <el-form-item label="去重窗口（分钟）">
              <el-input-number v-model="form.time_window" :min="1" :max="120" class="mobile-block" />
            </el-form-item>
          </el-col>
          <el-col :xs="12" :sm="12">
            <el-form-item label="冷却期（分钟）">
              <el-input-number v-model="form.cooldown" :min="0" :max="1440" class="mobile-block" />
            </el-form-item>
          </el-col>
          <el-col :span="24">
            <el-form-item label="通知渠道">
              <el-checkbox-group v-model="form.notify_channels">
                <el-checkbox v-for="item in notifyChannels" :key="item.channel" :value="item.channel" :disabled="!item.enabled">
                  {{ item.channel }}
                </el-checkbox>
              </el-checkbox-group>
            </el-form-item>
          </el-col>
          <el-col :span="24">
            <el-form-item label="规则说明">
              <el-input v-model="form.description" type="textarea" :rows="2" placeholder="补充该规则的处置口径" />
            </el-form-item>
          </el-col>
          <el-col :span="24">
            <el-form-item>
              <el-checkbox v-model="form.enabled">启用规则</el-checkbox>
              <el-checkbox v-model="form.ai_enabled">触发时自动进行 AI 诊断</el-checkbox>
            </el-form-item>
          </el-col>
        </el-row>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="submit">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.notify-card {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}

.channel-tag {
  cursor: pointer;
}

.filters {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}

.filter-item {
  width: 260px;
}

.mini-tag {
  margin-right: 4px;
}

.pager {
  margin-top: 12px;
  justify-content: flex-end;
}

@media (max-width: 767px) {
  .filter-item {
    width: 100%;
  }
}
</style>
