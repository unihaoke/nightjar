/** 接口封装：与设计文档 8.2 接口总览一一对应。 */
import { authHeaders, get, post, postSlow, put, del, withSignal, type PageResult } from './http'
import type {
  AISettingsInput,
  AISettingsView,
  AIProviderTestInput,
  AITestResult,
  AIUsageView,
  Alert,
  AlertRule,
  Approval,
  AuditLog,
  AuditSnapshot,
  CodeAnalysis,
  CodeRepo,
  DiagnoseResult,
  DiagnosisRecord,
  DiagnosisResponse,
  FixExecuteResult,
  FixPreview,
  FixRecord,
  IntegrationAccount,
  AccountProbeResult,
  AccountRetryResult,
  AccountRotateResult,
  AccountSecurePayload,
  IntegrationArtifacts,
  IntegrationInput,
  IntegrationOverview,
  IntegrationView,
  IntegrationSelfCheck,
  KnowledgeEntry,
  LogAlertExclusion,
  LogAlertExclusionInput,
  LogAlertRule,
  LogAlertRuleInput,
  LogEvent,
  LogPipelineProbeResult,
  LogPipelineStatus,
  Metric,
  MetricSample,
  MetricSnapshot,
  MiddlewareInput,
  MiddlewareInstance,
  NotifyChannel,
  NotifySettingsInput,
  NotifySettingsView,
  NotifyTestResult,
  Overview,
  Profile,
  ReanalyzeResult,
  Role,
  ServerInstance,
  Session,
  SystemInfo,
  TestResult,
  User,
} from './types'

/** 分页查询参数。 */
export interface PageQuery {
  page?: number
  page_size?: number
  [key: string]: unknown
}

/** 登录。 */
export const authApi = {
  login: (username: string, password: string) => post<Session>('/api/auth/login', { username, password }),
  logout: () => post<{ message: string }>('/api/auth/logout'),
  profile: () => get<Profile>('/api/auth/profile'),
  changePassword: (oldPassword: string, newPassword: string) =>
    post<{ message: string }>('/api/auth/password', { old_password: oldPassword, new_password: newPassword }),
}

/** 中间件纳管（4.1）。 */
export const middlewareApi = {
  list: (params: PageQuery, signal?: AbortSignal) =>
    get<PageResult<MiddlewareInstance>>('/api/middlewares', params, withSignal(signal)),
  detail: (id: number) =>
    get<{
      instance: MiddlewareInstance
      rules: AlertRule[]
      rule_total: number
      alerts: Alert[]
      alert_total: number
      metrics_catalog: { mw_type: string; metrics: Metric[] }
    }>(`/api/middlewares/${id}`),
  options: () =>
    get<{
      types: { value: string; label: string; port: number; phase: number }[]
      groups: string[]
      environments: string[]
      status_options: { value: number; label: string }[]
      /** Prometheus 中实际存在的 job 名（查不到时为空数组）。 */
      prom_jobs?: string[]
      /** Prometheus 中 instance_name 的实际取值（= 集成名称），用于把实例名对齐标签。 */
      prom_instance_names?: string[]
    }>('/api/middlewares/options'),
  create: (payload: MiddlewareInput) => post<MiddlewareInstance>('/api/middlewares', payload),
  update: (id: number, payload: MiddlewareInput) => put<MiddlewareInstance>(`/api/middlewares/${id}`, payload),
  remove: (id: number, reason?: string) =>
    del<{ message?: string; status?: string; ticket_id?: string }>(`/api/middlewares/${id}`, reason ? { reason } : undefined),
  test: (id: number) => post<TestResult>(`/api/middlewares/${id}/test`),
  testEndpoint: (payload: MiddlewareInput) => post<TestResult>('/api/middlewares/test', payload),
  health: (id: number) => post<TestResult>(`/api/middlewares/${id}/health`),
}

