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
  /** 本次查询实际使用的 PromQL 标签匹配串（排障依据）。 */
  selector?: string
  /** 取到数值的指标个数 / 画像中的指标总数。 */
  matched?: number
  total?: number
  /** up{job=...} 的取值；null 表示该 job 在 Prometheus 中不存在。 */
  job_up?: number | null
}

/** 单个指标的接入自检结果。 */
export interface DiagnoseMetric {
  name: string
  display_name: string
  expr: string
  matched: boolean
  status: string
}

/** 实例接入自检结果（回答「为什么这个实例没有指标/日志」）。 */
export interface DiagnoseResult {
  instance_id: number
  instance_name: string
  mw_type: string
  host: string
  port: number
  prom_job: string
  prom_instance: string
  monitor_kind: string
  prometheus_healthy: boolean
  selector: string
  job_up: number | null
  matched: number
  total: number
  source: string
  degraded: boolean
  note: string
  metrics: DiagnoseMetric[]
  hints: string[]
  /** 日志链路（与中间件实例无关）的必备条件清单。 */
  log_checklist: string[]
}

/** 集成中心：单个组件的可配置参数（环境变量或命令行开关）。 */
export interface IntegrationOption {
  key: string
  label: string
  target: 'env' | 'arg'
  kind: 'bool' | 'string' | 'number' | string
  default: string
  help: string
}

/** 集成中心：推荐告警规则。 */
export interface IntegrationAlert {
  name: string
  metric_name: string
  operator: string
  threshold: number
  level: string
  time_window: number
  cooldown: number
  description: string
}

/** 集成中心：组件模板。 */
export interface IntegrationTemplate {
  type: string
  name: string
  component: string
  description: string
  phase: number
  image: string
  exporter_port: number
  default_port: number
  metrics_path: string
  needs_auth: boolean
  /** 平台支持代建只读监控账号时的默认账号名（空表示该组件不需要账号）。 */
  monitor_user: string
  address_label: string
  address_hint: string
  address_is_url: boolean
  url_scheme: string
  url_path: string
  options: IntegrationOption[]
  notes: string[]
  docs: string[]
  alerts: IntegrationAlert[]
  dashboard: { title: string; id: string }
  job_name: string
  default_environment: string
  integrated: number
}

/** 集成中心：概览。 */
export interface IntegrationOverview {
  total: number
  by_type: Record<string, number>
  templates: IntegrationTemplate[]
  file_sd_path: string
  docker_note: string
  docker_ok: boolean
  /** 远程安装（Ansible）是否已开启 / 是否真的可用（镜像内有 ansible-playbook）。 */
  remote_install: boolean
  remote_ready: boolean
}

/** 集成中心：一条集成。 */
export interface IntegrationView {
  instance_id: number
  name: string
  mw_type: string
  component: string
  address: string
  host: string
  port: number
  username: string
  environment: string
  group_name: string
  labels: Record<string, string>
  options: Record<string, string>
  job_name: string
  container: string
  image: string
  container_status: string
  deploy_note: string
  /** 该集成是否勾选了「把目标容器接入平台网络」。 */
  join_platform_network: boolean
  /** 部署位置：local（本机 Docker）| remote（远程 Ansible）。 */
  deploy_target: string
  target_host: string
  exporter_host_port: number
  install_mode: string
  remote_installed_at: string
  selector: string
  applied_at: string
  last_error: string
  /** 下一步该点哪个按钮（后端判定）：reapply=重新应用，retry_account=去重试建号，investigate=看诊断。 */
  next_action?: string
  next_action_label?: string
  has_password: boolean
}

/** 集成中心：端到端自检的一个环节。 */
export interface IntegrationSelfCheckStage {
  key: string
  title: string
  /** ok | warn | fail */
  status: string
  detail: string
  advice?: string
}

/** 集成中心：端到端自检结论（哪一环断了 + 下一步动作）。 */
export interface IntegrationSelfCheck {
  instance_id: number
  name: string
  ok: boolean
  summary: string
  stages: IntegrationSelfCheckStage[]
  next_action?: string
  next_action_label?: string
}

/** 远程集成的账号操作所需 SSH 凭据（仅本次请求使用，平台不落库、不回显）。 */
export interface AccountSecurePayload {
  ssh_user?: string
  ssh_password?: string
  ssh_port?: number
  ssh_key?: string
}

