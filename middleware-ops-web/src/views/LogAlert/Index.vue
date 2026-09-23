<script setup lang="ts">
/**
 * 日志告警中心（4.8）：事件列表 + 堆栈详情 + 三点式 AI 代码分析。
 *
 * 采集入口有两条：
 *   - 日志集成：目标机上的 Filebeat（平台用 Ansible 装）→ 平台自带 Kafka → 平台消费入库；
 *   - 应用直推：POST /api/hooks/logs（零侵入兜底）。
 * 入库后由「日志告警规则」（/log-alerts/rules）决定去重窗口、冷却期、通知渠道与是否 AI 分析，
 * 事件上因此带着 rule_id / dedup_window / cooldown_until / notified_at / analysis_state，
 * 页面必须把它们展示出来——否则使用者无法回答"这条告警为什么没通知我"。
 * 顶部「Kafka 采集链路」卡片回答"采集这条路现在通不通"，避免只看到事件列表却不知道链路状态。
 */
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import { aiApi, logAlertApi } from '@/api'
import { toastError } from '@/api/http'
import type { CodeAnalysis, LogEvent, LogPipelineProbeResult, LogPipelineStatus } from '@/api/types'
import { useListPage } from '@/composables/useListPage'
import RefreshControl from '@/components/RefreshControl.vue'
import ResponsiveList from '@/components/ResponsiveList.vue'
import StatCard from '@/components/StatCard.vue'
import { formatTime } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const store = useUserStore()

const analyzing = ref(false)
/** 重新分析是否在提交中（与「AI 代码分析」分开，两个按钮的进度互不影响）。 */
const reanalyzing = ref(false)
const detailVisible = ref(false)
const current = ref<LogEvent | null>(null)
const analysis = ref<CodeAnalysis | null>(null)
const analyzeResult = ref<Record<string, unknown> | null>(null)

/**
 * AI 分析状态 → 标签文案与颜色（与后端 AnalysisState 常量一一对应）。
 *
 * 状态是"处理过程"的一部分：failed 要能看出失败、disabled 要能看出是规则关了 AI，
 * 否则两种情况都会表现成"没有报告"，让人误以为是分析还没跑完。
 */
const ANALYSIS_STATE_META: Record<string, { label: string; type: 'success' | 'warning' | 'danger' | 'info' }> = {
  pending: { label: '待分析', type: 'info' },
  running: { label: '分析中', type: 'warning' },
  // awaiting：已把问题提交给外部 AI 服务，等它回调/轮询结论（可能要几分钟）。
  awaiting: { label: 'AI 分析中', type: 'warning' },
  done: { label: '已分析', type: 'success' },
  failed: { label: '分析失败', type: 'danger' },
  disabled: { label: '未启用', type: 'info' },
}

/**
 * 取状态元信息。
 *
 * 空状态（后端未返回该字段）显示「无数据」而不是「待分析」：
 * 把"不知道"渲染成"排队中"会让人等一个永远不会来的结果（INC-016）。
 */
function analysisMeta(state: string): { label: string; type: 'success' | 'warning' | 'danger' | 'info' } {
  return ANALYSIS_STATE_META[state] || { label: state || '无数据', type: 'info' }
}

/** 去重窗口文案：0 是明确配置（不合并），要写清楚，避免被读成"没配"。 */
function dedupText(value: number | null | undefined): string {
  if (value === null || value === undefined) {
    return ''
  }
  return value > 0 ? `窗口 ${value} 分钟` : '窗口 0 分钟（不去重）'
}

/** 日志事件由应用主动上报，靠轮询才能看到最新的，默认开启。 */
const POLL_INTERVAL = 60_000

const list = useListPage<LogEvent>({
  fetch: (params, signal) => logAlertApi.events(params, signal),
  defaults: { keyword: '', service: '', status: '', alert_type: '', page: 1, page_size: 20 },
  pollInterval: POLL_INTERVAL,
})
const { query, items, total, loading, error, polling, lastLoadedAt } = list

const canWrite = computed(() => store.can('logalert:write'))