/** 统一监控（4.2）。 */
export const metricsApi = {
  snapshot: (id: number) => get<MetricSnapshot>(`/api/metrics/${id}`),
  history: (id: number, metric: string, params?: Record<string, unknown>) =>
    get<{ metric: string; series: MetricSample[]; source: string }>(`/api/metrics/${id}/history`, { metric, ...params }),
  compare: (instanceIds: number[], metric: string) =>
    get<{ metric: string; items: { instance_id: number; instance_name: string; value: number; unit: string }[]; source: string }>(
      '/api/metrics/compare',
      { instance_ids: instanceIds.join(','), metric },
    ),
  catalog: (mwType?: string) =>
    get<{
      mw_type: string
      types: string[]
      metrics: { name: string; display_name: string; unit: string; category: string }[]
    }>('/api/metrics/catalog', mwType ? { mw_type: mwType } : undefined),
  /** 接入自检：纳管后看不到监控时先看这里。 */
  diagnose: (id: number) => get<DiagnoseResult>(`/api/metrics/${id}/diagnose`),
}

/** 集成中心：页面一键集成（对齐云厂商控制台的「数据采集 → 集成中心」）。 */
export const integrationApi = {
  overview: () => get<IntegrationOverview>('/api/integrations/overview'),
  list: () => get<{ items: IntegrationView[] }>('/api/integrations'),
  detail: (id: number) => get<IntegrationView>(`/api/integrations/${id}`),
  preview: (payload: IntegrationInput) => post<IntegrationArtifacts>('/api/integrations/preview', payload),
  create: (payload: IntegrationInput) => post<IntegrationView>('/api/integrations', payload),
  update: (id: number, payload: IntegrationInput) => put<IntegrationView>(`/api/integrations/${id}`, payload),
  // 重新应用：远程部署需要 SSH 凭据（凭据仅本次使用，平台不落库）。
  apply: (id: number, payload: AccountSecurePayload = {}) =>
    post<IntegrationView>(`/api/integrations/${id}/apply`, payload),
  // 重新核验：只按 Prometheus 现状刷新状态，不重装、不需要凭据。
  verify: (id: number) => post<IntegrationView>(`/api/integrations/${id}/verify`),
  // 端到端自检：分环节给出"哪一环断了 + 下一步做什么"。
  selfCheck: (id: number) => post<IntegrationSelfCheck>(`/api/integrations/${id}/selfcheck`),
  /** 监控账号管理：查看平台代管的只读账号；轮换口令（账号改自己口令，无需管理员凭据）。 */
  accounts: () => get<{ items: IntegrationAccount[] }>('/api/integrations/accounts'),
  /**
   * 远程集成的账号操作在**目标主机**上执行（只经 SSH + Ansible），
   * 因此这些接口都可带一份可选的 SSH 凭据（仅本次请求使用，平台不落库）。
   */
  rotateAccount: (id: number, payload: AccountSecurePayload = {}) =>
    postSlow<AccountRotateResult>(`/api/integrations/${id}/account/rotate`, payload),
  /** 重试建号/连接：带管理凭据=幂等重建账号；不带=只测连接并重建 Exporter。 */
  retryAccount: (id: number, payload: AccountSecurePayload & { admin_username?: string; admin_password?: string }) =>
    postSlow<AccountRetryResult>(`/api/integrations/${id}/account/retry`, payload),
  /** 只做连接测试（不建号、不改配置）。 */
  probeAccount: (id: number, payload: AccountSecurePayload = {}) =>
    postSlow<AccountProbeResult>(`/api/integrations/${id}/account/probe`, payload),
  /** 删除监控账号（L2：需被管实例的管理员凭据；远程集成还需 SSH 凭据）。 */
  dropAccount: (id: number, payload: AccountSecurePayload & { admin_username: string; admin_password: string }) =>
    postSlow<IntegrationView>(`/api/integrations/${id}/account/drop`, payload),
  remove: (id: number) => del<{ message: string }>(`/api/integrations/${id}`),
}