/** 集成中心：平台代管的只读监控账号现状。 */
export interface IntegrationAccount {
  integration_id: number
  name: string
  mw_type: string
  component: string
  username: string
  address: string
  /** 该账号由平台创建（平台会记录在集成元信息里）。 */
  managed: boolean
  /** 平台持有该账号口令（加密存储），因此可自助轮换。 */
  has_password: boolean
  rotated_at: string
  grants: string
  /** 该组件是否支持平台代管账号（Redis 等不需要）。 */
  supports_management: boolean
  last_error: string
  /** 部署位置：remote 时账号操作在目标机上执行，需要 SSH 凭据。 */
  deploy_target: string
  target_host: string
}

/** 集成中心：重试建号/连接的结果。 */
export interface AccountRetryResult {
  /** 本次是否真的执行了建号 SQL。 */
  created: boolean
  /** 用监控账号是否连上了被管实例。 */
  connected: boolean
  /** 总体是否就绪。 */
  ok: boolean
  /** 可读结论（含失败原因与下一步）。 */
  message: string
  output: string
  view: IntegrationView
}

/** 集成中心：轮换口令的结果（新口令只在此响应里出现一次）。 */
export interface AccountRotateResult {
  view: IntegrationView
  new_password: string
}

/** 集成中心：单次连接测试结果。 */
export interface AccountProbeResult {
  ok: boolean
  message: string
  output: string
}

/** 集成中心：生成的采集配置。 */
export interface IntegrationArtifacts {
  job_name: string
  targets: string[]
  labels: Record<string, string>
  file_sd: string
  scrape_job: string
  compose: string
  deploy_cmd: string
  selector: string
  verify_steps: string[]
}

/** 集成中心：新建/更新入参。 */
export interface IntegrationInput {
  name: string
  mw_type: string
  address: string
  username?: string
  password?: string
  labels?: Record<string, string>
  options?: Record<string, string>
  environment?: string
  group_name?: string
  tags?: string[]
  deploy?: boolean
  auto_rules?: boolean
  /** 部署位置与远程安装参数（SSH 凭据不落库）。 */
  deploy_target?: 'local' | 'remote'
  target_host?: string
  exporter_port?: number
  /** 远程安装方式：docker（默认）| docker-systemd（容器交 systemd）| binary（二进制 + 原生 systemd）。 */
  install_mode?: 'docker' | 'docker-systemd' | 'binary'
  ssh_user?: string
  ssh_port?: number
  ssh_password?: string
  ssh_key?: string
  ssh_become?: boolean
  /** 由平台创建/更新只读监控账号（写操作，默认关闭）。 */
  bootstrap_account?: boolean
  /**
   * 反向接网：把**目标容器**接入平台网络（默认关闭）。
   *
   * 正常方向是平台把自己的 Exporter 接进目标网络（不改被管项目）；
   * 勾选后平台会执行等价的 `docker network connect <平台网络> <目标容器>`。
   */
  join_platform_network?: boolean
  /** 被管实例的管理凭据：仅本次请求使用，平台不落库、不写审计、不回显。 */
  admin_username?: string
  admin_password?: string
}

/** 日志接入：入参（平台按目标容器名反查日志位置）。 */
export interface LogCollectInput {
  name: string
  /** 被管容器名（docker ps 里的 NAMES 列）。 */
  target_container: string
  service?: string
  environment?: string
  glob?: string
  level_filter?: string
}

/** 日志接入：发现到的日志位置。 */
export interface LogSource {
  container: string
  networks: string[]
  dir: string
  mount_target: string
  mount_source: string
  mount_kind: 'volume' | 'bind' | string
  mount_spec: string
  glob: string
  /** 判断依据（环境变量名 / 挂载点），供使用者核对。 */
  evidence: string[]
}

