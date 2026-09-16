/** 接口封装：与设计文档 8.2 接口总览一一对应。 */
import { authHeaders, get, post, put, del, type PageResult } from './http'
import type {
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
  IntegrationArtifacts,
  IntegrationInput,
  IntegrationOverview,
  IntegrationView,
  KnowledgeEntry,
  LogEvent,
  Metric,
  MetricSample,
  MetricSnapshot,
  MiddlewareInput,
  MiddlewareInstance,
  NotifyChannel,
  Overview,
  Profile,
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
  list: (params: PageQuery) => get<PageResult<MiddlewareInstance>>('/api/middlewares', params),
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
  apply: (id: number) => post<IntegrationView>(`/api/integrations/${id}/apply`),
  remove: (id: number) => del<{ message: string }>(`/api/integrations/${id}`),
}

/** AI 诊断（4.3）。 */
export const aiApi = {
  diagnoseSync: (payload: { instance_id?: number; question: string; mw_type?: string; alert_id?: number; skip_cache?: boolean }) =>
    post<DiagnosisResponse>('/api/ai/diagnose/sync', payload),
  history: (params: PageQuery) => get<PageResult<DiagnosisRecord>>('/api/ai/diagnosis-history', params),
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
  codeAnalyses: (params: PageQuery) => get<PageResult<CodeAnalysis>>('/api/ai/code-analyses', params),
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
  list: (params: PageQuery) => get<PageResult<Alert>>('/api/alerts', params),
  options: () =>
    get<{
      instances: { id: number; name: string; mw_type: string; environment: string }[]
      operators: string[]
      levels: string[]
      channels: string[]
      notify_status: NotifyChannel[]
    }>('/api/alerts/options'),
  rules: (params: PageQuery) => get<PageResult<AlertRule>>('/api/alerts/rules', params),
  createRule: (payload: Partial<AlertRule>) => post<AlertRule>('/api/alerts/rules', payload),
  updateRule: (id: number, payload: Partial<AlertRule>) => put<AlertRule>(`/api/alerts/rules/${id}`, payload),
  removeRule: (id: number) => del<{ message: string }>(`/api/alerts/rules/${id}`),
  ack: (id: number) => post<{ message: string }>(`/api/alerts/${id}/ack`),
  resolve: (id: number) => post<{ message: string }>(`/api/alerts/${id}/resolve`),
  evaluate: () =>
    post<{ evaluated: number; triggered: number; merged: number; suppressed: number; skipped: number }>('/api/alerts/evaluate'),
  cluster: (threshold?: number) =>
    post<{ processed: number; clusters: { cluster_id: string; label: string; count: number; alert_ids: number[] }[] }>(
      '/api/alerts/cluster',
      undefined,
    ).then((result) => result),
  notifyTest: (channel: string) => post<{ message: string }>(`/api/alerts/notify-test?channel=${channel}`),
}

/** 知识库（4.5）。 */
export const knowledgeApi = {
  list: (params: PageQuery) => get<PageResult<KnowledgeEntry>>('/api/knowledge', params),
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
  }) => post<FixExecuteResult>('/api/fix/execute', payload),
  history: (params: PageQuery) => get<PageResult<FixRecord>>('/api/fix/history', params),
  validateSql: (sql: string) =>
    post<{ normalized_sql: string; notes: string[]; high_risk: boolean; high_risk_reason: string }>('/api/fix/validate-sql', { sql }),
}

/** 审批（6.2）。 */
export const approvalApi = {
  list: (params: PageQuery) => get<PageResult<Approval>>('/api/approvals', params),
  detail: (id: number) => get<Approval>(`/api/approvals/${id}`),
  decide: (id: number, approved: boolean, comment: string) =>
    post<Approval>(`/api/approvals/${id}/decide`, { approved, comment }),
}

/** 审计（4.7）。 */
export const auditApi = {
  logs: (params: PageQuery) => get<PageResult<AuditLog>>('/api/audit/logs', params),
  detail: (id: number) => get<AuditLog>(`/api/audit/logs/${id}`),
  verify: (params?: Record<string, unknown>) =>
    get<{ verified: boolean; broken_id: number; message: string }>('/api/audit/verify', params),
  snapshot: () =>
    post<AuditSnapshot>('/api/audit/snapshot').then((result) => result),
  snapshots: () => get<{ list: AuditSnapshot[] }>('/api/audit/snapshots'),
}

/** 日志告警与代码分析（4.8）。 */
export const logAlertApi = {
  events: (params: PageQuery) => get<PageResult<LogEvent>>('/api/log-alerts/events', params),
  event: (id: number) => get<{ event: LogEvent; analysis: CodeAnalysis | null }>(`/api/log-alerts/events/${id}`),
  updateStatus: (id: number, status: string) => put<{ message: string }>(`/api/log-alerts/events/${id}/status`, { status }),
  servers: (params: PageQuery) => get<PageResult<ServerInstance>>('/api/log-alerts/servers', params),
  createServer: (payload: Partial<ServerInstance>) => post<ServerInstance>('/api/log-alerts/servers', payload),
  updateServer: (id: number, payload: Partial<ServerInstance>) => put<ServerInstance>(`/api/log-alerts/servers/${id}`, payload),
  removeServer: (id: number) => del<{ message: string }>(`/api/log-alerts/servers/${id}`),
  codeRepos: (params: PageQuery) => get<PageResult<CodeRepo>>('/api/log-alerts/code-repos', params),
  saveCodeRepo: (id: number | undefined, payload: Partial<CodeRepo>) =>
    post<CodeRepo>(`/api/log-alerts/code-repos${id ? `?id=${id}` : ''}`, payload),
}

/** 系统与大盘（8.2）。 */
export const systemApi = {
  overview: () => get<Overview>('/api/system/overview'),
  info: () => get<SystemInfo>('/api/system/info'),
  config: () => get<Record<string, unknown>>('/api/system/config'),
}

/** 用户与角色（管理员）。 */
export const userApi = {
  list: (params: PageQuery) => get<PageResult<User>>('/api/users', params),
  create: (payload: Partial<User> & { password: string }) => post<User>('/api/users', payload),
  update: (id: number, payload: Partial<User> & { password?: string }) => put<User>(`/api/users/${id}`, payload),
  remove: (id: number) => del<{ message: string }>(`/api/users/${id}`),
  roles: () =>
    get<{ list: Role[]; permissions: { group: string; items: { code: string; name: string }[] }[] }>('/api/roles'),
  updateRole: (id: number, payload: Partial<Role>) => put<{ message: string }>(`/api/roles/${id}`, payload),
}