// ---------------------------------------------------------------------------
// Kafka 采集链路：Filebeat 推送到平台 Kafka，消费者入库后才会有日志事件
// ---------------------------------------------------------------------------
const pipeline = ref<LogPipelineStatus | null>(null)
const pipelineLoading = ref(false)
/**
 * 拉取失败的原因。
 *
 * 为什么单独存一份而不是复用 pipeline=null：接口不通与"后端没配 Kafka"是两件事，
 * 界面必须区分「无数据（读不到）」与「未启用」——否则会像 INC-016 那样把未知显示成结论。
 */
const pipelineError = ref('')
const probing = ref(false)
const probeResult = ref<LogPipelineProbeResult | null>(null)

/** 读取采集链路现状；失败即视为无数据，绝不把 null 当成 0 或"未启用"。 */
async function loadPipeline(): Promise<void> {
  pipelineLoading.value = true
  try {
    const result = await logAlertApi.pipeline()
    if (!result) {
      pipeline.value = null
      pipelineError.value = '接口未返回采集链路数据'
      return
    }
    pipeline.value = result
    pipelineError.value = ''
  } catch (err) {
    pipeline.value = null
    pipelineError.value = err instanceof Error ? err.message : String(err)
  } finally {
    pipelineLoading.value = false
  }
}

/** 手动刷新：列表与链路一起刷，避免"数据到了但卡片还是旧的"。 */
function refreshAll(): void {
  void list.load()
  void loadPipeline()
}

/** 测试 Kafka 连接：结果就地展示，不弹 toast（需要看到延迟等细节）。 */
async function probePipeline(): Promise<void> {
  probing.value = true
  probeResult.value = null
  try {
    probeResult.value = await logAlertApi.probePipeline()
  } catch (err) {
    toastError(err)
  } finally {
    probing.value = false
  }
}

/** brokers 列表展示：空数组代表没有可用的 broker，不能说成 0。 */
const brokersText = computed(() => {
  const brokers = pipeline.value?.brokers || []
  return brokers.length > 0 ? brokers.join(', ') : ''
})

/**
 * 列表刷新成功后同步刷新链路卡片。
 *
 * 自动轮询只驱动列表（useListPage 内部实现），卡片若不同步就会一直停在进页面那一刻的数字上。
 * 首次变化跳过：进页面时 onMounted 已经读过一次，不必重复请求。
 */
let pipelineFollowedList = false
watch(lastLoadedAt, (stamp) => {
  if (!stamp) {
    return
  }
  if (!pipelineFollowedList) {
    pipelineFollowedList = true
    return
  }
  void loadPipeline()
})

const route = useRoute()

/**
 * 支持从 IM 卡片直达：链接形如 /log-alerts?event_id=123。
 *
 * 落点是**列表页**（能看到同一时间还有哪些告警），再自动展开这一条的详情抽屉——
 * 只展开详情会让人失去上下文，只落在列表又得自己翻找，两者都要。
 */
onMounted(async () => {
  void loadPipeline()
  const raw = route.query.event_id
  const id = Number(Array.isArray(raw) ? raw[0] : raw)
  if (Number.isFinite(id) && id > 0) {
    await openEventById(id)
  }
})

/** 读取事件详情：列表行是聚合结果，详情带完整堆栈与规则处理结果。 */
async function loadDetail(id: number): Promise<void> {
  const detail = await logAlertApi.event(id)
  current.value = detail.event
  analysis.value = detail.analysis
}

/**
 * 打开指定事件的详情。
 *
 * 抽出来是为了让「IM 卡片带 event_id 跳进来」与「在列表里点一行」走同一条路径：
 * 之前 URL 上的 event_id 没人读，飞书卡片点了只落在列表上，还得自己翻找那一条。
 */
async function openEventById(id: number): Promise<void> {
  detailVisible.value = true
  analyzeResult.value = null
  try {
    await loadDetail(id)
  } catch (error) {
    toastError(error)
    detailVisible.value = false
  }
}

/** 查看详情。 */
function openDetail(event: LogEvent): void {
  void openEventById(event.id)
}

/** 触发 AI 代码分析。 */
async function analyze(): Promise<void> {
  if (!current.value) {
    return
  }
  analyzing.value = true
  try {
    const result = await aiApi.codeAnalyze({ event_id: current.value.id })
    analyzeResult.value = result.report
    ElMessage({ type: 'success', message: '代码分析完成' })
    await loadDetail(current.value.id)
    await list.load()
  } catch (error) {
    toastError(error)
  } finally {
    analyzing.value = false
  }
}

