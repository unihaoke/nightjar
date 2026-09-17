/** 通用格式化工具。 */
import dayjs from 'dayjs'
import relativeTime from 'dayjs/plugin/relativeTime'
import utc from 'dayjs/plugin/utc'
import 'dayjs/locale/zh-cn'

dayjs.extend(relativeTime)
dayjs.extend(utc)
dayjs.locale('zh-cn')

/** 格式化时间（后端统一返回 UTC，前端转换为本地时间展示）。 */
export function formatTime(value?: string | null, template = 'YYYY-MM-DD HH:mm:ss'): string {
  if (!value) {
    return '-'
  }
  const parsed = dayjs.utc(value).local()
  return parsed.isValid() ? parsed.format(template) : '-'
}

/** 相对时间（如「3 分钟前」）。 */
export function fromNow(value?: string | null): string {
  if (!value) {
    return '-'
  }
  const parsed = dayjs.utc(value).local()
  return parsed.isValid() ? parsed.fromNow() : '-'
}

/** 格式化数字：指标值按量级保留小数位。 */
export function formatNumber(value: number | null | undefined, unit = ''): string {
  if (value === null || value === undefined || Number.isNaN(value)) {
    return '-'
  }
  const abs = Math.abs(value)
  let text: string
  if (abs >= 1_000_000) {
    text = `${(value / 1_000_000).toFixed(2)}M`
  } else if (abs >= 10_000) {
    text = `${(value / 1000).toFixed(1)}k`
  } else if (abs >= 100) {
    text = value.toFixed(0)
  } else if (abs >= 1) {
    text = value.toFixed(2)
  } else {
    text = value.toFixed(4)
  }
  return unit ? `${text}${unit}` : text
}

/** 格式化百分比。 */
export function formatPercent(value: number | null | undefined, digits = 1): string {
  if (value === null || value === undefined || Number.isNaN(value)) {
    return '-'
  }
  // 后端比值可能为 0-1 或 0-100，这里统一按需换算由调用方决定。
  return `${value.toFixed(digits)}%`
}

/** 格式化字节数。 */
export function formatBytes(bytes: number): string {
  if (!bytes) {
    return '0 B'
  }
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  return `${(bytes / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`
}

/** 格式化时长（秒 / 毫秒）。 */
export function formatDuration(seconds: number): string {
  if (!seconds || seconds < 0) {
    return '-'
  }
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days > 0) {
    return `${days} 天 ${hours} 小时`
  }
  if (hours > 0) {
    return `${hours} 小时 ${minutes} 分`
  }
  if (minutes > 0) {
    return `${minutes} 分`
  }
  return `${Math.round(seconds)} 秒`
}

/** 中间件类型展示名。 */
export const mwTypeLabels: Record<string, string> = {
  redis: 'Redis',
  kafka: 'Kafka',
  mysql: 'MySQL',
  pg: 'PostgreSQL',
  es: 'Elasticsearch',
  nginx: 'Nginx',
  rabbitmq: 'RabbitMQ',
  // 主机监控（node_exporter）：采集对象是服务器本身
  node: '主机 / Node',
}

/** 中间件类型标签色（仅语义区分，不使用装饰性配色）。 */
export const mwTypeTagType: Record<string, 'primary' | 'success' | 'warning' | 'danger' | 'info'> = {
  redis: 'danger',
  kafka: 'primary',
  mysql: 'warning',
  pg: 'success',
  es: 'info',
  nginx: 'info',
  rabbitmq: 'warning',
}

/** 环境展示名。 */
export const envLabels: Record<string, string> = {
  dev: '开发',
  staging: '预发',
  prod: '生产',
}

/** 环境标签色：生产环境必须醒目（高危操作提示）。 */
export const envTagType: Record<string, 'primary' | 'success' | 'warning' | 'danger' | 'info'> = {
  dev: 'info',
  staging: 'warning',
  prod: 'danger',
}

/** 告警级别展示名。 */
export const alertLevelLabels: Record<string, string> = {
  warning: '警告',
  critical: '严重',
}

/** 告警状态展示名。 */
export const alertStatusLabels: Record<string, string> = {
  active: '待处理',
  acknowledged: '已确认',
  resolved: '已恢复',
}

/** 诊断反馈展示名。 */
export const feedbackLabels: Record<string, string> = {
  useful: '有用',
  useless: '没用',
  adopted: '已采纳',
}

/** 知识库状态展示名。 */
export const knowledgeStatusLabels: Record<string, string> = {
  draft: '草稿',
  published: '已发布',
  deprecated: '已废弃',
}

/** 审批状态展示名。 */
export const approvalStatusLabels: Record<string, string> = {
  pending: '待审批',
  approved: '已通过',
  rejected: '已驳回',
  expired: '已超时',
  executed: '已执行',
  failed: '执行失败',
  success: '执行成功',
  dry_run: '仅预演',
}

/** 操作级别说明（4.6）。 */
export const levelLabels: Record<string, string> = {
  L0: 'L0 只读',
  L1: 'L1 低危',
  L2: 'L2 高危',
}

/** 修复时机展示名。 */
export const horizonLabels: Record<string, string> = {
  immediate: '立即',
  short_term: '短期',
  long_term: '长期',
}

/** 指标状态对应的标签色。 */
export function metricStatusType(status: string): 'success' | 'warning' | 'danger' | 'info' {
  switch (status) {
    case 'ok':
      return 'success'
    case 'warning':
      return 'warning'
    case 'critical':
      return 'danger'
    default:
      return 'info'
  }
}

/** 截断尾部字符串，避免长文本撑破移动端布局。 */
export function ellipsis(value: string, limit = 40): string {
  if (!value) {
    return '-'
  }
  const runes = Array.from(value)
  return runes.length <= limit ? value : `${runes.slice(0, limit).join('')}…`
}

/** 解析 JSON 字符串（容错）。 */
export function safeParse<T>(raw: string | null | undefined, fallback: T): T {
  if (!raw) {
    return fallback
  }
  try {
    return JSON.parse(raw) as T
  } catch {
    return fallback
  }
}

/** 美化 JSON 展示。 */
export function prettyJSON(value: unknown): string {
  if (value === null || value === undefined) {
    return '-'
  }
  try {
    return JSON.stringify(value, null, 2)
  } catch {
    return String(value)
  }
}
