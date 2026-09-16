/**
 * 统一 HTTP 客户端。
 *
 * 约定（设计文档 8.1）：
 *   - 认证：Authorization: Bearer <JWT>，同时兼容 SameSite Cookie；
 *   - 响应：{code, message, data}，code=0 表示成功；
 *   - 401x 自动清理会话并跳转登录，避免在页面里重复处理。
 */
import axios, {
  type AxiosError,
  type AxiosInstance,
  type AxiosRequestConfig,
  type InternalAxiosRequestConfig,
} from 'axios'
import { ElMessage } from 'element-plus'

/** 后端统一响应结构。 */
export interface ApiEnvelope<T> {
  code: number
  message: string
  data: T
  /** 上下文预算触发截断时返回被截断的维度（5.2）。 */
  truncated?: string[]
}

/** 分页响应结构。 */
export interface PageResult<T> {
  list: T[]
  total: number
  page: number
  page_size: number
}

const TOKEN_KEY = 'mwops_token'

/** 读取本地令牌。 */
export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) || ''
}

/** 保存令牌。 */
export function setToken(token: string): void {
  if (token) {
    localStorage.setItem(TOKEN_KEY, token)
  } else {
    localStorage.removeItem(TOKEN_KEY)
  }
}

/** 清理令牌。 */
export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY)
}

const http: AxiosInstance = axios.create({
  baseURL: import.meta.env.VITE_API_BASE || '',
  timeout: 60_000,
  withCredentials: true,
})

http.interceptors.request.use((config: InternalAxiosRequestConfig) => {
  const token = getToken()
  if (token) {
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

/** 登录失效时的回调（由 router 注入，避免循环依赖）。 */
let onUnauthorized: (() => void) | null = null

/** 注册 401 处理器。 */
export function setUnauthorizedHandler(handler: () => void): void {
  onUnauthorized = handler
}

http.interceptors.response.use(
  (response) => response,
  (error: AxiosError<ApiEnvelope<unknown>>) => {
    const status = error.response?.status
    const payload = error.response?.data
    const code = payload?.code ?? 0

    // 认证类错误（401x / HTTP 401）：清理会话并提示重新登录。
    if (status === 401 || (code >= 4010 && code < 4020)) {
      clearToken()
      if (onUnauthorized) {
        onUnauthorized()
      }
      return Promise.reject(new Error(payload?.message || '登录已过期，请重新登录'))
    }
    const message =
      payload?.message ||
      (error.code === 'ECONNABORTED' ? '请求超时，请稍后重试' : error.message) ||
      '请求失败'
    return Promise.reject(new Error(message))
  },
)

/** 解包统一响应的请求方法。 */
export async function request<T>(config: AxiosRequestConfig): Promise<T> {
  const response = await http.request<ApiEnvelope<T>>(config)
  const body = response.data
  if (body && typeof body.code === 'number' && body.code !== 0) {
    throw new Error(body.message || '业务处理失败')
  }
  return body?.data as T
}

/** GET 请求。 */
export function get<T>(url: string, params?: Record<string, unknown>): Promise<T> {
  return request<T>({ method: 'GET', url, params })
}

/** POST 请求。 */
export function post<T>(url: string, data?: unknown): Promise<T> {
  return request<T>({ method: 'POST', url, data })
}

/** PUT 请求。 */
export function put<T>(url: string, data?: unknown): Promise<T> {
  return request<T>({ method: 'PUT', url, data })
}

/** DELETE 请求。 */
export function del<T>(url: string, params?: Record<string, unknown>): Promise<T> {
  return request<T>({ method: 'DELETE', url, params })
}

/** 统一错误提示（表单内联错误请自行处理）。 */
export function toastError(error: unknown): void {
  const message = error instanceof Error ? error.message : String(error)
  ElMessage({ type: 'error', message, grouping: true, duration: 4000 })
}

/** SSE 流式请求所需的认证头。 */
export function authHeaders(): Record<string, string> {
  const token = getToken()
  return token ? { Authorization: `Bearer ${token}` } : {}
}

export default http