/**
 * 重新分析（规则链路）。
 *
 * 与「AI 代码分析」的区别：那个走 /api/ai/code-analyze（现场把堆栈交给 AI 并回写报告），
 * 这个走 /api/log-alerts/events/:id/reanalyze（按当前规则重新排队，规则关了 AI 时后端会明确拒绝）。
 * 失败原因（如"该规则已关闭 AI 分析""该服务未配置代码仓库"）直接展示后端 message，不自行改写。
 */
async function reanalyze(): Promise<void> {
  if (!current.value) {
    return
  }
  reanalyzing.value = true
  try {
    const result = await logAlertApi.reanalyze(current.value.id)
    ElMessage({ type: 'success', message: result.message || '已触发重新分析' })
    await loadDetail(current.value.id)
    await list.load()
  } catch (error) {
    toastError(error)
  } finally {
    reanalyzing.value = false
  }
}

/** 更新事件状态。 */
async function updateStatus(status: string): Promise<void> {
  if (!current.value) {
    return
  }
  try {
    await logAlertApi.updateStatus(current.value.id, status)
    ElMessage({ type: 'success', message: '状态已更新' })
    current.value.status = status
    await list.load()
  } catch (error) {
    toastError(error)
  }
}

/** 复制 Hook 接入示例。 */
async function copyHookExample(): Promise<void> {
  const example = `curl -X POST http://<平台地址>/api/hooks/logs \\
  -H "Content-Type: application/json" \\
  -H "X-Hook-Token: <MWOPS_HOOK_TOKEN>" \\
  -d '{"service":"order-service","level":"ERROR","message":"Order 10086 处理失败","stacktrace":"java.lang.NullPointerException\\n\\tat com.demo.OrderService.process(OrderService.java:42)"}'`
  try {
    await navigator.clipboard.writeText(example)
    ElMessage({ type: 'success', message: '接入示例已复制' })
  } catch {
    ElMessage({ type: 'info', message: example })
  }
}
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2 class="page-title">日志告警</h2>
        <p class="page-subtitle">
          日志集成（Filebeat → 平台 Kafka）或 HTTP Hook（零侵入）上报 ERROR/堆栈，平台按错误指纹聚合去重
        </p>
      </div>
      <el-button size="small" :icon="'CopyDocument'" @click="copyHookExample">复制接入示例</el-button>
    </div>

    <div class="row toolbar-row">
      <RefreshControl
        v-model="polling"
        :interval-ms="POLL_INTERVAL"
        :last-loaded-at="lastLoadedAt"
        @refresh="refreshAll"
      />
    </div>

    <!-- Kafka 采集链路：先确认"采集这条路通不通"，再看事件列表 -->
    <div v-loading="pipelineLoading" class="card pipeline-card">
      <h3 class="card-title">
        <span>Kafka 采集链路</span>
        <span class="row">
          <el-tag
            v-if="pipeline"
            size="small"
            effect="light"
            :type="pipeline.enabled ? (pipeline.running ? 'success' : 'warning') : 'info'"
          >
            {{ pipeline.enabled ? (pipeline.running ? '运行中' : '已启用但消费者未运行') : '未启用' }}
          </el-tag>
          <el-button size="small" :loading="probing" :disabled="!canWrite" @click="probePipeline">测试 Kafka 连接</el-button>
        </span>
      </h3>

      <!-- 读不到就明确说"无数据"：不能显示成 0，也不能当成"未启用"（INC-016） -->
      <el-alert v-if="!pipeline && pipelineError" type="warning" :closable="false" show-icon
        title="无数据：无法获取 Kafka 采集链路状态">
        <p class="field-hint">{{ pipelineError }}</p>
        <el-button text size="small" @click="loadPipeline">重试</el-button>
      </el-alert>
      <p v-else-if="!pipeline" class="muted">正在读取采集链路状态…</p>

      <template v-else>
        <el-alert v-if="!pipeline.enabled" type="info" :closable="false" show-icon class="mb"
          title="未配置 Kafka：日志集成不可用">
          <p class="field-hint">
            {{ pipeline.note || '平台没有可用的 Kafka broker，目标机上的 Filebeat 无处可推。请先在平台配置 kafka.brokers 与对外地址后重启后端。' }}
          </p>
          <p class="field-hint">
            日志集成的自检会区分「平台侧能否连上 Kafka」与「目标机能否连上 Kafka」，两处都不通时先看这一项。
          </p>
        </el-alert>

        <!-- 未启用时没有消费者在跑：三个计数只会是 0，展示 0 等于把"没有数据"说成结论（INC-016） -->
        <el-row v-if="pipeline.enabled" :gutter="12" class="mb">
          <el-col :xs="12" :sm="8">
            <StatCard
              label="累计已消费"
              :value="pipeline.consumed_total"
              :status="pipeline.running ? 'ok' : 'neutral'"
              :hint="pipeline.persistent
                ? '跨重启、跨副本累加的历史总量（与 Kafka 消费组的已提交位点同口径）'
                : '未启用计数落库，此值等于本次启动以来的条数'"
            />
          </el-col>
          <el-col :xs="12" :sm="8">
            <StatCard
              label="入库条数"
              :value="pipeline.ingested"
              :status="'ok'"
              hint="真正进入日志事件的条数（窗口内同指纹会合并进既有事件，因此它可能小于事件新增数）"
            />
          </el-col>
          <el-col :xs="24" :sm="8">
            <StatCard
              label="有意忽略"
              :value="pipeline.ignored"
              :status="pipeline.ignored > 0 ? 'warning' : 'neutral'"
              hint="命中屏蔽项或未命中任何告警规则：平台处理了但按配置不入库（已消费 = 入库 + 有意忽略）"
            />
          </el-col>
        </el-row>
        <el-row v-if="pipeline.enabled" :gutter="12" class="mb">
          <el-col :xs="12" :sm="8">
            <StatCard
              label="本次启动以来"
              :value="pipeline.consumed"
              :status="'neutral'"
              hint="进程重启会归零；用来判断现在是否仍在收日志"
            />
          </el-col>
          <el-col :xs="12" :sm="8">
            <StatCard
              label="丢弃条数"
              :value="pipeline.dropped_total"
              :status="pipeline.dropped_total > 0 ? 'warning' : 'neutral'"
              hint="解析失败被跳过的脏消息（照常提交位点，因此 Kafka 侧算已消费，平台不计入）"
            />
          </el-col>
          <el-col :xs="24" :sm="8">
            <StatCard
              label="失败条数"
              :value="pipeline.failed_total"
              :status="pipeline.failed_total > 0 ? 'critical' : 'neutral'"
              hint="入库失败（成功前不提交位点，会重试同一条）"
            />
          </el-col>
        </el-row>
        <p v-else class="field-hint">未启用 Kafka 时没有消费者在跑，因此不展示消费计数（此时 0 不代表任何结论）。</p>

        <el-descriptions :column="2" border size="small">
          <el-descriptions-item label="启用状态">{{ pipeline.enabled ? '已启用' : '未启用' }}</el-descriptions-item>
          <el-descriptions-item label="消费者">
            <el-tag size="small" effect="light" :type="pipeline.running ? 'success' : 'info'">
              {{ pipeline.running ? '运行中' : '未运行' }}
            </el-tag>
          </el-descriptions-item>
          <el-descriptions-item label="消费组">
            <span v-if="pipeline.group_id" class="mono">{{ pipeline.group_id }}</span>
            <span v-else class="muted">无数据</span>
          </el-descriptions-item>
          <el-descriptions-item label="Topic">
            <span v-if="pipeline.topic" class="mono">{{ pipeline.topic }}</span>
            <span v-else class="muted">无数据</span>
          </el-descriptions-item>
          <el-descriptions-item label="Brokers（平台内部）">
            <span v-if="brokersText" class="mono">{{ brokersText }}</span>
            <span v-else class="muted">无数据</span>
          </el-descriptions-item>
          <el-descriptions-item label="对外地址（被管机接入用）">
            <span v-if="pipeline.external_address" class="mono">{{ pipeline.external_address }}</span>
            <span v-else class="muted">无数据</span>
          </el-descriptions-item>
          <el-descriptions-item label="最近一条消息">
            <span v-if="pipeline.last_message_at">{{ formatTime(pipeline.last_message_at) }}</span>
            <span v-else class="muted">还没有收到消息</span>
          </el-descriptions-item>
        </el-descriptions>

        <p v-if="pipeline.last_error" class="field-hint text-danger">最近错误：{{ pipeline.last_error }}</p>
        <p v-if="pipeline.note && pipeline.enabled" class="field-hint">{{ pipeline.note }}</p>
      </template>

      <!-- 探测结果就地展示：失败原因与耗时都要能看见，不需要再去翻日志 -->
      <el-alert
        v-if="probeResult"
        :type="probeResult.ok ? 'success' : 'error'"
        :closable="true"
        show-icon
        class="mt"
        :title="probeResult.ok ? 'Kafka 连接正常' : 'Kafka 连接失败'"
        @close="probeResult = null"
      >
        <p class="field-hint">{{ probeResult.message }}（耗时 {{ probeResult.latency_ms }} ms）</p>
        <p v-if="probeResult.address || probeResult.topic" class="field-hint mono">
          探测目标：{{ probeResult.address || '-' }}<template v-if="probeResult.topic"> · topic {{ probeResult.topic }}</template>
        </p>
      </el-alert>

      <p class="field-hint">
        日志集成由目标机上的 Filebeat 推送到上面这个对外地址；不能装 Filebeat 的应用可以改用
        <span class="mono">POST /api/hooks/logs</span> 直推，两条路径最终汇入同一套日志事件链路。
      </p>
    </div>

    <ResponsiveList
      :items="items"
      :loading="loading"
      :error="error"
      :total="total"
      :page="query.page"
      :page-size="query.page_size"
      empty-text="暂无日志告警事件"
      @update:page="list.setPage"
      @update:page-size="list.setPageSize"
      @retry="list.load"
    >
      <template #filters>
        <el-input v-model="query.keyword" placeholder="搜索堆栈或指纹" clearable class="filter-item" @keyup.enter="list.search" />
        <el-input v-model="query.service" placeholder="服务名" clearable class="filter-item" @keyup.enter="list.search" />
        <el-select v-model="query.status" placeholder="全部状态" clearable class="filter-item">
          <el-option label="待处理" value="pending" />
          <el-option label="分析中" value="analyzing" />
          <el-option label="已解决" value="resolved" />
          <el-option label="已忽略" value="ignored" />
        </el-select>
        <el-select v-model="query.alert_type" placeholder="全部类型" clearable class="filter-item">
          <el-option label="异常报错" value="error" />
          <el-option label="异常堆栈" value="stack" />
          <el-option label="GC 日志" value="gc" />
        </el-select>
        <el-button type="primary" :icon="'Search'" @click="list.search">查询</el-button>
        <el-button :icon="'RefreshLeft'" @click="list.reset">重置</el-button>
      </template>

      <!-- 移动端：卡片 -->
      <template #card="{ row }">
        <div class="event-head">
          <span class="event-service">{{ row.service_name }}</span>
          <el-tag size="small" :type="row.severity === 'critical' ? 'danger' : 'warning'" effect="plain">
            {{ row.severity }}
          </el-tag>
          <el-tag size="small" :type="row.error_count > 10 ? 'danger' : row.error_count > 3 ? 'warning' : 'info'" effect="light">
            {{ row.error_count }} 次
          </el-tag>
        </div>
        <p class="event-stack">{{ row.raw_stacktrace || '（无堆栈）' }}</p>
        <div class="event-meta muted">
          <code>{{ (row.error_signature || '').slice(0, 8) }}</code>
          <span>{{ row.alert_type }}</span>
          <span>{{ formatTime(row.last_seen_at) }}</span>
        </div>
        <!-- 规则处理结果：命中规则 / 去重窗口 / 冷却与通知 / AI 分析状态 -->
        <div class="event-flags">
          <el-tag v-if="row.rule_id" size="small" effect="plain">命中规则 #{{ row.rule_id }}</el-tag>
          <!-- rule_id=0 是后端的明确语义（未命中任何规则，按默认值处理），不能被渲染成空白 -->
          <span v-else-if="row.rule_id === 0" class="muted">默认值处理</span>
          <el-tag v-if="dedupText(row.dedup_window)" size="small" effect="plain">{{ dedupText(row.dedup_window) }}</el-tag>
          <template v-if="row.suppressed">
            <el-tag size="small" type="warning" effect="light">冷却中</el-tag>
            <span v-if="row.cooldown_until" class="muted">至 {{ formatTime(row.cooldown_until, 'MM-DD HH:mm') }}</span>
          </template>
          <el-tag v-else-if="row.notified_at" size="small" type="success" effect="light">
            已通知 {{ formatTime(row.notified_at, 'MM-DD HH:mm') }}
          </el-tag>
          <el-tooltip :content="row.analysis_error" :disabled="!row.analysis_error" placement="top">
            <el-tag size="small" :type="analysisMeta(row.analysis_state).type" effect="light">
              {{ analysisMeta(row.analysis_state).label }}
            </el-tag>
          </el-tooltip>
        </div>
        <div class="event-actions">
          <el-button size="small" @click="openDetail(row)">详情</el-button>
        </div>
      </template>

      <!-- 桌面端：表格 -->
      <template #table>
        <div class="table-scroll">
        <el-table :data="items" size="default" @row-click="openDetail">
          <el-table-column prop="service_name" label="服务" min-width="140" show-overflow-tooltip />
          <el-table-column label="指纹" width="130">
            <template #default="{ row }">
              <code>{{ (row.error_signature || '').slice(0, 8) }}</code>
            </template>
          </el-table-column>
          <el-table-column prop="alert_type" label="类型" width="90" />
          <el-table-column label="次数" width="80">
            <template #default="{ row }">
              <el-tag size="small" :type="row.error_count > 10 ? 'danger' : row.error_count > 3 ? 'warning' : 'info'" effect="light">
                {{ row.error_count }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="严重度" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.severity === 'critical' ? 'danger' : 'warning'" effect="plain">
                {{ row.severity }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="raw_stacktrace" label="堆栈摘要" min-width="220" show-overflow-tooltip />
          <el-table-column label="规则处理" width="160">
            <template #default="{ row }">
              <div class="cell-flags">
                <el-tag v-if="row.rule_id" size="small" effect="plain">命中规则 #{{ row.rule_id }}</el-tag>
                <span v-else-if="row.rule_id === 0" class="muted">默认值处理</span>
                <span v-if="dedupText(row.dedup_window)" class="muted">{{ dedupText(row.dedup_window) }}</span>
              </div>
            </template>
          </el-table-column>
          <el-table-column label="抑制 / 通知" width="170">
            <template #default="{ row }">
              <div class="cell-flags">
                <template v-if="row.suppressed">
                  <el-tag size="small" type="warning" effect="light">冷却中</el-tag>
                  <span v-if="row.cooldown_until" class="muted">至 {{ formatTime(row.cooldown_until, 'MM-DD HH:mm') }}</span>
                </template>
                <el-tag v-else-if="row.notified_at" size="small" type="success" effect="light">
                  已通知 {{ formatTime(row.notified_at, 'MM-DD HH:mm') }}
                </el-tag>
                <span v-else class="muted">无数据</span>
              </div>
            </template>
          </el-table-column>
          <el-table-column label="AI 分析" width="110">
            <template #default="{ row }">
              <el-tooltip :content="row.analysis_error" :disabled="!row.analysis_error" placement="top">
                <el-tag size="small" :type="analysisMeta(row.analysis_state).type" effect="light">
                  {{ analysisMeta(row.analysis_state).label }}
                </el-tag>
              </el-tooltip>
            </template>
          </el-table-column>
          <el-table-column label="最近出现" width="170">
            <template #default="{ row }">{{ formatTime(row.last_seen_at) }}</template>
          </el-table-column>
          <el-table-column label="状态" width="100">
            <template #default="{ row }">
              <el-tag size="small" effect="light" :type="row.status === 'resolved' ? 'success' : row.status === 'ignored' ? 'info' : 'warning'">
                {{ row.status }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="已分析" width="90">
            <template #default="{ row }">
              <el-icon :class="row.analyzed ? 'text-success' : 'muted'"><CircleCheck v-if="row.analyzed" /><Clock v-else /></el-icon>
            </template>
          </el-table-column>
        </el-table>
        </div>
      </template>
    </ResponsiveList>

    <el-drawer v-model="detailVisible" title="日志事件详情" size="720px" direction="rtl">
      <div v-if="current" class="stack">
        <div class="row">
          <el-tag size="small" effect="plain">{{ current.service_name }}</el-tag>
          <el-tag size="small" effect="plain">{{ current.alert_type }}</el-tag>
          <el-tag size="small" type="warning" effect="light">出现 {{ current.error_count }} 次</el-tag>
          <el-tag size="small" effect="light">{{ current.status }}</el-tag>
          <el-tag v-if="current.rule_id" size="small" effect="plain">命中规则 #{{ current.rule_id }}</el-tag>
          <span v-else-if="current.rule_id === 0" class="muted">默认值处理</span>
          <el-tag v-if="dedupText(current.dedup_window)" size="small" effect="plain">{{ dedupText(current.dedup_window) }}</el-tag>
          <template v-if="current.suppressed">
            <el-tag size="small" type="warning" effect="light">冷却中</el-tag>
            <span v-if="current.cooldown_until" class="muted">至 {{ formatTime(current.cooldown_until, 'MM-DD HH:mm') }}</span>
          </template>
          <el-tag v-else-if="current.notified_at" size="small" type="success" effect="light">
            已通知 {{ formatTime(current.notified_at, 'MM-DD HH:mm') }}
          </el-tag>
          <el-tooltip :content="current.analysis_error" :disabled="!current.analysis_error" placement="top">
            <el-tag size="small" :type="analysisMeta(current.analysis_state).type" effect="light">
              {{ analysisMeta(current.analysis_state).label }}
            </el-tag>
          </el-tooltip>
        </div>

        <!-- 错误消息原文：指纹是哈希，排查时真正要看的是这一句 -->
        <div v-if="current.error_message" class="field">
          <span class="field-label">错误消息</span>
          <span>{{ current.error_message }}</span>
        </div>
        <div class="field">
          <span class="field-label">错误指纹</span>
          <code>{{ current.error_signature }}</code>
        </div>
        <div class="field">
          <span class="field-label">时间范围</span>
          <span>{{ formatTime(current.first_seen_at) }} → {{ formatTime(current.last_seen_at) }}</span>
        </div>
        <!-- 哪个文件在报错往往比"报了什么"更快定位到服务与模块（Filebeat 采集的 log.file.path） -->
        <div v-if="current.log_path" class="field">
          <span class="field-label">日志文件</span>
          <code>{{ current.log_path }}</code>
        </div>
        <!-- 失败/跳过原因：直接写明，避免"分析中"永远转圈却看不到原因。
             只有 failed 才是故障（红），disabled 是明确的配置结果（灰）。 -->
        <div v-if="current.analysis_error" class="field">
          <span class="field-label">分析状态说明</span>
          <span :class="current.analysis_state === 'failed' ? 'text-danger' : 'muted'">{{ current.analysis_error }}</span>
        </div>

        <h4 class="section">堆栈原文</h4>
        <pre class="code-block">{{ current.raw_stacktrace || '（无堆栈）' }}</pre>

        <div v-if="current.context_lines" class="field">
          <span class="field-label">上下文日志</span>
          <pre class="code-block">{{ current.context_lines }}</pre>
        </div>

        <div class="row actions">
          <el-button type="primary" :loading="analyzing" :icon="'MagicStick'" @click="analyze">AI 代码分析</el-button>
          <!-- 重新分析：分析中禁用，避免重复触发同一条事件的分析（后端也会拦，但先在前端挡住更直观） -->
          <el-button
            v-if="canWrite"
            :loading="reanalyzing"
            :disabled="current.analysis_state === 'running' || current.analysis_state === 'awaiting'"
            :icon="'RefreshRight'"
            @click="reanalyze"
          >
            重新分析
          </el-button>
          <el-button v-if="canWrite" @click="updateStatus('resolved')">标记已解决</el-button>
          <el-button v-if="canWrite" @click="updateStatus('ignored')">忽略</el-button>
        </div>

        <!-- 三点式代码分析结果 -->
        <template v-if="analysis || analyzeResult">
          <h4 class="section">代码分析报告（三点式）</h4>
          <div class="analysis-card">
            <div class="analysis-row">
              <span class="field-label">问题位置</span>
              <span class="mono">
                {{ analysis?.located_file || (analyzeResult?.located_file as string) || '未能定位' }}
                <template v-if="analysis?.located_line || analyzeResult?.located_line">:{{ analysis?.located_line || analyzeResult?.located_line }}</template>
              </span>
            </div>
            <div class="analysis-row">
              <span class="field-label">根因分析</span>
              <span>{{ analysis?.root_cause || analyzeResult?.root_cause || '-' }}</span>
            </div>
            <div class="analysis-row">
              <span class="field-label">应急方案</span>
              <span>{{ analysis?.emergency_plan || analyzeResult?.emergency_plan || '-' }}</span>
            </div>
            <div class="analysis-row">
              <span class="field-label">修复建议</span>
              <span>{{ analysis?.fix_suggestion || analyzeResult?.fix_suggestion || '-' }}</span>
            </div>
            <div class="analysis-row">
              <span class="field-label">影响范围</span>
              <span>{{ analysis?.impact_scope || analyzeResult?.impact_scope || '-' }}</span>
            </div>
            <div class="analysis-row">
              <span class="field-label">出网合规</span>
              <el-tag size="small" :type="analysis?.outbound_ok ? 'success' : 'info'" effect="light">
                {{ analysis?.outbound_ok ? '已通过白名单 + 脱敏' : '本地分析（未出网）' }}
              </el-tag>
            </div>
            <!-- 代码版本：行号会随代码演进失效，复核结论时必须知道当时看的是哪一版。
                 没有它，"定位错了"与"代码后来改了"无法区分。 -->
            <div v-if="analysis?.repo_revision" class="analysis-row">
              <span class="field-label">代码版本</span>
              <span class="mono">{{ analysis.repo_revision }}</span>
            </div>
            <!-- 完整报告：补丁全文与验证结果在外部 AI 服务那边，平台上只有三点式摘要。 -->
            <div v-if="analysis?.report_url" class="analysis-row">
              <span class="field-label">完整报告</span>
              <el-link :href="analysis.report_url" target="_blank" rel="noopener" type="primary">
                在 AI 服务查看
              </el-link>
            </div>
          </div>
          <pre v-if="analysis?.code_snippet" class="code-block">{{ analysis.code_snippet }}</pre>
        </template>
      </div>
    </el-drawer>
  </div>
</template>

<style scoped>
.filter-item {
  width: 180px;
}

.toolbar-row {
  margin-bottom: 12px;
}

/* Kafka 采集链路卡片与下方事件列表的分隔（.card + .card 的间距由全局样式给） */
.pipeline-card {
  margin-bottom: 12px;
}

.mb {
  margin-bottom: 12px;
}

.mt {
  margin-top: 12px;
}

.field-hint {
  margin: 2px 0 0;
  font-size: 11.5px;
  line-height: 1.6;
}

/* 移动端卡片：日志事件 */
.event-head {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.event-service {
  font-size: 13.5px;
  font-weight: 600;
}

.event-stack {
  margin: 8px 0 0;
  font-size: 12.5px;
  line-height: 1.6;
  overflow: hidden;
  display: -webkit-box;
  -webkit-line-clamp: 3;
  -webkit-box-orient: vertical;
}

.event-meta {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
  margin-top: 6px;
  font-size: 11.5px;
}

/* 规则处理结果标签（命中规则 / 窗口 / 冷却 / 通知 / AI 状态） */
.event-flags {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  margin-top: 8px;
  font-size: 11.5px;
}

/* 表格单元格内的多标签：窄列里换行比撑宽列更可读 */
.cell-flags {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-wrap: wrap;
  font-size: 11.5px;
}

.event-actions {
  margin-top: 8px;
}

.stack {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 12.5px;
}

.field-label {
  font-size: 11.5px;
  color: var(--c-text-3);
}

.section {
  margin: 8px 0 0;
  font-size: 13px;
  font-weight: 600;
}

.code-block {
  margin: 0;
  padding: 10px 12px;
  background: var(--c-surface-2);
  border-radius: var(--r-md);
  font-size: 12px;
  line-height: 1.6;
  max-height: 280px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
}

.actions {
  margin-top: 4px;
}

.analysis-card {
  display: flex;
  flex-direction: column;
  gap: 10px;
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  padding: 12px;
}

.analysis-row {
  display: flex;
  gap: 10px;
  font-size: 12.5px;
}

.analysis-row .field-label {
  flex: 0 0 64px;
  padding-top: 1px;
}

</style>
