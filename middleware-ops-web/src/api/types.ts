/** 与后端模型对应的类型定义（对齐设计文档 7.1 表结构与 8.2 接口契约）。 */

/** 中间件类型。 */
export type MWType = 'redis' | 'kafka' | 'mysql' | 'pg' | 'es' | 'nginx' | 'rabbitmq'

/** 环境。 */
export type Environment = 'dev' | 'staging' | 'prod'

/** 操作级别。 */
export type OpLevel = 'L0' | 'L1' | 'L2'

/** 用户。 */
export interface User {
  id: number
  username: string
  nickname: string
  email: string
  role_code: string
  env_scope: string[] | null
  group_scope: string[] | null
  status: number
  last_login?: string
}

/** 角色。 */
export interface Role {
  id: number
  code: string
  name: string
  description: string
  permissions: string[] | null
  levels: string[] | null
  builtin: boolean
  user_count?: number
}

/** 登录会话。 */
export interface Session {
  user: User
  token: string
  expires_at: string
  permissions: string[]
  levels: string[]
  env_scope: string[] | null
  group_scope: string[] | null
}

/** 数据权限范围。 */
export interface DataScope {
  environments: string[] | null
  groups: string[] | null
  allow_all: boolean
}

/** 当前用户信息。 */
export interface Profile {
  user: User
  permissions: string[]
  levels: string[]
  data_scope: DataScope
  engine: EngineStatus
}

/** AI 引擎状态。 */
export interface EngineStatus {
  name: string
  available: boolean
  degraded?: boolean
  circuit_open?: boolean
  consecutive_fails?: number
  last_error?: string
}

/** 中间件实例。 */
export interface MiddlewareInstance {
  id: number
  name: string
  mw_type: MWType
  host: string
  port: number
  username: string
  environment: Environment
  group_name: string
  tags: string[] | null
  status: number
  last_message: string
  last_check_at?: string
  config: Record<string, unknown> | null
  prom_job: string
  prom_instance: string
  has_password: boolean
  created_at: string
  updated_at: string
}

/** 实例表单入参。 */
export interface MiddlewareInput {
  name: string
  mw_type: MWType
  host: string
  port: number
  username?: string
  password?: string
  environment?: Environment
  group_name?: string
  tags?: string[]
  config?: Record<string, unknown>
  prom_job?: string
  prom_instance?: string
}

/** 连接测试结果。 */
export interface TestResult {
  success: boolean
  message: string
  latency_ms: number
  endpoint: string
}

/** 指标采样点。 */
export interface MetricSample {
  timestamp: string
  value: number
}

/** 指标。 */
export interface Metric {
  name: string
  display_name: string
  unit: string
  category: string
  latest: number
  status: 'ok' | 'warning' | 'critical' | 'unknown'
  warning_threshold: number
  critical_threshold: number
  series?: MetricSample[]
  expr?: string
}

/** 指标快照。 */
export interface MetricSnapshot {
  instance_id: number
  mw_type: string
  collected_at: string
  metrics: Metric[]
  source: string
  degraded: boolean
  note: string
}

/** 诊断证据。 */
export interface DiagnosisEvidence {
  source: string
  ref: string
  detail: string
  speculative: boolean
}

/** 修复建议。 */
export interface DiagnosisSuggestion {
  action: string
  horizon: 'immediate' | 'short_term' | 'long_term' | string
  risk: string
  level: OpLevel | string
  evidence_refs?: number[]
}

/** 结构化诊断报告（质量护栏强制 schema，5.6）。 */
export interface DiagnosisReport {
  root_cause: string
  confidence: number
  evidence: DiagnosisEvidence[]
  suggestions: DiagnosisSuggestion[]
  impact_scope: string
  pending_confirm: string[]
  speculative: boolean
  low_confidence: boolean
  missing_dimensions?: string[]
  ai_available: boolean
  engine_note?: string
  grounded_ratio: number
}