/** AI 诊断（4.3）。 */
export const aiApi = {
  diagnoseSync: (payload: { instance_id?: number; question: string; mw_type?: string; alert_id?: number; skip_cache?: boolean }) =>
    post<DiagnosisResponse>('/api/ai/diagnose/sync', payload),
  history: (params: PageQuery, signal?: AbortSignal) =>
    get<PageResult<DiagnosisRecord>>('/api/ai/diagnosis-history', params, withSignal(signal)),
  detail: (id: number) => get<{ diagnosis: DiagnosisRecord; knowledge_entries: KnowledgeEntry[] }>(`/api/ai/diagnosis/${id}`),
  feedback: (id: number, feedback: 'useful' | 'useless' | 'adopted') =>
    post<{ message: string }>(`/api/ai/diagnosis/${id}/feedback`, { diagnosis_id: id, feedback }),
  quality: (params?: Record<string, unknown>) =>
    get<{
      quality: Record<string, unknown>
      guardrail: Record<string, unknown>
      cost: Record<string, unknown>
      engine: { name: string; available: boolean; degraded?: boolean }
      tools: string[]
      eval_set_size: number
    }>('/api/ai/quality', params),
  codeAnalyze: (payload: { event_id?: number; service?: string; stacktrace?: string; message?: string; force_local?: boolean }) =>
    post<{ analysis_id: number; report: Record<string, unknown>; warnings: string[]; outbound_ok: boolean }>(
      '/api/ai/code-analyze',
      payload,
    ),
  codeAnalyses: (params: PageQuery, signal?: AbortSignal) =>
    get<PageResult<CodeAnalysis>>('/api/ai/code-analyses', params, withSignal(signal)),
}

/** SSE 流式诊断。 */
export interface DiagnoseStreamHandlers {
  onMeta: (meta: DiagnosisResponse['meta']) => void
  onDelta: (delta: string) => void
  onDone: (result: DiagnosisResponse) => void
  onError: (message: string) => void
}

/**
 * 发起 SSE 流式诊断。
 *
 * 使用 fetch + ReadableStream 而非 EventSource：后者无法携带 Authorization 头。
 * 返回中止函数，便于用户取消。
 */
export function diagnoseStream(
  payload: { instance_id?: number; question: string; mw_type?: string; alert_id?: number; skip_cache?: boolean },
  handlers: DiagnoseStreamHandlers,
): () => void {
  const controller = new AbortController()
  const base = import.meta.env.VITE_API_BASE || ''

  void (async () => {
    try {
      const response = await fetch(`${base}/api/ai/diagnose`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...authHeaders() },
        body: JSON.stringify(payload),
        credentials: 'include',
        signal: controller.signal,
      })
      if (!response.ok || !response.body) {
        handlers.onError(`诊断请求失败（HTTP ${response.status}）`)
        return
      }
      const reader = response.body.getReader()
      const decoder = new TextDecoder('utf-8')
      let buffer = ''
      for (;;) {
        const { value, done } = await reader.read()
        if (done) {
          break
        }
        buffer += decoder.decode(value, { stream: true })
        // SSE 事件以空行分隔。
        let sep = buffer.indexOf('\n\n')
        while (sep >= 0) {
          const raw = buffer.slice(0, sep)
          buffer = buffer.slice(sep + 2)
          dispatchEvent(raw, handlers)
          sep = buffer.indexOf('\n\n')
        }
      }
    } catch (error) {
      if ((error as Error).name === 'AbortError') {
        handlers.onError('已取消诊断')
        return
      }
      handlers.onError(error instanceof Error ? error.message : '流式诊断异常')
    }
  })()

  return () => controller.abort()
}

/** 解析单个 SSE 事件块。 */
function dispatchEvent(raw: string, handlers: DiagnoseStreamHandlers): void {
  let event = 'message'
  const dataLines: string[] = []
  for (const line of raw.split('\n')) {
    if (line.startsWith('event:')) {
      event = line.slice(6).trim()
    } else if (line.startsWith('data:')) {
      dataLines.push(line.slice(5).trim())
    }
  }
  if (dataLines.length === 0) {
    return
  }
  const payload = dataLines.join('\n')
  try {
    const parsed = JSON.parse(payload)
    switch (event) {
      case 'meta':
        handlers.onMeta(parsed)
        break
      case 'data':
        handlers.onDelta(parsed.delta ?? '')
        break
      case 'done':
        handlers.onDone(parsed)
        break
      case 'error':
        handlers.onError(parsed.message || '诊断失败')
        break
      default:
        break
    }
  } catch {
    // 忽略无法解析的心跳数据。
  }
}