/** 日志接入：预览/执行结果。 */
export interface LogCollectPlan {
  name: string
  service: string
  source: LogSource
  collector_name: string
  collector_image: string
  binds: string[]
  networks: string[]
  agent_env: Record<string, string>
  steps: string[]
  discovered_from: string
  target_container: string
  existing?: { status: string; running: boolean } | null
  warnings: string[]
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
  /** 来源告警（修复工单 / 诊断带入，用于审批通过后回填）。 */
  alert_id: number
  /** 来源诊断。 */
  diagnosis_id: number
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
  /** 来源告警。 */
  alert_id: number
  /** 来源诊断。 */
  diagnosis_id: number
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

/** AI 提供方设置（单个提供方）。 */
export interface AIProviderSetting {
  enabled: boolean
  /** openai | anthropic | ollama | mock */
  kind: string
  base_url: string
  /** 只读：后端是否已存密钥。 */
  api_key_set: boolean
  /** 只读：形如 sk-****cdef；后端不返回明文。 */
  api_key_masked: string
  model: string
  max_tokens: number
  price_per_k_token: number
}

/** AI 设置视图（GET /api/settings/ai）。 */
export interface AISettingsView {
  /** third_party | self_hosted | hybrid */
  strategy: string
  third_party: AIProviderSetting
  self_hosted: AIProviderSetting
  daily_token_quota: number
  per_user_daily_quota: number
  updated_by: string
  updated_at: string
  /** 生效来源：platform（平台已保存）| env（仍是 .env 配置）。 */
  source: string
  /** 当前生效的提供方描述（不含密钥）。 */
  providers_active: string[]
}

/** AI 提供方入参（不填 api_key 即保留原有密钥）。 */
export interface AIProviderInput {
  enabled: boolean
  kind: string
  base_url: string
  api_key?: string
  clear_api_key?: boolean
  model: string
  max_tokens: number
  price_per_k_token: number
}

/** AI 设置入参（PUT /api/settings/ai）。 */
export interface AISettingsInput {
  strategy: string
  third_party: AIProviderInput
  self_hosted: AIProviderInput
  daily_token_quota: number
  per_user_daily_quota: number
}

/** token 消费趋势的单个数据点。 */
export interface AIUsagePoint {
  date: string
  tokens: number
  calls: number
}

/** AI 用量与额度视图（GET /api/settings/ai/usage）。 */
export interface AIUsageView {
  today_tokens: number
  today_calls: number
  daily_quota: number
  per_user_daily_quota: number
  /** -1 表示「不限」。 */
  remaining_today: number
  /** 0..1。 */
  used_ratio: number
  window_days: number
  window_tokens: number
  window_calls: number
  series: AIUsagePoint[]
  by_source: { source: string; tokens: number; calls: number }[]
  top_users: { user_id: number; username: string; tokens: number; calls: number }[]
}

/** AI 测试连接结果（POST /api/settings/ai/test、POST /api/settings/ai/test-provider）。 */
export interface AITestResult {
  ok: boolean
  engine: string
  message: string
  latency_ms: number
}

/** 单个提供方自测连接入参（不保存，仅验证当前填写的调用是否正确）。 */
export interface AIProviderTestInput {
  provider: 'third_party' | 'self_hosted'
  enabled: boolean
  kind: string
  base_url: string
  api_key?: string
  model: string
  max_tokens: number
}

/** 通知渠道：webhook 类渠道现状（飞书 / 企微 / 钉钉）。 */
export interface NotifyWebhookChannelView {
  enabled: boolean
  webhook_set: boolean
  webhook_masked: string
  secret_set: boolean
  mentions: string[]
}

/** 通知渠道：邮件渠道现状。 */
export interface NotifyEmailChannelView {
  enabled: boolean
  host: string
  port: number
  username: string
  password_set: boolean
  from: string
  to: string[]
  use_tls: boolean
}

/** 通知渠道设置视图（GET /api/settings/notify）。 */
export interface NotifySettingsView {
  enabled: boolean
  feishu: NotifyWebhookChannelView
  wecom: NotifyWebhookChannelView
  dingtalk: NotifyWebhookChannelView
  email: NotifyEmailChannelView
  /** 卡片确认落地页路径。 */
  card_confirm_path: string
  updated_by: string
  updated_at: string
  /** 生效来源：platform | env。 */
  source: string
}

/** 通知渠道：webhook 类渠道入参（留空 = 不修改，clear_* = 清空）。 */
export interface NotifyWebhookChannelInput {
  enabled: boolean
  webhook?: string
  clear_webhook?: boolean
  secret?: string
  clear_secret?: boolean
  mentions?: string[]
}

/** 通知渠道：邮件渠道入参。 */
export interface NotifyEmailChannelInput {
  enabled: boolean
  host: string
  port: number
  username: string
  password?: string
  clear_password?: boolean
  from: string
  to: string[]
  use_tls: boolean
}

/** 通知渠道设置入参（PUT /api/settings/notify）。 */
export interface NotifySettingsInput {
  enabled: boolean
  feishu: NotifyWebhookChannelInput
  wecom: NotifyWebhookChannelInput
  dingtalk: NotifyWebhookChannelInput
  email: NotifyEmailChannelInput
  card_confirm_path: string
}

/** 通知渠道：发送测试结果（POST /api/settings/notify/test）。 */
export interface NotifyTestResult {
  ok: boolean
  channel: string
  message: string
}
