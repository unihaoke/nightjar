/** 会话与权限状态。 */
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { authApi } from '@/api'
import { clearToken, getToken, setToken } from '@/api/http'
import type { DataScope, EngineStatus, Profile, User } from '@/api/types'

const THEME_KEY = 'mwops_theme'

/** 会话 Store：持有用户、权限点、数据权限范围与主题偏好。 */
export const useUserStore = defineStore('user', () => {
  const user = ref<User | null>(null)
  const permissions = ref<string[]>([])
  const levels = ref<string[]>([])
  const dataScope = ref<DataScope>({ environments: null, groups: null, allow_all: false })
  const engine = ref<EngineStatus | null>(null)
  const loaded = ref(false)
  const theme = ref<'light' | 'dark'>((localStorage.getItem(THEME_KEY) as 'light' | 'dark') || 'light')

  /** 是否已登录（本地令牌存在即认为需要拉取会话）。 */
  const isAuthenticated = computed(() => Boolean(getToken()))

  /** 是否具备某权限点。 */
  function can(permission: string): boolean {
    if (!permission) {
      return true
    }
    return permissions.value.includes(permission) || permissions.value.includes('*')
  }

  /** 是否具备任一权限点。 */
  function canAny(items: string[]): boolean {
    return items.some((item) => can(item))
  }

  /** 是否为管理员。 */
  const isAdmin = computed(() => user.value?.role_code === 'admin')

  /** 应用主题。 */
  function applyTheme(next: 'light' | 'dark'): void {
    theme.value = next
    localStorage.setItem(THEME_KEY, next)
    document.documentElement.classList.toggle('dark', next === 'dark')
  }

  /** 切换主题。 */
  function toggleTheme(): void {
    applyTheme(theme.value === 'dark' ? 'light' : 'dark')
  }

  /** 登录。 */
  async function login(username: string, password: string): Promise<void> {
    const session = await authApi.login(username, password)
    setToken(session.token)
    user.value = session.user
    permissions.value = session.permissions || []
    levels.value = session.levels || []
    dataScope.value = {
      environments: session.env_scope,
      groups: session.group_scope,
      allow_all: session.user.role_code === 'admin' && !(session.env_scope || []).length,
    }
    loaded.value = true
  }

  /** 拉取当前会话（刷新页面后恢复状态）。 */
  async function fetchProfile(): Promise<Profile | null> {
    if (!getToken()) {
      loaded.value = true
      return null
    }
    const profile = await authApi.profile()
    user.value = profile.user
    permissions.value = profile.permissions || []
    levels.value = profile.levels || []
    dataScope.value = profile.data_scope
    engine.value = profile.engine
    loaded.value = true
    return profile
  }

  /** 退出登录。 */
  async function logout(): Promise<void> {
    try {
      await authApi.logout()
    } catch {
      // 退出失败不阻塞本地清理。
    }
    reset()
  }

  /** 清理会话状态。 */
  function reset(): void {
    clearToken()
    user.value = null
    permissions.value = []
    levels.value = []
    dataScope.value = { environments: null, groups: null, allow_all: false }
    engine.value = null
    loaded.value = false
  }

  // 初始化主题。
  applyTheme(theme.value)

  return {
    user,
    permissions,
    levels,
    dataScope,
    engine,
    loaded,
    theme,
    isAuthenticated,
    isAdmin,
    can,
    canAny,
    login,
    fetchProfile,
    logout,
    reset,
    applyTheme,
    toggleTheme,
  }
})
