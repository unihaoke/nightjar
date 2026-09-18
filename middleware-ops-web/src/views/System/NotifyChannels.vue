<script setup lang="ts">
/**
 * 通知渠道管理：飞书 / 企微 / 钉钉 / 邮件。
 *
 * 密钥安全约定：webhook、签名密钥、邮箱口令都只写不读 ——
 * 接口只返回掩码，输入框留空 = 不修改，点「清除」= 保存后清空（clear_* 字段）。
 * 每个渠道可单独「发送测试」，结果就地展示。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { settingApi } from '@/api'
import { toastError } from '@/api/http'
import type { NotifySettingsInput, NotifySettingsView, NotifyTestResult } from '@/api/types'
import { formatTime } from '@/utils/format'
import { useUserStore } from '@/stores/user'

/** webhook 类渠道键（与接口字段同名，便于按 key 遍历渲染）。 */
type ChannelKey = 'feishu' | 'wecom' | 'dingtalk'

/** 单个 webhook 渠道的表单模型。 */
interface WebhookForm {
  enabled: boolean
  webhook: string
  webhookClear: boolean
  secret: string
  secretClear: boolean
  mentions: string[]
}

const CHANNELS: { key: ChannelKey; title: string; hint: string }[] = [
  { key: 'feishu', title: '飞书（feishu）', hint: '自定义机器人 Webhook；配置签名校验时需填「签名密钥」' },
  { key: 'wecom', title: '企业微信（wecom）', hint: '群机器人 Webhook，支持文本中 @ 成员' },
  { key: 'dingtalk', title: '钉钉（dingtalk）', hint: '自定义机器人 Webhook，加签密钥可留空' },
]

const store = useUserStore()

const loading = ref(false)
const saving = ref(false)
/** 渠道原始返回：用于展示 updated_by / updated_at / 掩码等只读信息。 */
const view = ref<NotifySettingsView | null>(null)
/** 正在发送测试的渠道键。 */
const testingChannel = ref('')
/** 各渠道测试结果（就地展示）。 */
const testResults = reactive<Record<string, NotifyTestResult | null>>({})

/** 总开关与卡片确认落地页。 */
const form = reactive({ enabled: true, card_confirm_path: '' })

/**
 * webhook 渠道表单（结构一致，合并成一张表避免重复代码）。
 *
 * 与 AI 设置同理，必须是 reactive：模板里按 key 取子对象再 v-model
 * （`channels[item.key].enabled` / `.webhook` / `.mentions`），普通对象不会触发重渲染（INC-020）。
 */
const channels = reactive<Record<ChannelKey, WebhookForm>>({
  feishu: { enabled: false, webhook: '', webhookClear: false, secret: '', secretClear: false, mentions: [] },
  wecom: { enabled: false, webhook: '', webhookClear: false, secret: '', secretClear: false, mentions: [] },
  dingtalk: { enabled: false, webhook: '', webhookClear: false, secret: '', secretClear: false, mentions: [] },
})

/** 后端返回的掩码，仅用于 placeholder 提示。 */
const webhookMasked: Record<ChannelKey, string> = reactive({ feishu: '', wecom: '', dingtalk: '' })

/** 邮件渠道表单。 */
const email = reactive({
  enabled: false,
  host: '',
  port: 587,
  username: '',
  password: '',
  passwordClear: false,
  from: '',
  to: [] as string[],
  use_tls: true,
})
const emailPasswordSet = ref(false)

const canWrite = computed(() => store.can('system:config:write'))

/** webhook 输入框 placeholder：掩码 / 已填新值 / 待清空三种语义。 */
function webhookPlaceholder(key: ChannelKey): string {
  const item = channels[key]
  if (item.webhookClear) {
    return '保存后将清空已存 Webhook'
  }
  if (item.webhook) {
    return '将替换为新 Webhook'
  }
  return webhookMasked[key] ? `已保存：${webhookMasked[key]}，留空表示不修改` : '尚未配置，粘贴机器人 Webhook 后保存'
}