/** 告警治理（4.4）。 */
export const alertApi = {
  list: (params: PageQuery, signal?: AbortSignal) => get<PageResult<Alert>>('/api/alerts', params, withSignal(signal)),
  options: () =>
    get<{
      instances: { id: number; name: string; mw_type: string; environment: string }[]
      operators: string[]
      levels: string[]
      channels: string[]
      notify_status: NotifyChannel[]
    }>('/api/alerts/options'),
  rules: (params: PageQuery, signal?: AbortSignal) =>
    get<PageResult<AlertRule>>('/api/alerts/rules', params, withSignal(signal)),
  createRule: (payload: Partial<AlertRule>) => post<AlertRule>('/api/alerts/rules', payload),
  updateRule: (id: number, payload: Partial<AlertRule>) => put<AlertRule>(`/api/alerts/rules/${id}`, payload),
  removeRule: (id: number) => del<{ message: string }>(`/api/alerts/rules/${id}`),
  ack: (id: number) => post<{ message: string }>(`/api/alerts/${id}/ack`),
  resolve: (id: number) => post<{ message: string }>(`/api/alerts/${id}/resolve`),
  evaluate: () =>
    post<{ evaluated: number; triggered: number; merged: number; suppressed: number; skipped: number }>('/api/alerts/evaluate'),
  cluster: () =>
    post<{ processed: number; clusters: { cluster_id: string; label: string; count: number; alert_ids: number[] }[] }>(
      '/api/alerts/cluster',
      undefined,
    ).then((result) => result),
  notifyTest: (channel: string) => post<{ message: string }>(`/api/alerts/notify-test?channel=${channel}`),
}

/** 知识库（4.5）。 */
export const knowledgeApi = {
  list: (params: PageQuery, signal?: AbortSignal) =>
    get<PageResult<KnowledgeEntry>>('/api/knowledge', params, withSignal(signal)),
  detail: (id: number) => get<KnowledgeEntry>(`/api/knowledge/${id}`),
  create: (payload: { title: string; content: string; mw_type?: string; tags?: string[]; status?: string }) =>
    post<KnowledgeEntry>('/api/knowledge', payload),
  update: (id: number, payload: { title?: string; content?: string; mw_type?: string; tags?: string[]; status?: string }) =>
    put<KnowledgeEntry>(`/api/knowledge/${id}`, payload),
  adopt: (id: number) => post<{ message: string }>(`/api/knowledge/${id}/adopt`),
  remove: (id: number) => del<{ message: string }>(`/api/knowledge/${id}`),
  stats: () => get<{ total: number; by_status: Record<string, number>; adoption_rate: number }>('/api/knowledge/stats'),
  options: () => get<{ statuses: { value: string; label: string }[]; sources: { value: string; label: string }[] }>(
    '/api/knowledge/options',
  ),
}

/** 修复执行（4.6）。 */
export const fixApi = {
  options: () =>
    get<{
      actions: { action: string; label: string; level: string; desc: string }[]
      levels: { level: string; label: string; desc: string }[]
      sql_guard: { default_limit: number; max_limit: number; allowlist: string[] }
      executor: string
    }>('/api/fix/options'),
  preview: (payload: { instance_id: number; action_type: string; params?: Record<string, unknown>; reason?: string }) =>
    post<FixPreview>('/api/fix/preview', payload),
  execute: (payload: {
    instance_id: number
    action_type: string
    params?: Record<string, unknown>
    reason?: string
    dry_run?: boolean
    ticket_id?: string
    /** 来源上下文（告警 / 诊断），随审批单与修复记录落库，用于回填来源告警）。 */
    alert_id?: number
    diagnosis_id?: number
  }) => post<FixExecuteResult>('/api/fix/execute', payload),
  history: (params: PageQuery, signal?: AbortSignal) =>
    get<PageResult<FixRecord>>('/api/fix/history', params, withSignal(signal)),
  validateSql: (sql: string) =>
    post<{ normalized_sql: string; notes: string[]; high_risk: boolean; high_risk_reason: string }>('/api/fix/validate-sql', { sql }),
}