/** 参考案例（仅作参考，不作为结论，5.7）。 */
export interface KnowledgeReference {
  id: number
  title: string
  mw_type: string
  similarity: number
  excerpt: string
  adopt_count: number
}

/** 诊断元信息。 */
export interface DiagnosisMeta {
  instance_id: number
  instance_name: string
  mw_type: string
  engine_name: string
  engine_status: string
  cache_hit: boolean
  truncated: string[] | null
  missing: string[] | null
  prompt_tokens: number
  scope: string
  data_source: string
}

/** 诊断结果。 */
export interface DiagnosisResponse {
  diagnosis_id: number
  meta: DiagnosisMeta
  report: DiagnosisReport
  references: KnowledgeReference[] | null
  raw?: string
  warnings: string[] | null
  engine_used: string
  cost_tokens: number
  duration_ms: number
}

/** 诊断记录。 */
export interface DiagnosisRecord {
  id: number
  user_id: number
  instance_id: number
  mw_type: string
  user_query: string
  diagnosis_result: string
  report: DiagnosisReport | null
  suggestions: DiagnosisSuggestion[] | null
  feedback: string
  engine_status: string
  cost_tokens: number
  engine_used: string
  duration_ms: number
  cache_hit: boolean
  truncated: string[] | null
  collected_metrics: Record<string, unknown> | null
  created_at: string
}

/** 告警。 */
export interface Alert {
  id: number
  instance_id: number
  rule_id: number
  mw_type: string
  alert_level: 'warning' | 'critical' | string
  alert_message: string
  metric_value: number
  status: 'active' | 'acknowledged' | 'resolved' | string
  count: number
  triggered_at: string
  acked_by: number
  acked_at?: string
  resolved_at?: string
  diagnosis_id: number
  cluster_id: string
  fingerprint: string
}

/** 告警规则。 */
export interface AlertRule {
  id: number
  name: string
  instance_id: number
  mw_type: string
  metric_name: string
  operator: string
  threshold: number
  level: string
  /** 去重窗口（分钟）。列名刻意避开 PostgreSQL 保留字 window，使用 time_window。 */
  time_window: number
  cooldown: number
  notify_channels: string[] | null
  enabled: boolean
  ai_enabled: boolean
  description: string
}

/** 知识条目。 */
export interface KnowledgeEntry {
  id: number
  title: string
  content: string
  mw_type: string
  tags: string[] | null
  source: 'manual' | 'auto' | string
  status: 'draft' | 'published' | 'deprecated' | string
  author_id: number
  adopt_count: number
  use_count: number
  feedback: Record<string, unknown> | null
  diagnosis_id: number
  similarity?: number
  created_at: string
  updated_at: string
}

/** 审批工单。 */
export interface Approval {
  id: number
  ticket_id: string
  applicant_id: number
  approver_id: number
  instance_id: number
  environment: string
  action_type: string
  action_detail: Record<string, unknown> | null
  preview: Record<string, unknown> | null
  reason: string
  status: 'pending' | 'approved' | 'rejected' | 'expired' | 'executed' | 'failed' | string
  comment: string
  expires_at: string
  decided_at?: string
  executed_at?: string
  exec_result: Record<string, unknown> | null
  created_at: string
}

/** 修复记录。 */
export interface FixRecord {
  id: number
  approval_id: number
  ticket_id: string
  user_id: number
  instance_id: number
  action_type: string
  level: string
  command: string
  action_detail: Record<string, unknown> | null
  status: string
  result: string
  dry_run: boolean
  duration_ms: number
  created_at: string
}

/** 审计日志。 */
export interface AuditLog {
  id: number
  user_id: number
  username: string
  instance_id: number
  action_type: string
  action_detail: Record<string, unknown> | null
  result: string
  level: string
  ip_address: string
  user_agent: string
  route: string
  hash_prev: string
  hash_self: string
  created_at: string
}