/** 签名密钥 placeholder。 */
function secretPlaceholder(key: ChannelKey): string {
  if (channels[key].secretClear) {
    return '保存后将清空已存签名密钥'
  }
  return '留空表示不修改；无签名校验时无需填写'
}

/** 邮件口令 placeholder。 */
const passwordPlaceholder = computed(() => {
  if (email.passwordClear) {
    return '保存后将清空已存口令'
  }
  if (email.password) {
    return '将替换为新口令'
  }
  return emailPasswordSet.value ? '已保存口令，留空表示不修改' : '尚未配置'
})

/** 新填了密钥就不再请求清空（新值优先）。 */
function onSecretInput(kind: 'webhook' | 'secret' | 'password', key?: ChannelKey): void {
  if (kind === 'password') {
    if (email.password) {
      email.passwordClear = false
    }
    return
  }
  if (!key) {
    return
  }
  const item = channels[key]
  if (kind === 'webhook' && item.webhook) {
    item.webhookClear = false
  }
  if (kind === 'secret' && item.secret) {
    item.secretClear = false
  }
}

/** 切换「清除」标记。 */
function toggleClear(kind: 'webhook' | 'secret' | 'password', key?: ChannelKey): void {
  if (kind === 'password') {
    email.passwordClear = !email.passwordClear
    if (email.passwordClear) {
      email.password = ''
    }
    return
  }
  if (!key) {
    return
  }
  const item = channels[key]
  if (kind === 'webhook') {
    item.webhookClear = !item.webhookClear
    if (item.webhookClear) {
      item.webhook = ''
    }
  } else {
    item.secretClear = !item.secretClear
    if (item.secretClear) {
      item.secret = ''
    }
  }
}