/** 审批（6.2）。 */
export const approvalApi = {
  list: (params: PageQuery, signal?: AbortSignal) =>
    get<PageResult<Approval>>('/api/approvals', params, withSignal(signal)),
  detail: (id: number) => get<Approval>(`/api/approvals/${id}`),
  decide: (id: number, approved: boolean, comment: string) =>
    post<Approval>(`/api/approvals/${id}/decide`, { approved, comment }),
}

/** 审计（4.7）。 */
export const auditApi = {
  logs: (params: PageQuery, signal?: AbortSignal) =>
    get<PageResult<AuditLog>>('/api/audit/logs', params, withSignal(signal)),
  detail: (id: number) => get<AuditLog>(`/api/audit/logs/${id}`),
  verify: (params?: Record<string, unknown>) =>
    get<{ verified: boolean; broken_id: number; message: string }>('/api/audit/verify', params),
  snapshot: () =>
    post<AuditSnapshot>('/api/audit/snapshot').then((result) => result),
  snapshots: () => get<{ list: AuditSnapshot[] }>('/api/audit/snapshots'),
}

/** 日志告警与代码分析（4.8）。 */
export const logAlertApi = {
  events: (params: PageQuery, signal?: AbortSignal) =>
    get<PageResult<LogEvent>>('/api/log-alerts/events', params, withSignal(signal)),
  event: (id: number) => get<{ event: LogEvent; analysis: CodeAnalysis | null }>(`/api/log-alerts/events/${id}`),
  updateStatus: (id: number, status: string) => put<{ message: string }>(`/api/log-alerts/events/${id}/status`, { status }),
  servers: (params: PageQuery, signal?: AbortSignal) =>
    get<PageResult<ServerInstance>>('/api/log-alerts/servers', params, withSignal(signal)),
  createServer: (payload: Partial<ServerInstance>) => post<ServerInstance>('/api/log-alerts/servers', payload),
  updateServer: (id: number, payload: Partial<ServerInstance>) => put<ServerInstance>(`/api/log-alerts/servers/${id}`, payload),
  removeServer: (id: number) => del<{ message: string }>(`/api/log-alerts/servers/${id}`),
  codeRepos: (params: PageQuery, signal?: AbortSignal) =>
    get<PageResult<CodeRepo>>('/api/log-alerts/code-repos', params, withSignal(signal)),
  saveCodeRepo: (id: number | undefined, payload: Partial<CodeRepo>) =>
    post<CodeRepo>(`/api/log-alerts/code-repos${id ? `?id=${id}` : ''}`, payload),
  /** 平台自带 Kafka 的采集链路现状（Filebeat 推送到平台 Kafka 后由消费者入库）。 */
  pipeline: () => get<LogPipelineStatus>('/api/log-alerts/pipeline'),
  /** 测试 Kafka 连接：失败也返回 ok=false 与原因，不抛错。 */
  probePipeline: () => post<LogPipelineProbeResult>('/api/log-alerts/pipeline/probe'),
  /**
   * 日志告警规则（4.8.2）：按服务/指纹/级别匹配，决定去重窗口、冷却期、通知渠道与是否 AI 分析。
   *
   * 与指标告警规则（alertApi.rules）刻意同名同语义，使用者在两个页面看到的是一套心智模型。
   */
  rules: (params: PageQuery, signal?: AbortSignal) =>
    get<PageResult<LogAlertRule>>('/api/log-alerts/rules', params, withSignal(signal)),
  createRule: (payload: LogAlertRuleInput) => post<LogAlertRule>('/api/log-alerts/rules', payload),
  updateRule: (id: number, payload: LogAlertRuleInput) => put<LogAlertRule>(`/api/log-alerts/rules/${id}`, payload),
  removeRule: (id: number) => del<{ message: string }>(`/api/log-alerts/rules/${id}`),
  /**
   * 日志告警屏蔽项：命中即丢弃（不入库、不通知、不分析），优先于所有规则。
   * 典型用法是屏蔽框架噪音，如 "Request method 'GET' is not supported"。
   */
  exclusions: (params: PageQuery, signal?: AbortSignal) =>
    get<PageResult<LogAlertExclusion>>('/api/log-alerts/exclusions', params, withSignal(signal)),
  createExclusion: (payload: LogAlertExclusionInput) => post<LogAlertExclusion>('/api/log-alerts/exclusions', payload),
  updateExclusion: (id: number, payload: Partial<LogAlertExclusionInput>) =>
    put<LogAlertExclusion>(`/api/log-alerts/exclusions/${id}`, payload),
  removeExclusion: (id: number) => del<{ message: string }>(`/api/log-alerts/exclusions/${id}`),
  /** 重新分析：对同一条事件重新入队 AI 代码分析（失败原因由后端 message 原样返回）。 */
  reanalyze: (eventId: number) => post<ReanalyzeResult>(`/api/log-alerts/events/${eventId}/reanalyze`),
}