/** 审计快照。 */
export interface AuditSnapshot {
  id: number
  snapshot_date: string
  last_log_id: number
  log_count: number
  chain_hash: string
  file_path: string
  verified: boolean
  verified_at?: string
}

/** 日志告警事件。 */
export interface LogEvent {
  id: number
  event_id: string
  server_id: number
  service_name: string
  alert_type: string
  error_signature: string
  raw_stacktrace: string
  context_lines: string
  error_count: number
  severity: string
  status: 'pending' | 'analyzing' | 'resolved' | 'ignored' | string
  first_seen_at: string
  last_seen_at: string
  analyzed: boolean
  suppressed: boolean
}

/** 代码分析报告。 */
export interface CodeAnalysis {
  id: number
  event_id: number
  event_key: string
  service_name: string
  located_file: string
  located_line: number
  code_snippet: string
  root_cause: string
  emergency_plan: string
  fix_suggestion: string
  impact_scope: string
  confidence: number
  evidence: Record<string, unknown> | null
  engine_used: string
  engine_status: string
  cost_tokens: number
  outbound_ok: boolean
  created_at: string
}

/** 服务器实例。 */
export interface ServerInstance {
  id: number
  name: string
  ip: string
  hostname: string
  environment: string
  group_name: string
  status: number
  tags: string[] | null
  last_seen_at?: string
}

/** 代码仓库映射。 */
export interface CodeRepo {
  id: number
  service_name: string
  repo_url: string
  branch: string
  local_path: string
  language: string
  allow_third_party: boolean
  last_pull_at?: string
}

/** 大盘总览。 */
export interface Overview {
  instances: { online: number; offline: number; total: number; by_type: Record<string, number> }
  alerts: { by_level: Record<string, number>; trend: { label: string; total: number }[]; active: number }
  log_events: Record<string, number>
  knowledge: Record<string, number>
  diagnosis: {
    total: number
    avg_duration_ms: number
    total_tokens: number
    degraded: number
    feedback: Record<string, number>
    adoption_rate: number
  }
  approvals: { pending: number }
  audit: { by_action: { action_type: string; total: number }[] }
  platform: {
    version: string
    uptime_sec: number
    monitor_source: string
    cache_kind: string
    queue_pending: number
    engine: EngineStatus
    cost: Record<string, unknown>
    cost_tripped: boolean
    notify: { channel: string; enabled: boolean }[]
  }
}

/** 系统信息（能力矩阵与护栏参数）。 */
export interface SystemInfo {
  app: { name: string; version: string; mode: string; started_at: number; now: number }
  ai_engine: {
    strategy: string
    providers: string[]
    notes: string[]
    status: EngineStatus
    tools: string[]
    eval_set_size: number
  }
  infrastructure: {
    database: { host: string; name: string; auto_migrate: boolean }
    redis: { addr: string; cache_kind: string; queue_kind: string }
    prometheus: { base_url: string; source: string }
    vector: { native_pgvector: boolean }
  }
  capability_matrix: {
    mw_type: string
    manage: boolean
    metrics: boolean
    alerts: boolean
    ai_diagnose: boolean
    note: string
  }[]
  guardrail: Record<string, unknown>
  security: Record<string, unknown>
  notify: { channel: string; enabled: boolean }[]
}

/** 修复预览结果。 */
export interface FixPreview {
  instance_id: number
  action_type: string
  level: OpLevel | string
  environment: string
  requires_approval: boolean
  high_risk: boolean
  high_risk_reason: string
  impact: Record<string, unknown>
  normalized_sql?: string
  sql_notes?: string[]
  warnings: string[] | null
}

/** 修复执行结果。 */
export interface FixExecuteResult {
  status: string
  level: string
  ticket_id?: string
  fix_id: number
  result: Record<string, unknown> | null
  message: string
}

/** 通知渠道状态。 */
export interface NotifyChannel {
  channel: string
  enabled: boolean
}