/** 拉取通知渠道设置。 */
async function load(): Promise<void> {
  loading.value = true
  try {
    const data = await settingApi.notify()
    view.value = data
    form.enabled = data.enabled
    form.card_confirm_path = data.card_confirm_path || ''
    for (const item of CHANNELS) {
      const source = data[item.key]
      Object.assign(channels[item.key], {
        enabled: source?.enabled ?? false,
        webhook: '',
        webhookClear: false,
        secret: '',
        secretClear: false,
        mentions: [...(source?.mentions || [])],
      })
      webhookMasked[item.key] = source?.webhook_masked || ''
    }
    Object.assign(email, {
      enabled: data.email?.enabled ?? false,
      host: data.email?.host || '',
      port: data.email?.port ?? 587,
      username: data.email?.username || '',
      password: '',
      passwordClear: false,
      from: data.email?.from || '',
      to: [...(data.email?.to || [])],
      use_tls: data.email?.use_tls ?? true,
    })
    emailPasswordSet.value = data.email?.password_set ?? false
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

/** 组装单个 webhook 渠道入参：空值不下发，避免把占位符写进后端。 */
function buildChannel(key: ChannelKey): NotifySettingsInput['feishu'] {
  const item = channels[key]
  const payload: NotifySettingsInput['feishu'] = {
    enabled: item.enabled,
    mentions: item.mentions || [],
  }
  const webhook = item.webhook.trim()
  if (webhook) {
    payload.webhook = webhook
  }
  if (item.webhookClear) {
    payload.clear_webhook = true
  }
  const secret = item.secret.trim()
  if (secret) {
    payload.secret = secret
  }
  if (item.secretClear) {
    payload.clear_secret = true
  }
  return payload
}

/** 保存全部渠道设置。 */
async function save(): Promise<void> {
  saving.value = true
  try {
    const payload: NotifySettingsInput = {
      enabled: form.enabled,
      feishu: buildChannel('feishu'),
      wecom: buildChannel('wecom'),
      dingtalk: buildChannel('dingtalk'),
      email: {
        enabled: email.enabled,
        host: email.host,
        port: Number(email.port) || 0,
        username: email.username,
        ...(email.password.trim() ? { password: email.password.trim() } : {}),
        ...(email.passwordClear ? { clear_password: true } : {}),
        from: email.from,
        to: email.to,
        use_tls: email.use_tls,
      },
      card_confirm_path: form.card_confirm_path,
    }
    view.value = await settingApi.saveNotify(payload)
    ElMessage({ type: 'success', message: '通知渠道设置已保存' })
    await load()
  } catch (error) {
    toastError(error)
  } finally {
    saving.value = false
  }
}

/** 单渠道发送测试消息。 */
async function sendTest(key: string): Promise<void> {
  testingChannel.value = key
  testResults[key] = null
  try {
    testResults[key] = await settingApi.testNotify(key)
    if (testResults[key]?.ok) {
      ElMessage({ type: 'success', message: `${key} 测试消息已发送` })
    }
  } catch (error) {
    toastError(error)
  } finally {
    testingChannel.value = ''
  }
}

onMounted(load)
</script>

<template>
  <div class="page" v-loading="loading">
    <div class="page-header">
      <div>
        <h2 class="page-title">通知渠道</h2>
        <p class="page-subtitle">
          飞书 / 企微 / 钉钉 / 邮件四个通道；Webhook、签名密钥与邮箱口令只写不读，页面只显示掩码
        </p>
      </div>
      <div class="row">
        <el-button :icon="'Refresh'" size="small" @click="load">刷新</el-button>
        <el-button
          v-if="canWrite"
          type="primary"
          size="small"
          :icon="'Check'"
          :loading="saving"
          @click="save"
        >
          保存全部
        </el-button>
      </div>
    </div>

    <!-- 总开关 -->
    <div class="card">
      <h3 class="card-title">
        <span>总开关</span>
        <el-tag size="small" effect="plain" :type="view?.source === 'env' ? 'warning' : 'success'">
          {{ view?.source === 'env' ? '来源：环境变量（env）' : '来源：平台设置（platform）' }}
        </el-tag>
      </h3>
      <div class="row switch-row">
        <el-switch v-model="form.enabled" :disabled="!canWrite" />
        <span>启用平台通知（关闭后所有渠道一律不发消息，含告警与审批卡片）</span>
      </div>
      <div class="kv-list mt">
        <div class="kv">
          <span>最近更新</span>
          <span class="mono">
            {{ formatTime(view?.updated_at) }}
            <span v-if="view?.updated_by" class="muted">（{{ view.updated_by }}）</span>
          </span>
        </div>
        <div class="kv">
          <span>已启用渠道</span>
          <span class="row">
            <el-tag v-for="item in CHANNELS" :key="item.key" size="small" effect="plain" class="mini-tag"
              :type="form.enabled && channels[item.key].enabled ? 'success' : 'info'">
              {{ item.title }}
            </el-tag>
            <el-tag size="small" effect="plain" class="mini-tag" :type="form.enabled && email.enabled ? 'success' : 'info'">
              邮件
            </el-tag>
          </span>
        </div>
      </div>
    </div>

    <!-- webhook 类渠道：飞书 / 企微 / 钉钉 -->
    <div v-for="item in CHANNELS" :key="item.key" class="card">
      <h3 class="card-title">
        <span>{{ item.title }}</span>
        <div class="row">
          <el-tag v-if="channels[item.key].webhookClear" size="small" type="danger" effect="plain">保存后清空 Webhook</el-tag>
          <el-tag v-else-if="channels[item.key].webhook" size="small" type="warning" effect="plain">保存后替换 Webhook</el-tag>
          <el-switch v-model="channels[item.key].enabled" :disabled="!canWrite" active-text="启用" />
        </div>
      </h3>

      <el-form label-position="top" :disabled="!canWrite">
        <el-row :gutter="12">
          <el-col :span="24">
            <el-form-item label="Webhook 地址">
              <div class="row key-row">
                <el-input
                  v-model="channels[item.key].webhook"
                  type="password"
                  show-password
                  autocomplete="new-password"
                  class="mono key-input"
                  :placeholder="webhookPlaceholder(item.key)"
                  @input="onSecretInput('webhook', item.key)"
                />
                <el-button
                  :type="channels[item.key].webhookClear ? 'danger' : 'default'"
                  :plain="channels[item.key].webhookClear"
                  :icon="'Delete'"
                  @click="toggleClear('webhook', item.key)"
                >
                  {{ channels[item.key].webhookClear ? '取消清除' : '清除' }}
                </el-button>
              </div>
            </el-form-item>
            <p class="field-hint">
              已存地址不显示明文；留空即不改动。
              <span v-if="webhookMasked[item.key]" class="mono">{{ webhookMasked[item.key] }}</span>
              <span v-else class="muted">当前未配置</span>
            </p>
          </el-col>

          <el-col :xs="24" :sm="12">
            <el-form-item label="签名密钥（可选）">
              <div class="row key-row">
                <el-input
                  v-model="channels[item.key].secret"
                  type="password"
                  show-password
                  autocomplete="new-password"
                  class="key-input"
                  :placeholder="secretPlaceholder(item.key)"
                  @input="onSecretInput('secret', item.key)"
                />
                <el-button
                  :type="channels[item.key].secretClear ? 'danger' : 'default'"
                  :plain="channels[item.key].secretClear"
                  :icon="'Delete'"
                  @click="toggleClear('secret', item.key)"
                >
                  {{ channels[item.key].secretClear ? '取消清除' : '清除' }}
                </el-button>
              </div>
            </el-form-item>
          </el-col>

          <el-col :xs="24" :sm="12">
            <el-form-item label="@ 成员（手机号 / 用户 ID，回车添加）">
              <el-select
                v-model="channels[item.key].mentions"
                multiple
                filterable
                allow-create
                default-first-option
                clearable
                class="mobile-block"
                placeholder="留空表示不 @ 任何人"
              />
            </el-form-item>
          </el-col>
        </el-row>
      </el-form>

      <div class="row actions">
        <el-button
          :loading="testingChannel === item.key"
          :icon="'Promotion'"
          :disabled="!canWrite"
          @click="sendTest(item.key)"
        >
          发送测试
        </el-button>
      </div>

      <el-alert
        v-if="testResults[item.key]"
        class="mt-sm"
        :type="testResults[item.key]?.ok ? 'success' : 'error'"
        :closable="false"
        show-icon
        :title="testResults[item.key]?.ok ? `发送成功：${testResults[item.key]?.channel}` : `发送失败：${testResults[item.key]?.channel}`"
      >
        <p class="field-hint">{{ testResults[item.key]?.message }}</p>
      </el-alert>
    </div>

    <!-- 邮件渠道 -->
    <div class="card">
      <h3 class="card-title">
        <span>邮件（email）</span>
        <div class="row">
          <el-tag v-if="email.passwordClear" size="small" type="danger" effect="plain">保存后清空口令</el-tag>
          <el-tag v-else-if="email.password" size="small" type="warning" effect="plain">保存后替换口令</el-tag>
          <el-switch v-model="email.enabled" :disabled="!canWrite" active-text="启用" />
        </div>
      </h3>

      <el-form label-position="top" :disabled="!canWrite">
        <el-row :gutter="12">
          <el-col :xs="24" :sm="16">
            <el-form-item label="SMTP 服务器">
              <el-input v-model="email.host" class="mono" placeholder="如 smtp.example.com" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="8">
            <el-form-item label="端口">
              <el-input-number
                v-model="email.port"
                :min="1"
                :max="65535"
                controls-position="right"
                class="mobile-block"
              />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="登录账号">
              <el-input v-model="email.username" class="mono" placeholder="如 ops@example.com" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="登录口令 / 授权码">
              <div class="row key-row">
                <el-input
                  v-model="email.password"
                  type="password"
                  show-password
                  autocomplete="new-password"
                  class="key-input"
                  :placeholder="passwordPlaceholder"
                  @input="onSecretInput('password')"
                />
                <el-button
                  :type="email.passwordClear ? 'danger' : 'default'"
                  :plain="email.passwordClear"
                  :icon="'Delete'"
                  @click="toggleClear('password')"
                >
                  {{ email.passwordClear ? '取消清除' : '清除' }}
                </el-button>
              </div>
            </el-form-item>
            <p class="field-hint">
              已存口令不显示明文；留空即不改动。
              <span :class="emailPasswordSet ? 'text-success' : 'muted'">
                {{ emailPasswordSet ? '当前已配置' : '当前未配置' }}
              </span>
            </p>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="发件人">
              <el-input v-model="email.from" class="mono" placeholder="如 中间件平台 <ops@example.com>" />
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="收件人（回车添加多个）">
              <el-select
                v-model="email.to"
                multiple
                filterable
                allow-create
                default-first-option
                clearable
                class="mobile-block"
                placeholder="如 ops@example.com"
              />
            </el-form-item>
          </el-col>
          <el-col :span="24">
            <el-form-item label="传输加密">
              <div class="row switch-row">
                <el-switch v-model="email.use_tls" :disabled="!canWrite" />
                <span>{{ email.use_tls ? '使用 TLS（587 / 465 常用）' : '明文传输（仅限内网测试）' }}</span>
              </div>
            </el-form-item>
          </el-col>
        </el-row>
      </el-form>

      <div class="row actions">
        <el-button
          :loading="testingChannel === 'email'"
          :icon="'Promotion'"
          :disabled="!canWrite"
          @click="sendTest('email')"
        >
          发送测试
        </el-button>
      </div>

      <el-alert
        v-if="testResults.email"
        class="mt-sm"
        :type="testResults.email?.ok ? 'success' : 'error'"
        :closable="false"
        show-icon
        :title="testResults.email?.ok ? `发送成功：${testResults.email?.channel}` : `发送失败：${testResults.email?.channel}`"
      >
        <p class="field-hint">{{ testResults.email?.message }}</p>
      </el-alert>
    </div>

    <!-- 卡片确认落地页 -->
    <div class="card">
      <h3 class="card-title">卡片确认落地页</h3>
      <el-form label-position="top" :disabled="!canWrite">
        <el-form-item label="card_confirm_path">
          <el-input v-model="form.card_confirm_path" class="mono" placeholder="如 /fix；留空由后端使用默认值" />
        </el-form-item>
      </el-form>
      <p class="field-hint">
        审批卡片上的「通过 / 驳回」按钮点开后跳转的站内路径；高危操作（L2）一律回到平台走审批，不在 IM 里直接执行。
      </p>
    </div>

    <div v-if="canWrite" class="row actions">
      <el-button type="primary" :loading="saving" :icon="'Check'" @click="save">保存全部渠道设置</el-button>
    </div>
    <p v-else class="muted foot">
      当前账号缺少 <span class="mono">system:config:write</span>：只能查看，保存与「发送测试」均已禁用。
    </p>
  </div>
</template>

<style scoped>
.mt {
  margin-top: 12px;
}

.mt-sm {
  margin-top: 8px;
}

.mini-tag {
  margin-right: 4px;
}

.switch-row {
  font-size: 12.5px;
  color: var(--c-text-2);
}

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

.key-row {
  flex-wrap: nowrap;
}

.key-input {
  flex: 1;
  min-width: 0;
}

.actions {
  margin-top: 10px;
}

.foot {
  margin: 10px 0 0;
  font-size: 11.5px;
  text-align: center;
}

.field-hint {
  margin: 2px 0 0;
  font-size: 11.5px;
  line-height: 1.6;
  color: var(--c-text-3);
}

@media (max-width: 767px) {
  .key-row {
    flex-wrap: wrap;
  }

  .key-input {
    width: 100%;
  }
}
</style>