/** 系统与大盘（8.2）。 */
export const systemApi = {
  overview: () => get<Overview>('/api/system/overview'),
  info: () => get<SystemInfo>('/api/system/info'),
  config: () => get<Record<string, unknown>>('/api/system/config'),
}

/** AI 设置与通知渠道设置（平台侧可写配置，需 system:config / system:config:write）。 */
export const settingApi = {
  /** 读取 AI Key 设置（策略、双提供方、额度）。 */
  ai: () => get<AISettingsView>('/api/settings/ai'),
  /** 保存 AI 设置：密钥留空表示不修改，clear_api_key=true 表示清空。 */
  saveAI: (payload: AISettingsInput) => put<AISettingsView>('/api/settings/ai', payload),
  /** token 消费与额度（含趋势、来源分布、Top 用户）。 */
  aiUsage: (days = 30) => get<AIUsageView>('/api/settings/ai/usage', { days }),
  /** 测试 AI 连接：失败也返回 ok=false 与原因，不抛错。 */
  testAI: () => post<AITestResult>('/api/settings/ai/test'),
  /** 按提供方自测连接（不保存）：用当前合并配置验证调用是否正确。 */
  testAIProvider: (payload: AIProviderTestInput) => post<AITestResult>('/api/settings/ai/test-provider', payload),
  /** 读取通知渠道设置。 */
  notify: () => get<NotifySettingsView>('/api/settings/notify'),
  /** 保存通知渠道设置：密钥/口令留空表示不修改。 */
  saveNotify: (payload: NotifySettingsInput) => put<NotifySettingsView>('/api/settings/notify', payload),
  /** 给单个渠道发一条测试消息。 */
  testNotify: (channel: string) => post<NotifyTestResult>('/api/settings/notify/test', { channel }),
}

/** 用户与角色（管理员）。 */
export const userApi = {
  list: (params: PageQuery, signal?: AbortSignal) => get<PageResult<User>>('/api/users', params, withSignal(signal)),
  create: (payload: Partial<User> & { password: string }) => post<User>('/api/users', payload),
  update: (id: number, payload: Partial<User> & { password?: string }) => put<User>(`/api/users/${id}`, payload),
  remove: (id: number) => del<{ message: string }>(`/api/users/${id}`),
  roles: () =>
    get<{ list: Role[]; permissions: { group: string; items: { code: string; name: string }[] }[] }>('/api/roles'),
  updateRole: (id: number, payload: Partial<Role>) => put<{ message: string }>(`/api/roles/${id}`, payload),
}
